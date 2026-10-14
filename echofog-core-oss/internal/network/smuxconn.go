package network

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// Over TCP every smux stream starts with one byte saying what it is.
const (
	smuxKindBidi     byte = 'B'
	smuxKindUni      byte = 'U'
	smuxKindDatagram byte = 'D'
	smuxKindAnnounce byte = 'A' // the dialer's listening port, sent once after authentication
)

const (
	smuxMaxStreams    = 1000 // like QUIC MaxIncomingStreams
	smuxAcceptBacklog = 64
	smuxDatagramQueue = 2048 // ~2 s of a 4K stream
	smuxMaxDatagram   = 64 << 10
	smuxKindTimeout   = 10 * time.Second
)

// Datagrams dropped by TCP connections because a queue was full (diagnostics).
var smuxDroppedOut, smuxDroppedIn atomic.Uint64

// SmuxDatagramDrops reports datagrams dropped on send and on receive across TCP connections.
func SmuxDatagramDrops() (out, in uint64) { return smuxDroppedOut.Load(), smuxDroppedIn.Load() }

func smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2 // per-stream flow control: one slow stream cannot stall the others
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxReceiveBuffer = 8 << 20
	c.MaxStreamBuffer = 1 << 20
	return c
}

// smuxConn is a MuxConn over one byte-stream connection (TLS by the time it reaches here). Datagrams travel on a dedicated stream and
// are dropped, not queued, when either side falls behind, so media keeps UDP-like semantics
// (TCP still adds head-of-line delay on packet loss: this transport is the fallback).
type smuxConn struct {
	sess    *smux.Session
	conn    net.Conn
	binding []byte // channel binding of the underlying TLS session

	ctx    context.Context
	cancel context.CancelFunc

	bidi     chan MuxStream
	uni      chan MuxRecvStream
	dgramIn  chan []byte
	announce chan uint16

	dgramOut     chan []byte
	dgramOutOnce sync.Once
	dgramInMu    sync.Mutex
	dgramInOpen  bool
	closeOnce    sync.Once
}

// NewSmuxConn runs smux over an established (TLS) connection. binding is the TLS channel
// binding returned by the handshake. The dialing side is the client.
func NewSmuxConn(parent context.Context, conn net.Conn, binding []byte, isClient bool) (MuxConn, error) {
	var (
		sess *smux.Session
		err  error
	)
	if isClient {
		sess, err = smux.Client(conn, smuxConfig())
	} else {
		sess, err = smux.Server(conn, smuxConfig())
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	c := &smuxConn{
		sess:     sess,
		conn:     conn,
		binding:  binding,
		ctx:      ctx,
		cancel:   cancel,
		bidi:     make(chan MuxStream, smuxAcceptBacklog),
		uni:      make(chan MuxRecvStream, smuxAcceptBacklog),
		dgramIn:  make(chan []byte, smuxDatagramQueue),
		dgramOut: make(chan []byte, smuxDatagramQueue),
		announce: make(chan uint16, 1),
	}
	go func() {
		select {
		case <-sess.CloseChan():
		case <-ctx.Done():
		}
		c.shutdown()
	}()
	go c.acceptLoop()
	return c, nil
}

func (c *smuxConn) shutdown() {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.sess.Close()
		_ = c.conn.Close()
	})
}

func (c *smuxConn) acceptLoop() {
	for {
		st, err := c.sess.AcceptStream()
		if err != nil {
			c.shutdown()
			return
		}
		if c.sess.NumStreams() > smuxMaxStreams {
			_ = st.Close()
			continue
		}
		go c.classify(st)
	}
}

func (c *smuxConn) classify(st *smux.Stream) {
	var kind [1]byte
	_ = st.SetReadDeadline(time.Now().Add(smuxKindTimeout))
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		_ = st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	switch kind[0] {
	case smuxKindBidi:
		c.deliver(func() bool {
			select {
			case c.bidi <- smuxStream{st}:
				return true
			default:
				return false
			}
		}, st)
	case smuxKindUni:
		c.deliver(func() bool {
			select {
			case c.uni <- smuxStream{st}:
				return true
			default:
				return false
			}
		}, st)
	case smuxKindAnnounce:
		var port [2]byte
		_, err := io.ReadFull(st, port[:])
		_ = st.Close()
		if err == nil {
			select {
			case c.announce <- binary.BigEndian.Uint16(port[:]):
			default: // only the first announcement counts
			}
		}
	case smuxKindDatagram:
		c.dgramInMu.Lock()
		dup := c.dgramInOpen
		c.dgramInOpen = true
		c.dgramInMu.Unlock()
		if dup {
			_ = st.Close()
			return
		}
		c.readDatagrams(st)
	default:
		_ = st.Close()
	}
}

