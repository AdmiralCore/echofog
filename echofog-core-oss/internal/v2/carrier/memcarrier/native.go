package memcarrier

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

type frameKind uint8

const (
	frameOpen frameKind = iota
	frameData
	frameFIN
	frameReset
	frameDatagram
	frameClose
)

type frame struct {
	kind frameKind
	id   uint64
	data []byte
	code uint64
}

type nativeLink struct {
	latency *atomic.Int64
	once    sync.Once
	eps     [2]*endpoint
}

func (l *nativeLink) drop() {
	l.once.Do(func() {
		for _, e := range l.eps {
			e.fail(ErrClosed)
		}
	})
}

func newNativePair(cfg config, loss *atomic.Uint64, latency *atomic.Int64) *Pair {
	link := &nativeLink{latency: latency}
	client := newEndpoint(link, true, cfg)
	server := newEndpoint(link, false, cfg)
	client.peer = server
	server.peer = client
	link.eps = [2]*endpoint{client, server}
	go client.sendLoop()
	go server.sendLoop()
	return &Pair{
		Client:  lossyCarrier{Carrier: client, loss: loss},
		Server:  lossyCarrier{Carrier: server, loss: loss},
		loss:    loss,
		latency: latency,
		drop:    link.drop,
	}
}

// endpoint is one side of the in-memory native carrier.
type endpoint struct {
	link   *nativeLink
	peer   *endpoint
	client bool

	cb         [32]byte
	sealed     bool
	dgramQueue int

	mu      sync.Mutex
	cond    *sync.Cond
	streams map[uint64]*nstream
	nextID  uint64
	acceptQ []*nstream
	queue   []frame
	err     error

	acceptCh chan struct{}
	dgrams   chan []byte
	done     chan struct{}
	closeOne sync.Once

	lastRecv atomic.Int64
}

func newEndpoint(link *nativeLink, client bool, cfg config) *endpoint {
	e := &endpoint{
		link:       link,
		client:     client,
		cb:         cfg.cb,
		sealed:     cfg.sealed,
		dgramQueue: cfg.dgramQueue,
		streams:    make(map[uint64]*nstream),
		acceptCh:   make(chan struct{}, 1),
		dgrams:     make(chan []byte, cfg.dgramQueue),
		done:       make(chan struct{}),
	}
	if !client {
		e.nextID = 1
	}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *endpoint) OpenStream(ctx context.Context) (mux.RawStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.err != nil {
		err := e.err
		e.mu.Unlock()
		return nil, err
	}
	id := e.nextID
	e.nextID += 4
	s := newStream(e, id)
	e.streams[id] = s
	e.queue = append(e.queue, frame{kind: frameOpen, id: id})
	e.cond.Broadcast()
	e.mu.Unlock()
	return s, nil
}

