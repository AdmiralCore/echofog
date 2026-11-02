package dpi

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

// maxDatagram is the longest datagram the multiplexer keeps whole. A pooled buffer holds the
// datagram in its first maxDatagram bytes and the datagram's control messages (ECN, packet info)
// after them.
const maxDatagram = 4096

var packetPool = sync.Pool{
	New: func() interface{} {
		return new(make([]byte, maxDatagram+OOBBufferSize))
	},
}

var (
	ErrClosed = errors.New("connection closed")
	// ErrSmallBuffer is no longer returned: a datagram longer than the read buffer is truncated.
	ErrSmallBuffer = errors.New("read buffer too small for packet")
)

// stunMagicCookie is the fixed value at bytes 4..8 of every STUN message (RFC 5389).
const stunMagicCookie = 0x2112A442

// MuxedConn shares one UDP socket between QUIC and STUN. A node discovers its public address by
// sending STUN requests from the very socket QUIC listens on, so that the NAT mapping it learns is
// the one its peers will use; the demultiplexer hands each incoming packet to the right consumer.
type MuxedConn struct {
	conn     *net.UDPConn
	stunConn *filteredConn
	quicConn *filteredConn
	closed   atomic.Bool

	// read errors seen by the read loop (its own: no lock), for a rate-limited log line
	readErrs, readErrsRun int
	readErrLogged         time.Time
}

// readErrLogEvery is how often at most a dropped datagram's read error is logged.
const readErrLogEvery = time.Minute

// readFailed tells the read loop what a read error means. Only a closed socket ends the loop.
// Anything else is one bad datagram, which is dropped: on Windows a datagram longer than the buffer
// fails the read with WSAEMSGSIZE, and an ICMP "port unreachable" for an earlier send can surface
// as WSAECONNRESET. Ending the loop there stopped all UDP reception, QUIC and STUN alike, until the
// node restarted, and anybody who can reach the port can send a 5 KB datagram.
func (m *MuxedConn) readFailed(err error) (stop bool) {
	if m.closed.Load() || errors.Is(err, net.ErrClosed) {
		return true
	}
	m.readErrs++
	m.readErrsRun++
	if now := time.Now(); now.Sub(m.readErrLogged) >= readErrLogEvery {
		slog.Warn("UDP read failed, datagram dropped", "err", err, "errors", m.readErrs)
		m.readErrLogged, m.readErrs = now, 0
	}
	if m.readErrsRun%64 == 0 {
		time.Sleep(10 * time.Millisecond) // an error that does not go away must not spin a core
	}
	return false
}

// readOK tells readFailed the socket reads again.
func (m *MuxedConn) readOK() { m.readErrsRun = 0 }

// NewMuxedConn starts demultiplexing conn. Closing the MuxedConn closes conn.
func NewMuxedConn(conn *net.UDPConn) (*MuxedConn, error) {
	const rbSize = 16384

	stunConn, err := newFilteredConn(conn, rbSize)
	if err != nil {
		return nil, err
	}
	quicConn, err := newFilteredConn(conn, rbSize)
	if err != nil {
		stunConn.Close()
		return nil, err
	}

	m := &MuxedConn{conn: conn, stunConn: stunConn, quicConn: quicConn}
	go m.readLoop()
	return m, nil
}

// dispatch hands a datagram read into bufPtr to its consumer, which then owns the buffer.
func (m *MuxedConn) dispatch(bufPtr *[]byte, n, oobn int, addr netip.AddrPort) {
	if isSTUN((*bufPtr)[:n]) {
		m.stunConn.push(bufPtr, n, oobn, addr)
	} else {
		m.quicConn.push(bufPtr, n, oobn, addr)
	}
}

func (m *MuxedConn) Close() error {
	m.closed.Store(true)
	_ = m.stunConn.Close()
	_ = m.quicConn.Close()
	return m.conn.Close()
}

func (m *MuxedConn) GetSTUNConn() net.PacketConn { return m.stunConn }
func (m *MuxedConn) GetQUICConn() net.PacketConn { return m.quicConn }

// isSTUN recognises a STUN message by its structure: the first two bits are zero and the magic
// cookie sits at bytes 4..8. Looking at the first byte alone is not enough: with UDP obfuscation on
// QUIC packets look like random bytes, and a first byte of 0 or 1 would send about one in a hundred
// of them to the STUN side, silently dropping them.
func isSTUN(data []byte) bool {
	return len(data) >= 20 && data[0]&0xC0 == 0 && binary.BigEndian.Uint32(data[4:8]) == stunMagicCookie
}

