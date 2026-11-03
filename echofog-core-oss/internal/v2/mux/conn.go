package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Defaults of the byte-stream mux (wire.md §5.2, §10).
const (
	DefaultStreamWindow  = 256 << 10 // per stream
	DefaultConnWindow    = 4 << 20   // per connection
	DefaultMaxStreams    = 256       // open streams per side
	DefaultDatagramQueue = 64
	MaxDatagram          = 1452 // MUX_DATAGRAM payload maximum (registries.md §1)
)

var (
	ErrConnClosed       = errors.New("mux: connection closed")
	ErrTooManyStreams   = errors.New("mux: too many open streams")
	ErrDatagramTooLarge = errors.New("mux: datagram too large")
	ErrWriteClosed      = errors.New("mux: write on closed stream")
	ErrDeadline         = deadlineError{}
	errFlowControl      = errors.New("mux: data beyond flow-control window")
	errUnknownLocalStrm = errors.New("mux: frame for a local stream that was never opened")
	errDataAfterFin     = errors.New("mux: data after FIN")
	errWrongLevel       = errors.New("mux: frame type not valid at connection level")
	errUnknownConnFrame = errors.New("mux: unknown connection-level frame without IGNORABLE")
	errMalformedConnFrm = errors.New("mux: malformed connection-level frame")
)

type deadlineError struct{}

func (deadlineError) Error() string   { return "mux: deadline exceeded" }
func (deadlineError) Timeout() bool   { return true }
func (deadlineError) Temporary() bool { return true }

// ViolationError closes the connection: the peer broke a rule of the mux. Abuse is true only for
// malformed or misused frames of known types (versioning.md §2 rule 2).
type ViolationError struct {
	Err   error
	Abuse bool
}

func (e *ViolationError) Error() string { return "mux: protocol violation: " + e.Err.Error() }
func (e *ViolationError) Unwrap() error { return e.Err }

// ResetError is returned by a stream that was reset.
type ResetError struct {
	Code   uint64
	Remote bool
}

func (e *ResetError) Error() string {
	side := "locally"
	if e.Remote {
		side = "by peer"
	}
	return fmt.Sprintf("mux: stream reset %s, code %d", side, e.Code)
}

// ConnConfig configures a byte-stream mux connection.
type ConnConfig struct {
	// Client: this side opened the carrier; its stream IDs are even (0, 2, 4, …).
	Client bool
	// ChannelBinding of the carrier's TLS session; Sealed if it negotiated a classical group.
	ChannelBinding [32]byte
	Sealed         bool
	StreamWindow   uint64
	ConnWindow     uint64
	MaxStreams     int
	DatagramQueue  int
	// OnViolation, if set, is called once when the peer breaks a mux rule.
	OnViolation func(*ViolationError)
}

func (c *ConnConfig) fill() {
	if c.StreamWindow == 0 {
		c.StreamWindow = DefaultStreamWindow
	}
	if c.ConnWindow == 0 {
		c.ConnWindow = DefaultConnWindow
	}
	if c.MaxStreams == 0 {
		c.MaxStreams = DefaultMaxStreams
	}
	if c.DatagramQueue == 0 {
		c.DatagramQueue = DefaultDatagramQueue
	}
}

// ConnStats counts what the connection dropped or skipped.
type ConnStats struct {
	DatagramsDroppedOut uint64 // send queue full
	DatagramsDroppedIn  uint64 // receive queue full
	StreamsRefused      uint64 // peer exceeded MaxStreams
	FramesSkipped       uint64 // unknown IGNORABLE frames
}