func (e *endpoint) AcceptStream(ctx context.Context) (mux.RawStream, error) {
	for {
		e.mu.Lock()
		if len(e.acceptQ) > 0 {
			s := e.acceptQ[0]
			e.acceptQ[0] = nil
			e.acceptQ = e.acceptQ[1:]
			if len(e.acceptQ) > 0 {
				select {
				case e.acceptCh <- struct{}{}:
				default:
				}
			}
			e.mu.Unlock()
			return s, nil
		}
		err := e.err
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		select {
		case <-e.acceptCh:
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (e *endpoint) SendDatagram(d []byte) error {
	if len(d) > mux.MaxDatagram {
		return mux.ErrDatagramTooLarge
	}
	cp := append([]byte(nil), d...)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.queue = append(e.queue, frame{kind: frameDatagram, data: cp})
	e.cond.Broadcast()
	return nil
}

func (e *endpoint) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-e.dgrams:
		return d, nil
	case <-e.done:
		e.mu.Lock()
		err := e.err
		e.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *endpoint) ChannelBinding() [32]byte { return e.cb }
func (e *endpoint) Sealed() bool             { return e.sealed }
func (e *endpoint) IsClient() bool           { return e.client }
func (e *endpoint) Done() <-chan struct{}    { return e.done }

func (e *endpoint) LastReceive() time.Time {
	n := e.lastRecv.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (e *endpoint) Close() error {
	// Deliver the peer's EOF before local state is torn down; Close is idempotent.
	e.peer.deliver(frame{kind: frameClose})
	e.fail(io.EOF)
	return nil
}

func (e *endpoint) fail(err error) {
	e.closeOne.Do(func() {
		e.mu.Lock()
		e.err = err
		for _, s := range e.streams {
			s.mu.Lock()
			s.closed = err
			s.cond.Broadcast()
			s.mu.Unlock()
		}
		close(e.done)
		e.cond.Broadcast()
		e.mu.Unlock()
	})
}

// sendLoop delivers queued frames in order, applying the current latency to every frame.
func (e *endpoint) sendLoop() {
	for {
		e.mu.Lock()
		for len(e.queue) == 0 && e.err == nil {
			e.cond.Wait()
		}
		if e.err != nil {
			e.mu.Unlock()
			return
		}
		f := e.queue[0]
		e.queue[0] = frame{}
		e.queue = e.queue[1:]
		e.mu.Unlock()

		if d := time.Duration(e.link.latency.Load()); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-e.done:
				t.Stop()
				return
			}
		}
		e.peer.deliver(f)
	}
}

func (e *endpoint) deliver(f frame) {
	e.lastRecv.Store(time.Now().UnixNano())
	e.mu.Lock()
	if e.err != nil {
		e.mu.Unlock()
		return
	}
	if f.kind == frameClose {
		e.mu.Unlock()
		e.fail(io.EOF)
		return
	}
	switch f.kind {
	case frameOpen:
		if _, ok := e.streams[f.id]; !ok {
			s := newStream(e, f.id)
			e.streams[f.id] = s
			e.acceptQ = append(e.acceptQ, s)
			select {
			case e.acceptCh <- struct{}{}:
			default:
			}
		}
	case frameData:
		if s := e.streams[f.id]; s != nil {
			s.mu.Lock()
			if !s.readAbandoned {
				s.buf = append(s.buf, f.data...)
			}
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	case frameFIN:
		if s := e.streams[f.id]; s != nil {
			s.mu.Lock()
			s.recvFin = true
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	case frameReset:
		if s := e.streams[f.id]; s != nil {
			s.mu.Lock()
			if s.reset == nil {
				s.reset = &mux.ResetError{Code: f.code, Remote: true}
			}
			s.sendDone = true
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	case frameDatagram:
		d := append([]byte(nil), f.data...)
		select {
		case e.dgrams <- d:
		default: // a full queue drops the datagram, as QUIC does
		}
	}
	e.mu.Unlock()
}

// nstream is one native stream.
type nstream struct {
	e  *endpoint
	id uint64

	mu            sync.Mutex
	cond          *sync.Cond
	buf           []byte
	recvFin       bool
	readAbandoned bool
	sendDone      bool
	reset         *mux.ResetError
	closed        error
	rdl, wdl      time.Time
	rTimer        *time.Timer
}

func newStream(e *endpoint, id uint64) *nstream {
	s := &nstream{e: e, id: id}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *nstream) ID() uint64 { return s.id }

func (s *nstream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if len(s.buf) > 0 {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			if len(s.buf) == 0 {
				s.buf = nil
			}
			return n, nil
		}
		switch {
		case s.reset != nil:
			return 0, s.reset
		case s.recvFin || s.readAbandoned:
			return 0, io.EOF
		case s.closed != nil:
			return 0, s.closed
		}
		if deadlinePassed(s.rdl) {
			return 0, os.ErrDeadlineExceeded
		}
		s.cond.Wait()
	}
}

func (s *nstream) Write(p []byte) (int, error) {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return 0, e.err
	}
	s.mu.Lock()
	switch {
	case s.reset != nil:
		err := s.reset
		s.mu.Unlock()
		return 0, err
	case s.sendDone:
		s.mu.Unlock()
		return 0, mux.ErrWriteClosed
	case deadlinePassed(s.wdl):
		s.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	data := append([]byte(nil), p...)
	s.mu.Unlock()
	e.queue = append(e.queue, frame{kind: frameData, id: s.id, data: data})
	e.cond.Broadcast()
	return len(p), nil
}

func (s *nstream) CloseWrite() error {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	s.mu.Lock()
	if s.sendDone || s.reset != nil {
		s.mu.Unlock()
		return nil
	}
	s.sendDone = true
	s.mu.Unlock()
	e.queue = append(e.queue, frame{kind: frameFIN, id: s.id})
	e.cond.Broadcast()
	return nil
}

func (s *nstream) Close() error {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	s.mu.Lock()
	if !s.sendDone && s.reset == nil {
		s.sendDone = true
		e.queue = append(e.queue, frame{kind: frameFIN, id: s.id})
	}
	s.readAbandoned = true
	s.buf = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	e.cond.Broadcast()
	return nil
}

func (s *nstream) Reset(code uint64) {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return
	}
	s.mu.Lock()
	if s.reset != nil {
		s.mu.Unlock()
		return
	}
	s.reset = &mux.ResetError{Code: code}
	s.sendDone = true
	s.readAbandoned = true
	s.buf = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	e.queue = append(e.queue, frame{kind: frameReset, id: s.id, code: code})
	e.cond.Broadcast()
}

func (s *nstream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rdl = t
	if s.rTimer != nil {
		s.rTimer.Stop()
		s.rTimer = nil
	}
	if !t.IsZero() {
		s.rTimer = time.AfterFunc(time.Until(t), func() {
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		})
	}
	s.cond.Broadcast()
	return nil
}

func (s *nstream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wdl = t
	s.cond.Broadcast()
	return nil
}

func deadlinePassed(t time.Time) bool { return !t.IsZero() && !time.Now().Before(t) }
