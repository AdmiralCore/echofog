package udpsock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

// Errors returned before a datagram reaches the kernel.
var (
	// ErrNoAddress means a write had no destination and the socket is not connected.
	ErrNoAddress = errors.New("udpsock: no destination address")
	// ErrAddress means a destination is not a UDP address.
	ErrAddress = errors.New("udpsock: destination is not a UDP address")
	// ErrNoBuffer means a batch message has no room for a datagram.
	ErrNoBuffer = errors.New("udpsock: message has no buffer")
)

const (
	// maxGSOSegments is Linux UDP_MAX_SEGMENTS: one sendmsg carries at most 64 datagrams.
	maxGSOSegments = 64
	// maxTrainBytes bounds one GSO train, so a pool buffer cannot grow to 64 jumbo datagrams.
	maxTrainBytes = 256 << 10
	// minGSOSegment keeps tiny datagrams out of GSO trains. The gain of a train is in system
	// calls, and it appears with QUIC-sized datagrams; a train of sub-16-byte datagrams saves
	// almost nothing and would turn any strict segment-size check into a permanent fallback for
	// a socket that mostly carries full packets.
	minGSOSegment = 16
)

// Options are the local policy and app hooks of one socket (platforms.md §1.1, §2).
type Options struct {
	// Restricted is the udp=restricted flag: no GSO, no recvmmsg and no ECN from the start,
	// one call per datagram. TransportConn then hands the transport a plain view of the
	// socket, so a transport that probes for the fast path stays on single calls.
	Restricted bool

	// Protect receives the socket's file descriptor before the socket binds, connects or sees
	// its first datagram. A VpnService needs it so the core's own traffic does not loop into
	// its tunnel (platforms.md §1.1). An error aborts the socket.
	Protect func(fd uintptr) error
}

// Conn is one UDP socket with batched I/O. It implements net.PacketConn and the methods
// quic-go probes for its fast path: SyscallConn, ReadMsgUDP, WriteMsgUDP, SetReadBuffer,
// SetWriteBuffer and ReadBatch.
//
// One goroutine may read (through ReadFrom, ReadMsgUDP or ReadBatch, one at a time) while any
// number of goroutines write.
type Conn struct {
	c   *net.UDPConn
	pc  *ipv4.PacketConn
	opt Options

	// ecnOn is set at construction: the socket delivers ECN control messages.
	ecnOn bool
	// gso and batchRead say whether the batched calls may still be used. The first refused
	// batched call clears one of them for good (see doc.go).
	gso       atomic.Bool
	batchRead atomic.Bool

	// Seams for the fallback tests; nil means the kernel implementation.
	readKernelFn func(ms []ipv4.Message, flags int) (int, error)
	writeMsgFn   func(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error)
}

var _ net.PacketConn = (*Conn)(nil)

// oobPacketConn is the surface quic-go probes for its UDP fast path
// (third_party/quic-go/sys_conn.go: OOBCapablePacketConn plus ReadBatch). ReadBatch stands in
// for net.Conn, which quic-go accepts when a batch reader is missing.
type oobPacketConn interface {
	net.PacketConn
	SyscallConn() (syscall.RawConn, error)
	SetReadBuffer(int) error
	ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error)
	WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error)
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
}

var _ oobPacketConn = (*Conn)(nil)

// Listen opens an unconnected UDP socket on laddr (nil means every local address).
func Listen(ctx context.Context, network string, laddr *net.UDPAddr, opt Options) (*Conn, error) {
	if network == "" {
		network = "udp"
	}
	address := ":0"
	if laddr != nil {
		address = laddr.String()
	}
	lc := net.ListenConfig{Control: control(opt.Protect)}
	pc, err := lc.ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	u, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("udpsock: %s socket is %T, not a UDP socket", network, pc)
	}
	return newConn(u, opt), nil
}

// Dial opens a UDP socket connected to raddr.
func Dial(ctx context.Context, network string, raddr *net.UDPAddr, opt Options) (*Conn, error) {
	if raddr == nil {
		return nil, ErrNoAddress
	}
	if network == "" {
		network = "udp"
	}
	d := net.Dialer{Control: control(opt.Protect)}
	nc, err := d.DialContext(ctx, network, raddr.String())
	if err != nil {
		return nil, err
	}
	u, ok := nc.(*net.UDPConn)
	if !ok {
		nc.Close()
		return nil, fmt.Errorf("udpsock: %s socket is %T, not a UDP socket", network, nc)
	}
	return newConn(u, opt), nil
}

// Wrap takes over an existing socket. Protect, when set, runs at once; the caller must not have
// used the socket for peer traffic before this call.
func Wrap(c *net.UDPConn, opt Options) (*Conn, error) {
	if c == nil {
		return nil, errors.New("udpsock: nil socket")
	}
	if opt.Protect != nil {
		rc, err := c.SyscallConn()
		if err != nil {
			return nil, err
		}
		var perr error
		if err := rc.Control(func(fd uintptr) { perr = opt.Protect(fd) }); err != nil {
			return nil, err
		}
		if perr != nil {
			return nil, perr
		}
	}
	return newConn(c, opt), nil
}