// Conn is the native byte-stream mux (wire.md §5.2, Decision D14) over one carrier byte stream
// (the TLS application data of reality2-tcp or ws2, or the plaintext of a relay stream
// circuit's sealed records). It implements Carrier.
//
// Writes go through one writer goroutine; the reader goroutine never blocks on the carrier's
// write side, so two peers that both write cannot deadlock.
type Conn struct {
	rw  io.ReadWriteCloser
	cfg ConnConfig

	mu      sync.Mutex
	cond    *sync.Cond
	streams map[uint64]*connStream
	// nextLocal is the next ID this side opens; peerNext is one past the highest peer ID seen
	// (in the peer's own numbering: id>>1).
	nextLocal  uint64
	peerNext   uint64
	localOpen  int
	peerOpen   int
	connCredit uint64 // send credit at connection level
	recvLimit  uint64 // connection-level receive limit (bytes)
	recvd      uint64
	consumed   uint64
	advertised uint64 // consumed at the last MUX_MAX_DATA
	ctrlQ      [][]byte
	dataQ      [][]byte
	dgramQ     [][]byte
	writing    bool
	err        error
	stats      ConnStats

	acceptQ  []*connStream
	acceptCh chan struct{}
	dgramIn  chan []byte
	wake     chan struct{}
	done     chan struct{}
	lastRecv atomic.Int64
}

// NewConn starts a mux connection on rw.
func NewConn(rw io.ReadWriteCloser, cfg ConnConfig) *Conn {
	cfg.fill()
	c := &Conn{
		rw:         rw,
		cfg:        cfg,
		streams:    make(map[uint64]*connStream),
		connCredit: cfg.ConnWindow,
		recvLimit:  cfg.ConnWindow,
		acceptCh:   make(chan struct{}, 1),
		dgramIn:    make(chan []byte, cfg.DatagramQueue),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if !cfg.Client {
		c.nextLocal = 1
	}
	c.cond = sync.NewCond(&c.mu)
	c.lastRecv.Store(time.Now().UnixNano())
	go c.readLoop()
	go c.writeLoop()
	return c
}

func (c *Conn) ChannelBinding() [32]byte { return c.cfg.ChannelBinding }
func (c *Conn) Sealed() bool             { return c.cfg.Sealed }
func (c *Conn) IsClient() bool           { return c.cfg.Client }
func (c *Conn) Done() <-chan struct{}    { return c.done }
func (c *Conn) LastReceive() time.Time   { return time.Unix(0, c.lastRecv.Load()) }

// Err returns the error that closed the connection, or nil.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Stats returns a snapshot of the counters.
func (c *Conn) Stats() ConnStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Close closes the connection and every stream.
func (c *Conn) Close() error {
	c.fail(ErrConnClosed)
	return nil
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	close(c.done)
	c.cond.Broadcast()
	c.mu.Unlock()
	var v *ViolationError
	if errors.As(err, &v) && c.cfg.OnViolation != nil {
		c.cfg.OnViolation(v)
	}
	c.rw.Close()
}

func (c *Conn) violation(err error, abuse bool) {
	c.fail(&ViolationError{Err: err, Abuse: abuse})
}

func (c *Conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// isLocal reports whether a stream ID belongs to this side.
func (c *Conn) isLocal(id uint64) bool { return (id&1 == 0) == c.cfg.Client }

// OpenStream opens a new stream. It is announced to the peer by its first MUX_STREAM.
func (c *Conn) OpenStream(ctx context.Context) (RawStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if c.localOpen >= c.cfg.MaxStreams {
		return nil, ErrTooManyStreams
	}
	if c.nextLocal > frame.MaxVarint {
		return nil, ErrTooManyStreams
	}
	s := c.newStream(c.nextLocal)
	c.nextLocal += 2
	c.localOpen++
	// Announce the stream at once with an empty MUX_STREAM, so streams reach the peer in ID
	// order: a receiver treats a peer ID below the highest one it has seen as finished.
	s.announced = true
	c.queueData(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxStream,
		Payload: frame.MuxStream{StreamID: s.id}.Append(nil)}))
	return s, nil
}