type filteredConn struct {
	parent       *net.UDPConn
	rb           *RingBuffer
	wait         chan struct{}
	closed       atomic.Bool
	readDeadline atomic.Int64
}

func newFilteredConn(parent *net.UDPConn, rbSize uint64) (*filteredConn, error) {
	rb, err := NewRingBuffer(rbSize)
	if err != nil {
		return nil, err
	}
	return &filteredConn{
		parent: parent,
		rb:     rb,
		wait:   make(chan struct{}, 1),
	}, nil
}

func (f *filteredConn) push(bufPtr *[]byte, n, oobn int, addr netip.AddrPort) {
	if f.closed.Load() {
		packetPool.Put(bufPtr)
		return
	}

	if !f.rb.Push(packet{bufPtr: bufPtr, n: n, oobn: oobn, addr: addr}) {
		packetPool.Put(bufPtr)
		return
	}

	select {
	case f.wait <- struct{}{}:
	default:
	}
}

func (f *filteredConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	pkt, err := f.next()
	if err != nil {
		return 0, nil, err
	}
	// A datagram longer than p is cut short, as the kernel does with a short buffer. An error
	// here would end quic-go's read loop: one oversized datagram from anybody would close the
	// node's QUIC transport.
	n = copy(p, (*pkt.bufPtr)[:pkt.n])
	packetPool.Put(pkt.bufPtr)
	return n, net.UDPAddrFromAddrPort(pkt.addr), nil
}

// ReadBatch hands over up to len(ms) queued datagrams with their control messages, waiting for
// the first one like ReadFrom. Each is copied into the caller's buffers (truncated if longer).
func (f *filteredConn) ReadBatch(ms []ipv4.Message, _ int) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	pkt, err := f.next()
	if err != nil {
		return 0, err
	}
	k := 0
	for {
		buf := *pkt.bufPtr
		m := &ms[k]
		m.N = copy(m.Buffers[0], buf[:pkt.n])
		m.NN = copy(m.OOB, buf[maxDatagram:maxDatagram+pkt.oobn])
		m.Flags = 0
		m.Addr = net.UDPAddrFromAddrPort(pkt.addr)
		packetPool.Put(pkt.bufPtr)
		if k++; k == len(ms) {
			return k, nil
		}
		var ok bool
		if pkt, ok = f.rb.Pop(); !ok {
			return k, nil
		}
	}
}

// ReadMsgUDP reads one queued datagram with its control messages.
func (f *filteredConn) ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	return readMsgViaBatch(f, b, oob)
}

// WriteMsgUDP writes to the shared socket with control messages (GSO, ECN, source address).
func (f *filteredConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error) {
	return f.parent.WriteMsgUDP(b, oob, addr)
}

// next waits for the next queued datagram, the read deadline or Close.
func (f *filteredConn) next() (packet, error) {
	for {
		if f.closed.Load() {
			return packet{}, ErrClosed
		}

		dl := f.readDeadline.Load()
		if dl > 0 && time.Now().UnixNano() > dl {
			return packet{}, os.ErrDeadlineExceeded
		}

		if pkt, ok := f.rb.Pop(); ok {
			return pkt, nil
		}

		timeout := time.Second
		if dl > 0 {
			now := time.Now().UnixNano()
			if dl > now {
				until := time.Duration(dl - now)
				if until < timeout {
					timeout = until
				}
			} else {
				return packet{}, os.ErrDeadlineExceeded
			}
		}

		select {
		case <-f.wait:
		case <-time.After(timeout):
		}
	}
}

func (f *filteredConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return f.parent.WriteTo(p, addr)
}

func (f *filteredConn) Close() error {
	f.closed.Store(true)
	return nil
}

func (f *filteredConn) LocalAddr() net.Addr { return f.parent.LocalAddr() }

func (f *filteredConn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		f.readDeadline.Store(0)
	} else {
		f.readDeadline.Store(t.UnixNano())
	}
	return nil
}
func (f *filteredConn) SetWriteDeadline(t time.Time) error { return nil }
func (f *filteredConn) SetDeadline(t time.Time) error {
	f.SetReadDeadline(t)
	return nil
}

// SyscallConn lets quic-go set the don't-fragment bit on the shared socket. Without it QUIC
// cannot discover the path MTU and stays at 1280-byte packets for good, which on Windows (one
// system call per packet) is what caps throughput.
func (f *filteredConn) SyscallConn() (syscall.RawConn, error) { return f.parent.SyscallConn() }

func (f *filteredConn) SetReadBuffer(bytes int) error  { return f.parent.SetReadBuffer(bytes) }
func (f *filteredConn) SetWriteBuffer(bytes int) error { return f.parent.SetWriteBuffer(bytes) }