func control(protect func(fd uintptr) error) func(network, address string, c syscall.RawConn) error {
	if protect == nil {
		return nil
	}
	return func(_, _ string, rc syscall.RawConn) error {
		var perr error
		if err := rc.Control(func(fd uintptr) { perr = protect(fd) }); err != nil {
			return err
		}
		return perr
	}
}

func newConn(c *net.UDPConn, opt Options) *Conn {
	gso, batchRead := platformCapabilities()
	n := &Conn{c: c, pc: ipv4.NewPacketConn(c), opt: opt}
	if opt.Restricted {
		gso, batchRead = false, false
	} else {
		n.ecnOn = enableECN(c)
	}
	n.gso.Store(gso)
	n.batchRead.Store(batchRead)
	return n
}

// TransportConn returns the connection to hand to a transport that turns on kernel offload when
// it sees the OOB methods (quic-go). In full mode it is the Conn itself; in restricted mode it
// is a plain view of the socket, so the transport keeps one call per datagram and no ECN.
func (c *Conn) TransportConn() net.PacketConn {
	if c.opt.Restricted {
		return plainConn{c.c}
	}
	return c
}

// plainConn is the net.PacketConn view of a socket without the OOB methods. quic-go enables its
// fast path for any *net.UDPConn, so restricted mode must hide even the bare socket's methods;
// it keeps SyscallConn and the buffer setters, which the transport uses to set DF and grow the
// kernel buffers without turning GSO, recvmmsg or ECN on.
type plainConn struct{ c *net.UDPConn }

func (p plainConn) ReadFrom(b []byte) (int, net.Addr, error) { return p.c.ReadFrom(b) }
func (p plainConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	return p.c.WriteTo(b, addr)
}
func (p plainConn) Close() error                       { return p.c.Close() }
func (p plainConn) LocalAddr() net.Addr                { return p.c.LocalAddr() }
func (p plainConn) SetDeadline(t time.Time) error      { return p.c.SetDeadline(t) }
func (p plainConn) SetReadDeadline(t time.Time) error  { return p.c.SetReadDeadline(t) }
func (p plainConn) SetWriteDeadline(t time.Time) error { return p.c.SetWriteDeadline(t) }
func (p plainConn) SetReadBuffer(n int) error          { return p.c.SetReadBuffer(n) }
func (p plainConn) SetWriteBuffer(n int) error         { return p.c.SetWriteBuffer(n) }
func (p plainConn) SyscallConn() (syscall.RawConn, error) {
	return p.c.SyscallConn()
}

// ReadFrom reads one datagram without its control messages.
func (c *Conn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.c.ReadFromUDP(p)
	if err != nil {
		return 0, nil, err
	}
	return n, addr, nil
}

// WriteTo writes one datagram without control messages. On a connected socket the destination
// is the peer from Dial, whatever addr says.
func (c *Conn) WriteTo(p []byte, addr net.Addr) (int, error) {
	var ua *net.UDPAddr
	if addr != nil {
		var ok bool
		if ua, ok = addr.(*net.UDPAddr); !ok {
			return 0, ErrAddress
		}
	} else if c.c.RemoteAddr() == nil {
		return 0, ErrNoAddress
	}
	n, _, err := c.writeMsg(p, nil, ua)
	return n, err
}

// ReadMsgUDP reads one datagram with its control messages.
func (c *Conn) ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	if !c.ecnOn {
		n, addr, err = c.c.ReadFromUDP(b)
		return n, 0, 0, addr, err
	}
	return c.c.ReadMsgUDP(b, oob)
}

// WriteMsgUDP writes one datagram, or one GSO train supplied through oob, with its control
// messages. A refused train is split into single writes before the error reaches the caller, and
// the socket never asks for a train again.
func (c *Conn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error) {
	if addr == nil && c.c.RemoteAddr() == nil {
		return 0, 0, ErrNoAddress
	}
	if c.opt.Restricted || !c.ecnOn {
		oob = stripECN(nil, oob)
	}
	seg := gsoSegment(oob)
	if seg <= 0 {
		return c.writeMsg(b, oob, addr)
	}
	if c.opt.Restricted || !c.gso.Load() {
		return c.writeSplit(b, oob, seg, addr)
	}
	n, oobn, err = c.writeMsg(b, oob, addr)
	if err == nil || !batchFailure(err) {
		return n, oobn, err
	}
	c.gso.Store(false)
	return c.writeSplit(b, oob, seg, addr)
}

// ReadBatch reads up to len(ms) datagrams that are already waiting, or one that is not. After
// the first batch read the kernel refuses, the socket reads one datagram per call for good.
func (c *Conn) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	for i := range ms {
		if len(ms[i].Buffers) == 0 || ms[i].Buffers[0] == nil {
			return 0, ErrNoBuffer
		}
	}
	if c.opt.Restricted || !c.batchRead.Load() {
		return c.readOne(ms)
	}
	read := c.readKernel
	if c.readKernelFn != nil {
		read = c.readKernelFn
	}
	n, err := read(ms, flags)
	if err == nil || n > 0 {
		return n, nil
	}
	if !batchFailure(err) {
		return 0, err
	}
	c.batchRead.Store(false)
	return c.readOne(ms)
}