// AcceptStream returns the next stream opened by the peer.
func (c *Conn) AcceptStream(ctx context.Context) (RawStream, error) {
	for {
		c.mu.Lock()
		for len(c.acceptQ) > 0 {
			s := c.acceptQ[0]
			c.acceptQ[0] = nil
			c.acceptQ = c.acceptQ[1:]
			if len(c.acceptQ) == 0 {
				c.acceptQ = nil
			}
			if !s.removed || len(s.buf) > 0 || s.recvFin {
				if len(c.acceptQ) > 0 {
					select {
					case c.acceptCh <- struct{}{}:
					default:
					}
				}
				c.mu.Unlock()
				return s, nil
			}
		}
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		select {
		case <-c.acceptCh:
		case <-c.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SendDatagram queues one datagram in a MUX_DATAGRAM frame. A full queue drops it (counted).
func (c *Conn) SendDatagram(d []byte) error {
	if len(d) > MaxDatagram {
		return ErrDatagramTooLarge
	}
	b := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxDatagram, Payload: d})
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	if len(c.dgramQ) >= c.cfg.DatagramQueue {
		c.stats.DatagramsDroppedOut++
		c.mu.Unlock()
		return nil
	}
	c.dgramQ = append(c.dgramQ, b)
	c.mu.Unlock()
	c.signal()
	return nil
}

// ReceiveDatagram returns the next received datagram.
func (c *Conn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-c.dgramIn:
		return d, nil
	case <-c.done:
		return nil, c.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---- writer

const writeBatch = 64 << 10

func (c *Conn) writeLoop() {
	var buf []byte
	for {
		c.mu.Lock()
		for len(c.ctrlQ) == 0 && len(c.dataQ) == 0 && len(c.dgramQ) == 0 && c.err == nil {
			c.mu.Unlock()
			select {
			case <-c.wake:
			case <-c.done:
			}
			c.mu.Lock()
		}
		if c.err != nil {
			c.mu.Unlock()
			return
		}
		buf = buf[:0]
		// Window updates first: they unblock the peer. Then datagrams (latency), then data in
		// FIFO order (data, FIN and MUX_RESET of a stream stay in order).
		for _, b := range c.ctrlQ {
			buf = append(buf, b...)
		}
		c.ctrlQ = c.ctrlQ[:0]
		for _, b := range c.dgramQ {
			buf = append(buf, b...)
		}
		c.dgramQ = c.dgramQ[:0]
		n := 0
		for n < len(c.dataQ) && len(buf) < writeBatch {
			buf = append(buf, c.dataQ[n]...)
			c.dataQ[n] = nil
			n++
		}
		c.dataQ = c.dataQ[n:]
		c.writing = true
		c.mu.Unlock()
		_, err := c.rw.Write(buf)
		c.mu.Lock()
		c.writing = false
		c.cond.Broadcast()
		c.mu.Unlock()
		if err != nil {
			c.fail(fmt.Errorf("mux: carrier write: %w", err))
			return
		}
	}
}

// CloseAfterFlush closes the connection once every queued frame was written to the carrier,
// or after timeout. It lets a last GOAWAY or CLOSE reach the peer.
func (c *Conn) CloseAfterFlush(timeout time.Duration) {
	t := time.AfterFunc(timeout, func() { c.Close() })
	defer t.Stop()
	c.mu.Lock()
	for c.err == nil && (c.writing || len(c.ctrlQ)+len(c.dataQ)+len(c.dgramQ) > 0) {
		c.cond.Wait()
	}
	c.mu.Unlock()
	c.Close()
}

func (c *Conn) queueCtrl(b []byte) {
	c.ctrlQ = append(c.ctrlQ, b)
	c.signal()
}

func (c *Conn) queueData(b []byte) {
	c.dataQ = append(c.dataQ, b)
	c.signal()
}

// ---- reader

func (c *Conn) readLoop() {
	rd := frame.NewReader(c.rw, nil)
	for {
		f, err := rd.ReadFrame()
		if sk := rd.Skipped(); sk > 0 {
			c.mu.Lock()
			c.stats.FramesSkipped = sk
			c.mu.Unlock()
		}
		if err != nil {
			switch {
			case frame.IsUnknown(err):
				// No stream to CLOSE at connection level: the carrier is closed. Not abuse.
				c.violation(errUnknownConnFrame, false)
			case frame.IsTooLarge(err):
				var tl *frame.TooLargeError
				errors.As(err, &tl)
				c.violation(err, tl.Known)
			case err == frame.ErrNonMinimal:
				c.violation(err, false)
			case err == io.EOF:
				c.fail(io.EOF)
			default:
				c.fail(fmt.Errorf("mux: carrier read: %w", err))
			}
			return
		}
		c.lastRecv.Store(time.Now().UnixNano())
		if err := c.handle(f); err != nil {
			return
		}
	}
}

func (c *Conn) handle(f frame.Frame) error {
	switch f.Type {
	case frame.TypePadding:
		return nil
	case frame.TypeMuxStream:
		m, err := frame.ParseMuxStream(f.Payload)
		if err != nil {
			c.violation(err, true)
			return err
		}
		return c.handleData(m)
	case frame.TypeMuxReset:
		m, err := frame.ParseMuxReset(f.Payload)
		if err != nil {
			c.violation(err, true)
			return err
		}
		return c.handleReset(m)
	case frame.TypeMuxWindow:
		m, err := frame.ParseMuxWindow(f.Payload)
		if err != nil {
			c.violation(err, true)
			return err
		}
		c.mu.Lock()
		if s := c.streams[m.StreamID]; s != nil {
			s.credit = addCap(s.credit, m.Increment)
			c.cond.Broadcast()
		} else if c.isLocal(m.StreamID) && m.StreamID >= c.nextLocal {
			c.mu.Unlock()
			c.violation(errUnknownLocalStrm, true)
			return errUnknownLocalStrm
		}
		c.mu.Unlock()
		return nil
	case frame.TypeMuxMaxData:
		inc, err := frame.ParseMuxMaxData(f.Payload)
		if err != nil {
			c.violation(err, true)
			return err
		}
		c.mu.Lock()
		c.connCredit = addCap(c.connCredit, inc)
		c.cond.Broadcast()
		c.mu.Unlock()
		return nil
	case frame.TypeMuxDatagram:
		select {
		case c.dgramIn <- f.Payload:
		default:
			c.mu.Lock()
			c.stats.DatagramsDroppedIn++
			c.mu.Unlock()
		}
		return nil
	}
	// A known session-level type that is not valid at connection level.
	c.violation(fmt.Errorf("%w: %s", errWrongLevel, frame.TypeName(f.Type)), true)
	return errWrongLevel
}

func addCap(a, b uint64) uint64 {
	if a+b < a || a+b > frame.MaxVarint {
		return frame.MaxVarint
	}
	return a + b
}

func (c *Conn) handleData(m frame.MuxStream) error {
	n := uint64(len(m.Data))
	c.mu.Lock()
	if c.recvd+n > c.recvLimit {
		c.mu.Unlock()
		c.violation(errFlowControl, true)
		return errFlowControl
	}
	c.recvd += n
	s := c.streams[m.StreamID]
	if s == nil {
		var err error
		s, err = c.peerStreamLocked(m.StreamID)
		if err != nil {
			c.mu.Unlock()
			c.violation(err, true)
			return err
		}
		if s == nil { // closed, refused or reset: discard and return the connection credit
			c.consumeLocked(n)
			c.mu.Unlock()
			return nil
		}
	}
	if s.recvFin {
		c.mu.Unlock()
		c.violation(errDataAfterFin, true)
		return errDataAfterFin
	}
	if s.recvd+n > s.recvLimit {
		c.mu.Unlock()
		c.violation(errFlowControl, true)
		return errFlowControl
	}
	s.recvd += n
	if s.readAbandoned {
		c.consumeLocked(n)
	} else {
		s.buf = append(s.buf, m.Data...)
	}
	if m.Fin() {
		s.recvFin = true
		c.maybeRemoveLocked(s)
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

// peerStreamLocked finds or creates the stream for an incoming frame with an ID not in the map.
// It returns nil, nil for a stream that is gone (or refused).
func (c *Conn) peerStreamLocked(id uint64) (*connStream, error) {
	if c.isLocal(id) {
		if id >= c.nextLocal {
			return nil, errUnknownLocalStrm
		}
		return nil, nil // a local stream that has finished
	}
	seq := id >> 1
	if seq < c.peerNext {
		return nil, nil // finished, reset or refused
	}
	c.peerNext = seq + 1
	if c.peerOpen >= c.cfg.MaxStreams {
		c.stats.StreamsRefused++
		c.queueData(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxReset,
			Payload: frame.MuxReset{StreamID: id, Code: frame.CodeRefused}.Append(nil)}))
		return nil, nil
	}
	s := c.newStream(id)
	c.peerOpen++
	if len(c.acceptQ) >= 4*c.cfg.MaxStreams { // drop entries for streams already gone
		q := c.acceptQ[:0]
		for _, e := range c.acceptQ {
			if !e.removed {
				q = append(q, e)
			}
		}
		c.acceptQ = q
	}
	c.acceptQ = append(c.acceptQ, s)
	select {
	case c.acceptCh <- struct{}{}:
	default:
	}
	return s, nil
}

func (c *Conn) handleReset(m frame.MuxReset) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streams[m.StreamID]
	if s == nil {
		if c.isLocal(m.StreamID) {
			if m.StreamID >= c.nextLocal {
				c.mu.Unlock()
				c.violation(errUnknownLocalStrm, true)
				c.mu.Lock()
				return errUnknownLocalStrm
			}
			return nil
		}
		// A peer stream reset before any data: it is opened and gone.
		if seq := m.StreamID >> 1; seq >= c.peerNext {
			c.peerNext = seq + 1
		}
		return nil
	}
	if s.reset == nil {
		// Data that arrived before the reset stays readable (so a CLOSE frame sent just before
		// the reset is delivered); then reads fail.
		s.reset = &ResetError{Code: m.Code, Remote: true}
		s.sendDone = true
	}
	c.removeLocked(s)
	c.cond.Broadcast()
	return nil
}

func (c *Conn) consumeLocked(n uint64) {
	c.consumed += n
	if c.consumed-c.advertised >= c.cfg.ConnWindow/2 && c.err == nil {
		inc := c.consumed - c.advertised
		c.advertised = c.consumed
		c.recvLimit += inc
		c.queueCtrl(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxMaxData,
			Payload: frame.AppendVarint(nil, inc)}))
	}
}

