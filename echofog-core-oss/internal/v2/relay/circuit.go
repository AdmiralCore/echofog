package relay

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Pipe is the byte pipe of one circuit stream (a detached relay/2 stream).
type Pipe interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

var errNoStream = errors.New("relay: circuit carrier has no such stream")

// Circuit is a stream circuit (services.md §6.2) presented to the session layer as a
// mux.Carrier of kind relay. Its bytes are records (wire.md §8.4): before the handshake
// completes they are kind-0 records that carry the HS_* frames (or PATH_BIND and PATH_BIND_OK on
// an added path); the session layer then calls StartSealing once, and everything after is kind-1
// sealed records whose plaintext is a connection-level mux (mux.Conn).
//
// R never sees any of this: it copies bytes.
type Circuit struct {
	pipe   Pipe
	rc     *mux.RecordConn
	cb     [32]byte
	client bool
	cid    uint64

	peerAddr netip.AddrPort // the peer's address R reported (PEER_ADDR), if both ends asked for it

	mu      sync.Mutex
	conn    *mux.Conn
	ready   chan struct{} // closed when conn exists
	st0     mux.RawStream // the control stream (stream 0) of the sealed mux
	st0Cond chan struct{} // closed when st0 is set
	opened  bool          // the plain stream 0 was handed out
	dgrams  *DatagramCircuit
	done    chan struct{}
	closeMu sync.Once
	started time.Time
}

// NewCircuit wraps the pipe of a circuit. client is true on the side that sent CONNECT (the
// session initiator, as A opens the first path); cb is the circuit channel binding
// (handshake.CircuitBinding).
func NewCircuit(p Pipe, cb [32]byte, cid uint64, client bool) *Circuit {
	return &Circuit{pipe: p, rc: mux.NewRecordConn(p), cb: cb, client: client, cid: cid,
		ready: make(chan struct{}), st0Cond: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
}

// PeerAddr is the peer's public address as R observed it (PEER_ADDR, services.md §6.4 step 1). It is
// invalid unless both ends agreed to it.
func (c *Circuit) PeerAddr() netip.AddrPort { return netip.AddrPort{} }

// CID is the circuit ID R assigned (the same value at both ends).
func (c *Circuit) CID() uint64 { return c.cid }

// ChannelBinding implements mux.Carrier.
func (c *Circuit) ChannelBinding() [32]byte { return c.cb }

// Sealed implements mux.Carrier: relay paths are always sealed.
func (c *Circuit) Sealed() bool { return true }

// IsClient implements mux.Carrier.
func (c *Circuit) IsClient() bool { return c.client }

// Done implements mux.Carrier.
func (c *Circuit) Done() <-chan struct{} { return c.done }

// LastReceive implements mux.Carrier.
func (c *Circuit) LastReceive() time.Time {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		return conn.LastReceive()
	}
	return c.started
}

// Close implements mux.Carrier.
func (c *Circuit) Close() error {
	c.closeMu.Do(func() { close(c.done) })
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return c.pipe.Close()
}

// StartSealing is called once by the session layer after the last handshake frame (after
// PATH_BIND_OK on an added path): the records are sealed from here on, and the mux starts.
func (c *Circuit) StartSealing(s *mux.Sealer, pathID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return
	}
	c.rc.StartSealing(s, pathID, 0)
	c.conn = mux.NewConn(c.rc, mux.ConnConfig{Client: c.client, ChannelBinding: c.cb})
	close(c.ready)
	go func() {
		select {
		case <-c.conn.Done():
			c.closeMu.Do(func() { close(c.done) })
		case <-c.done:
		}
	}()
	if c.client {
		// The initiator's mux stream 0 is the control stream the handshake already used.
		if st, err := c.conn.OpenStream(context.Background()); err == nil {
			c.st0 = st
			close(c.st0Cond)
		}
	}
}

func (c *Circuit) waitConn(ctx context.Context) (*mux.Conn, error) {
	select {
	case <-c.ready:
		return c.conn, nil
	case <-c.done:
		return nil, mux.ErrConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// OpenStream implements mux.Carrier. The first call before sealing returns stream 0 in its
// plain-record phase; afterwards it opens streams of the sealed mux.
func (c *Circuit) OpenStream(ctx context.Context) (mux.RawStream, error) {
	c.mu.Lock()
	if !c.opened && c.client {
		c.opened = true
		c.mu.Unlock()
		return &circuitStream{c: c}, nil
	}
	c.mu.Unlock()
	conn, err := c.waitConn(ctx)
	if err != nil {
		return nil, err
	}
	return conn.OpenStream(ctx)
}

// AcceptStream implements mux.Carrier. The responder gets stream 0 (plain phase) at once; after
// sealing, it returns the streams the peer opens (stream 0 stays the control stream).
func (c *Circuit) AcceptStream(ctx context.Context) (mux.RawStream, error) {
	c.mu.Lock()
	if !c.opened && !c.client {
		c.opened = true
		c.mu.Unlock()
		return &circuitStream{c: c}, nil
	}
	c.mu.Unlock()
	conn, err := c.waitConn(ctx)
	if err != nil {
		return nil, err
	}
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			return nil, err
		}
		if !c.client && st.ID() == 0 {
			c.mu.Lock()
			if c.st0 == nil {
				c.st0 = st
				close(c.st0Cond)
			}
			c.mu.Unlock()
			continue
		}
		return st, nil
	}
}