// WriteBatch sends bufs to addr in as few system calls as the kernel allows: one GSO train for
// equal-length datagrams, single writes otherwise. It returns how many datagrams went out. A
// refused train is resent one datagram at a time, and the socket never asks for a train again.
func (c *Conn) WriteBatch(addr *net.UDPAddr, bufs [][]byte) (int, error) {
	if len(bufs) == 0 {
		return 0, nil
	}
	if addr == nil && c.c.RemoteAddr() == nil {
		return 0, ErrNoAddress
	}
	sent := 0
	for sent < len(bufs) {
		chunk := bufs[sent:]
		if len(chunk) > maxGSOSegments {
			chunk = chunk[:maxGSOSegments]
		}
		if seg := segmentable(chunk); seg >= minGSOSegment && !c.opt.Restricted && c.gso.Load() {
			if limit := maxTrainBytes / seg; limit < len(chunk) {
				if limit < 2 {
					limit = 2
				}
				chunk = chunk[:limit]
			}
			err := c.writeGSO(chunk, addr, seg)
			if err == nil {
				sent += len(chunk)
				continue
			}
			if !batchFailure(err) {
				return sent, err
			}
			c.gso.Store(false)
		}
		for _, p := range chunk {
			if _, _, err := c.writeMsg(p, nil, addr); err != nil {
				return sent, err
			}
			sent++
		}
	}
	return sent, nil
}

// segmentable reports the segment size when bufs fit one GSO train: all but the last have that
// size and the last is neither longer (the kernel may shorten the last segment) nor empty (the
// kernel would not send a zero-byte segment, so the datagram would disappear).
func segmentable(bufs [][]byte) int {
	if len(bufs) < 2 {
		return 0
	}
	seg := len(bufs[0])
	if seg == 0 || seg > 65535 {
		return 0
	}
	for _, p := range bufs[:len(bufs)-1] {
		if len(p) != seg {
			return 0
		}
	}
	if last := len(bufs[len(bufs)-1]); last == 0 || last > seg {
		return 0
	}
	return seg
}

func (c *Conn) readOne(ms []ipv4.Message) (int, error) {
	m := &ms[0]
	buf := m.Buffers[0]
	if c.ecnOn {
		n, oobn, flags, addr, err := c.c.ReadMsgUDP(buf, m.OOB)
		if err != nil {
			return 0, err
		}
		m.N, m.NN, m.Flags, m.Addr = n, oobn, flags, addr
		return 1, nil
	}
	n, addr, err := c.c.ReadFromUDP(buf)
	if err != nil {
		return 0, err
	}
	m.N, m.NN, m.Flags, m.Addr = n, 0, 0, addr
	return 1, nil
}

// writeSplit resends the datagrams of a GSO train one by one, without the UDP_SEGMENT message.
func (c *Conn) writeSplit(b, oob []byte, seg int, addr *net.UDPAddr) (int, int, error) {
	clean := dropGSO(nil, oob)
	sent := 0
	for off := 0; off < len(b); off += seg {
		end := off + seg
		if end > len(b) {
			end = len(b)
		}
		if _, _, err := c.writeMsg(b[off:end], clean, addr); err != nil {
			return sent, 0, err
		}
		sent = end
	}
	return sent, len(oob), nil
}

// writeMsg is the one place a datagram reaches the kernel.
func (c *Conn) writeMsg(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if c.writeMsgFn != nil {
		return c.writeMsgFn(b, oob, addr)
	}
	return c.writeRaw(b, oob, addr)
}

// writeRaw writes with control messages; a connected socket takes its destination from the
// kernel and needs no address (a non-nil one would fail with EISCONN).
func (c *Conn) writeRaw(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if c.c.RemoteAddr() != nil {
		if oob == nil {
			n, err := c.c.Write(b)
			return n, 0, err
		}
		return c.c.WriteMsgUDP(b, oob, nil)
	}
	return c.c.WriteMsgUDP(b, oob, addr)
}

// batchFailure tells whether err means the batched call is not coming back. A deadline or a
// closed socket says nothing about the kernel's willingness to batch.
func batchFailure(err error) bool {
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, net.ErrClosed)
}

func (c *Conn) LocalAddr() net.Addr  { return c.c.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }

func (c *Conn) Close() error                          { return c.c.Close() }
func (c *Conn) SyscallConn() (syscall.RawConn, error) { return c.c.SyscallConn() }

func (c *Conn) SetDeadline(t time.Time) error      { return c.c.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.c.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.c.SetWriteDeadline(t) }

func (c *Conn) SetReadBuffer(n int) error  { return c.c.SetReadBuffer(n) }
func (c *Conn) SetWriteBuffer(n int) error { return c.c.SetWriteBuffer(n) }