func (c *Conn) removeLocked(s *connStream) {
	if s.removed {
		return
	}
	s.removed = true
	delete(c.streams, s.id)
	if c.isLocal(s.id) {
		c.localOpen--
	} else {
		c.peerOpen--
	}
}

// maybeRemoveLocked removes a stream from the table when both directions are finished.
func (c *Conn) maybeRemoveLocked(s *connStream) {
	if s.sendDone && (s.recvFin || s.readAbandoned) {
		c.removeLocked(s)
	}
}

// ---- streams

type connStream struct {
	c  *Conn
	id uint64

	// guarded by c.mu
	announced     bool
	credit        uint64 // send credit
	sendDone      bool   // FIN sent or reset
	recvLimit     uint64
	recvd         uint64
	consumed      uint64
	advertised    uint64
	buf           []byte
	recvFin       bool
	readAbandoned bool
	reset         *ResetError
	removed       bool
	rdl, wdl      time.Time
	rTimer        *time.Timer
	wTimer        *time.Timer
}

func (c *Conn) newStream(id uint64) *connStream {
	s := &connStream{c: c, id: id, credit: c.cfg.StreamWindow, recvLimit: c.cfg.StreamWindow}
	c.streams[id] = s
	return s
}

func (s *connStream) ID() uint64 { return s.id }