// deliver hands a stream to the application, waiting a little when it is busy.
func (c *smuxConn) deliver(try func() bool, st *smux.Stream) {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !try() {
		select {
		case <-tick.C:
		case <-deadline.C:
			_ = st.Close()
			return
		case <-c.ctx.Done():
			_ = st.Close()
			return
		}
	}
}

func (c *smuxConn) readDatagrams(st *smux.Stream) {
	defer st.Close()
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(st, hdr[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(hdr[:]))
		buf := make([]byte, n)
		if _, err := io.ReadFull(st, buf); err != nil {
			return
		}
		select {
		case c.dgramIn <- buf:
		default: // the application is behind: drop, as a congested network would
			smuxDroppedIn.Add(1)
		}
	}
}

func (c *smuxConn) writeDatagrams() {
	st, err := c.sess.OpenStream()
	if err != nil {
		return
	}
	defer st.Close()
	if _, err := st.Write([]byte{smuxKindDatagram}); err != nil {
		return
	}
	// coalesce whatever is queued into one write: one smux frame and one TLS record for many
	// datagrams instead of one each
	buf := make([]byte, 0, smuxMaxDatagram+2)
	appendFrame := func(b []byte) {
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(b)))
		buf = append(buf, b...)
	}
	for {
		select {
		case b := <-c.dgramOut:
			buf = buf[:0]
			appendFrame(b)
		drain:
			for len(buf) < smuxMaxDatagram {
				select {
				case b := <-c.dgramOut:
					appendFrame(b)
				default:
					break drain
				}
			}
			if _, err := st.Write(buf); err != nil {
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *smuxConn) open(kind byte) (*smux.Stream, error) {
	if c.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	st, err := c.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if _, err := st.Write([]byte{kind}); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// announcePort tells the remote which port this node accepts connections on.
func (c *smuxConn) announcePort(port uint16) error {
	st, err := c.open(smuxKindAnnounce)
	if err != nil {
		return err
	}
	defer st.Close()
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], port)
	_, err = st.Write(b[:])
	return err
}

// awaitAnnounce waits for the remote's listening port.
func (c *smuxConn) awaitAnnounce(ctx context.Context) (uint16, bool) {
	select {
	case p := <-c.announce:
		return p, p != 0
	case <-ctx.Done():
		return 0, false
	case <-c.ctx.Done():
		return 0, false
	}
}

func (c *smuxConn) OpenStreamSync(ctx context.Context) (MuxStream, error) {
	st, err := c.open(smuxKindBidi)
	if err != nil {
		return nil, err
	}
	// the start of a bidirectional stream is padded to hide TLS-in-TLS when the proxy carries a
	// user's HTTPS; the kind byte above went out raw, so both ends begin the padded framing here
	return newVisionStream(smuxStream{st}), nil
}

func (c *smuxConn) OpenUniStreamSync(ctx context.Context) (MuxSendStream, error) {
	st, err := c.open(smuxKindUni)
	if err != nil {
		return nil, err
	}
	return smuxStream{st}, nil
}

func (c *smuxConn) AcceptStream(ctx context.Context) (MuxStream, error) {
	select {
	case st := <-c.bidi:
		return newVisionStream(st), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (c *smuxConn) AcceptUniStream(ctx context.Context) (MuxRecvStream, error) {
	select {
	case st := <-c.uni:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (c *smuxConn) SendDatagram(b []byte) error {
	if len(b) > smuxMaxDatagram-1 {
		return &DatagramTooLargeError{Size: len(b), Max: smuxMaxDatagram - 1}
	}
	if c.ctx.Err() != nil {
		return net.ErrClosed
	}
	c.dgramOutOnce.Do(func() { go c.writeDatagrams() })
	cp := append([]byte(nil), b...)
	select {
	case c.dgramOut <- cp:
	default: // queue full: drop like UDP
		smuxDroppedOut.Add(1)
	}
	return nil
}

func (c *smuxConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case b := <-c.dgramIn:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (c *smuxConn) LocalAddr() net.Addr      { return c.conn.LocalAddr() }
func (c *smuxConn) RemoteAddr() net.Addr     { return c.conn.RemoteAddr() }
func (c *smuxConn) Context() context.Context { return c.ctx }
func (c *smuxConn) Protocol() string         { return ProtocolTCP }

func (c *smuxConn) CloseWithError(_ QuicErrorCode, _ string) error {
	c.shutdown()
	return nil
}

func (c *smuxConn) ChannelBinding() ([]byte, error) {
	if len(c.binding) == 0 {
		return nil, errors.New("no channel binding for this connection")
	}
	return c.binding, nil
}

type smuxStream struct{ *smux.Stream }

func (s smuxStream) CancelRead() { _ = s.Stream.Close() }
