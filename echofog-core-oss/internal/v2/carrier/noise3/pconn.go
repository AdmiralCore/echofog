package noise3

import (
	"crypto/sha256"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"
)

// flowAddr is the stable virtual address of one noise3 flow. The inner QUIC stack sees one such
// address per flow, so an outer NAT rebinding does not move its peer; a new flow is a new address
// and therefore a new QUIC path (wire.md §2.5 step 8, §2.6).
type flowAddr struct {
	id uint64
}

func (a flowAddr) Network() string { return "noise3" }
func (a flowAddr) String() string  { return "noise3/" + strconv.FormatUint(a.id, 10) }

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.AddrPort(), true
	default:
		ap, err := netip.ParseAddrPort(a.String())
		return ap, err == nil
	}
}

// errClosed is returned by a closed virtual connection.
var errClosed = net.ErrClosed

type queuedPacket struct {
	data []byte
	addr net.Addr
}

// queueConn is a net.PacketConn backed by a channel. Both sides of noise3 present one to the
// inner QUIC stack: reads come from authenticated inner datagrams, writes go back through the
// L0 flow. A full queue drops, like UDP (mux.Carrier datagrams are best effort).
type queueConn struct {
	local net.Addr
	write func([]byte, net.Addr) (int, error)

	recv chan queuedPacket
	done chan struct{}
	once sync.Once

	mu           sync.Mutex
	readDeadline time.Time
	deadlineCh   chan struct{}
}

func newQueueConn(local net.Addr, write func([]byte, net.Addr) (int, error)) *queueConn {
	return &queueConn{
		local:      local,
		write:      write,
		recv:       make(chan queuedPacket, 1024),
		done:       make(chan struct{}),
		deadlineCh: make(chan struct{}),
	}
}

func (c *queueConn) deliver(data []byte, addr net.Addr) {
	p := queuedPacket{data: append([]byte(nil), data...), addr: addr}
	select {
	case c.recv <- p:
	case <-c.done:
	default:
		// UDP semantics: a full receive queue drops the datagram.
	}
}

func (c *queueConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		deadline := c.readDeadline
		changed := c.deadlineCh
		c.mu.Unlock()

		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}

		select {
		case q := <-c.recv:
			if timer != nil {
				timer.Stop()
			}
			n := copy(p, q.data)
			return n, q.addr, nil
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, errClosed
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		}
	}
}

func (c *queueConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, errClosed
	default:
	}
	return c.write(p, addr)
}

func (c *queueConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *queueConn) LocalAddr() net.Addr { return c.local }

func (c *queueConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *queueConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	close(c.deadlineCh)
	c.deadlineCh = make(chan struct{})
	c.mu.Unlock()
	return nil
}

func (c *queueConn) SetWriteDeadline(time.Time) error { return nil }

// connPacketConn adapts a net.Conn (for example a UDP association behind a proxy) to the
// net.PacketConn quic-go wants. The remote address is fixed by the connection.
type connPacketConn struct {
	net.Conn
	remote net.Addr
}

func (c *connPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.remote, err
}

func (c *connPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.Write(p) }

func (c *connPacketConn) LocalAddr() net.Addr { return c.Conn.LocalAddr() }

// udpPacketConn adapts a connected net.Conn to the net.PacketConn quic-go wants. A connected
// *net.UDPConn cannot use WriteTo ("use of WriteTo with pre-connected connection"), so the
// adapter always uses Read/Write.
func udpPacketConn(c net.Conn) net.PacketConn {
	return &connPacketConn{Conn: c, remote: c.RemoteAddr()}
}