func expired(t time.Time) bool { return !t.IsZero() && !time.Now().Before(t) }

func (s *connStream) Read(p []byte) (int, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(s.buf) == 0 && !s.recvFin && s.reset == nil && c.err == nil && !s.readAbandoned {
		if expired(s.rdl) {
			return 0, ErrDeadline
		}
		c.cond.Wait()
	}
	if len(s.buf) > 0 {
		n := copy(p, s.buf)
		s.buf = s.buf[n:]
		if len(s.buf) == 0 {
			s.buf = nil
		}
		s.consumed += uint64(n)
		if !s.recvFin && s.reset == nil && s.consumed-s.advertised >= c.cfg.StreamWindow/2 && c.err == nil {
			inc := s.consumed - s.advertised
			s.advertised = s.consumed
			s.recvLimit += inc
			c.queueCtrl(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxWindow,
				Payload: frame.MuxWindow{StreamID: s.id, Increment: inc}.Append(nil)}))
		}
		c.consumeLocked(uint64(n))
		return n, nil
	}
	switch {
	case s.reset != nil:
		return 0, s.reset
	case s.recvFin:
		return 0, io.EOF
	case s.readAbandoned:
		return 0, io.EOF
	}
	return 0, c.err
}

func (s *connStream) Write(p []byte) (int, error) {
	c := s.c
	written := 0
	for len(p) > 0 {
		c.mu.Lock()
		for (s.credit == 0 || c.connCredit == 0) && s.reset == nil && !s.sendDone && c.err == nil {
			if expired(s.wdl) {
				c.mu.Unlock()
				return written, ErrDeadline
			}
			c.cond.Wait()
		}
		switch {
		case s.reset != nil:
			c.mu.Unlock()
			return written, s.reset
		case s.sendDone:
			c.mu.Unlock()
			return written, ErrWriteClosed
		case c.err != nil:
			err := c.err
			c.mu.Unlock()
			return written, err
		}
		n := uint64(len(p))
		n = min(n, frame.MaxMuxData, s.credit, c.connCredit)
		s.credit -= n
		c.connCredit -= n
		s.announced = true
		c.queueData(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxStream,
			Payload: frame.MuxStream{StreamID: s.id, Data: p[:n]}.Append(nil)}))
		c.mu.Unlock()
		p = p[n:]
		written += int(n)
	}
	return written, nil
}