// AttachDatagrams routes the session's datagrams over a datagram circuit to the same peer
// instead of the mux inside the stream circuit (wire.md §8.4: datagram circuits).
func (c *Circuit) AttachDatagrams(d *DatagramCircuit) {
	c.mu.Lock()
	c.dgrams = d
	c.mu.Unlock()
}

// SendDatagram implements mux.Carrier.
func (c *Circuit) SendDatagram(d []byte) error {
	c.mu.Lock()
	dg, conn := c.dgrams, c.conn
	c.mu.Unlock()
	if dg != nil {
		return dg.Send(d)
	}
	if conn == nil {
		return errNoStream
	}
	return conn.SendDatagram(d)
}

// ReceiveDatagram implements mux.Carrier.
func (c *Circuit) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	dg := c.dgrams
	c.mu.Unlock()
	if dg != nil {
		return dg.Receive(ctx, c.done)
	}
	conn, err := c.waitConn(ctx)
	if err != nil {
		return nil, err
	}
	return conn.ReceiveDatagram(ctx)
}

// circuitStream is stream 0 of a circuit: kind-0 records until sealing starts, then stream 0 of
// the sealed mux.
type circuitStream struct {
	c *Circuit

	rmu  sync.Mutex
	rbuf []byte
	rdl  time.Time
	wdl  time.Time
}

func (s *circuitStream) ID() uint64 { return 0 }

func (s *circuitStream) sealedStream(ctx context.Context) (mux.RawStream, bool) {
	select {
	case <-s.c.ready:
	default:
		return nil, false
	}
	select {
	case <-s.c.st0Cond:
	case <-s.c.done:
		return nil, true
	case <-ctx.Done():
		return nil, true
	}
	return s.c.st0, true
}

func (s *circuitStream) Read(p []byte) (int, error) {
	if st, sealed := s.sealedStream(context.Background()); sealed {
		if st == nil {
			return 0, mux.ErrConnClosed
		}
		return st.Read(p)
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if len(s.rbuf) == 0 {
		_ = s.c.pipe.SetReadDeadline(s.rdl)
		body, err := s.c.rc.ReadPlain()
		if err != nil {
			return 0, err
		}
		s.rbuf = body
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

func (s *circuitStream) Write(p []byte) (int, error) {
	if st, sealed := s.sealedStream(context.Background()); sealed {
		if st == nil {
			return 0, mux.ErrConnClosed
		}
		return st.Write(p)
	}
	_ = s.c.pipe.SetWriteDeadline(s.wdl)
	written := 0
	for len(p) > 0 {
		n := min(len(p), 16384)
		if err := s.c.rc.WritePlain(p[:n]); err != nil {
			return written, err
		}
		p = p[n:]
		written += n
	}
	return written, nil
}

func (s *circuitStream) CloseWrite() error {
	if st, sealed := s.sealedStream(context.Background()); sealed && st != nil {
		return st.CloseWrite()
	}
	return nil
}

func (s *circuitStream) Close() error {
	if st, sealed := s.sealedStream(context.Background()); sealed && st != nil {
		return st.Close()
	}
	return s.c.Close()
}

func (s *circuitStream) Reset(code uint64) {
	if st, sealed := s.sealedStream(context.Background()); sealed && st != nil {
		st.Reset(code)
		return
	}
	_ = s.c.Close()
}

func (s *circuitStream) SetReadDeadline(t time.Time) error {
	s.rdl = t
	if st, sealed := s.sealedStream(context.Background()); sealed && st != nil {
		return st.SetReadDeadline(t)
	}
	return s.c.pipe.SetReadDeadline(t)
}

func (s *circuitStream) SetWriteDeadline(t time.Time) error {
	s.wdl = t
	if st, sealed := s.sealedStream(context.Background()); sealed && st != nil {
		return st.SetWriteDeadline(t)
	}
	return s.c.pipe.SetWriteDeadline(t)
}

var _ mux.Carrier = (*Circuit)(nil)
