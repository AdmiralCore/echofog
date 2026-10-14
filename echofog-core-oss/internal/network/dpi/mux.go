package dpi

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var packetPool = sync.Pool{
	New: func() interface{} {
		return new(make([]byte, 4096))
	},
}

var (
	ErrClosed      = errors.New("connection closed")
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
}

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

func (m *MuxedConn) readLoop() {
	for {
		bufPtr := packetPool.Get().(*[]byte)
		buf := *bufPtr

		n, addrPort, err := m.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			packetPool.Put(bufPtr)
			return
		}

		if isSTUN(buf[:n]) {
			m.stunConn.push(bufPtr, n, addrPort)
		} else {
			m.quicConn.push(bufPtr, n, addrPort)
		}
	}
}

func (m *MuxedConn) Close() error {
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

func (f *filteredConn) push(bufPtr *[]byte, n int, addr netip.AddrPort) {
	if f.closed.Load() {
		packetPool.Put(bufPtr)
		return
	}

	if !f.rb.Push(packet{bufPtr: bufPtr, n: n, addr: addr}) {
		packetPool.Put(bufPtr)
		return
	}

	select {
	case f.wait <- struct{}{}:
	default:
	}
}

func (f *filteredConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	for {
		if f.closed.Load() {
			return 0, nil, ErrClosed
		}

		dl := f.readDeadline.Load()
		if dl > 0 && time.Now().UnixNano() > dl {
			return 0, nil, os.ErrDeadlineExceeded
		}

		pkt, ok := f.rb.Pop()
		if ok {
			data := (*pkt.bufPtr)[:pkt.n]
			if len(p) < len(data) {
				packetPool.Put(pkt.bufPtr)
				return 0, nil, ErrSmallBuffer
			}
			n = copy(p, data)

			packetPool.Put(pkt.bufPtr)

			return n, net.UDPAddrFromAddrPort(pkt.addr), nil
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
				return 0, nil, os.ErrDeadlineExceeded
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

func (f *filteredConn) SetReadBuffer(bytes int) error  { return f.parent.SetReadBuffer(bytes) }
func (f *filteredConn) SetWriteBuffer(bytes int) error { return f.parent.SetWriteBuffer(bytes) }