func (s *connStream) CloseWrite() error {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.sendDone || s.reset != nil {
		return nil
	}
	if c.err != nil {
		return c.err
	}
	s.sendDone = true
	s.announced = true
	c.queueData(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxStream,
		Payload: frame.MuxStream{StreamID: s.id, Flags: frame.MuxFlagFin}.Append(nil)}))
	c.maybeRemoveLocked(s)
	c.cond.Broadcast()
	return nil
}

// Close sends FIN and abandons the receiving direction: buffered and later data is discarded
// and its connection credit returned.
func (s *connStream) Close() error {
	err := s.CloseWrite()
	c := s.c
	c.mu.Lock()
	if !s.readAbandoned {
		s.readAbandoned = true
		c.consumeLocked(uint64(len(s.buf)))
		s.buf = nil
		c.maybeRemoveLocked(s)
		c.cond.Broadcast()
	}
	c.mu.Unlock()
	return err
}

func (s *connStream) Reset(code uint64) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.reset != nil {
		return
	}
	s.reset = &ResetError{Code: code}
	if c.err == nil && !(s.sendDone && s.recvFin) {
		// In the data queue, after the stream's earlier frames (a CLOSE frame precedes it).
		c.queueData(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeMuxReset,
			Payload: frame.MuxReset{StreamID: s.id, Code: code}.Append(nil)}))
	}
	s.sendDone = true
	c.consumeLocked(uint64(len(s.buf)))
	s.buf = nil
	s.readAbandoned = true
	c.removeLocked(s)
	c.cond.Broadcast()
}

func (s *connStream) setDeadline(t time.Time, dl *time.Time, tm **time.Timer) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	*dl = t
	if *tm != nil {
		(*tm).Stop()
		*tm = nil
	}
	if !t.IsZero() {
		*tm = time.AfterFunc(time.Until(t), func() {
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		})
	}
	c.cond.Broadcast()
}

func (s *connStream) SetReadDeadline(t time.Time) error {
	s.setDeadline(t, &s.rdl, &s.rTimer)
	return nil
}

func (s *connStream) SetWriteDeadline(t time.Time) error {
	s.setDeadline(t, &s.wdl, &s.wTimer)
	return nil
}
