package mux

import (
	"context"
	"crypto/hmac"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Guard owns the accepting side of a carrier from its start. Until a session is attached
// (after the handshake or PATH_BIND on stream 0), every stream other than stream 0 is answered
// with CLOSE{protocol-violation} and reset, its data discarded (wire.md §6.2). Afterwards it
// hands accepted streams to the session.
//
// The one exception is armed by Hold: once the responder has answered message 1, the initiator
// may open service streams the moment it writes message 3, while the responder is still checking
// it and has not attached its session. Those streams are queued, bounded in number and in time,
// and handed to the session when it binds; whatever is left when the hold ends is refused as
// before.
type Guard struct {
	c     Carrier
	ch0   chan RawStream
	bound atomic.Pointer[boundPath]
	// Rejected counts streams refused before the session was attached.
	rejected atomic.Uint64

	mu      sync.Mutex
	holding bool
	maxHeld int
	held    []RawStream
	stop    func() bool
}

type boundPath struct {
	s *Session
	p *path
}

// Hold limits. A held stream is never read by the Guard, so the bytes behind it wait in the
// carrier's own flow-control window (DefaultStreamWindow per stream on the byte-stream mux): the
// memory the hold can pin is at most MaxStreams windows, and no stream data is copied.
const (
	DefaultHoldStreams = 16
	DefaultHoldFor     = 10 * time.Second
)

// HoldConfig configures Guard.Hold.
type HoldConfig struct {
	// MaxStreams is the most streams held at once; further ones are refused (default 16).
	MaxStreams int
	// For is how long the hold lasts (default 10 s, the handshake timeout).
	For time.Duration
	// After starts the expiry timer (AD13); nil means time.AfterFunc. It returns a stop function.
	After func(d time.Duration, f func()) (stop func() bool)
}

// NewGuard starts guarding the carrier.
func NewGuard(c Carrier) *Guard {
	g := &Guard{c: c, ch0: make(chan RawStream, 1)}
	go g.loop()
	return g
}

// Hold starts queueing streams that arrive before the session is bound (see Guard). It is a
// no-op when the session is already bound or a hold is running.
func (g *Guard) Hold(cfg HoldConfig) {
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = DefaultHoldStreams
	}
	if cfg.For <= 0 {
		cfg.For = DefaultHoldFor
	}
	if cfg.After == nil {
		cfg.After = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holding || g.bound.Load() != nil {
		return
	}
	g.holding, g.maxHeld = true, cfg.MaxStreams
	g.stop = cfg.After(cfg.For, g.expireHold)
}

// Held returns how many streams wait for the session.
func (g *Guard) Held() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.held)
}

// Carrier returns the guarded carrier.
func (g *Guard) Carrier() Carrier { return g.c }

// Rejected returns how many streams were refused before the session was attached.
func (g *Guard) Rejected() uint64 { return g.rejected.Load() }

// Stream0 returns stream 0 opened by the peer (the carrier client).
func (g *Guard) Stream0(ctx context.Context) (RawStream, error) {
	select {
	case s := <-g.ch0:
		return s, nil
	case <-g.c.Done():
		return nil, ErrConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// bind attaches the session and hands it the streams held so far, in arrival order.
func (g *Guard) bind(s *Session, p *path) {
	b := &boundPath{s, p}
	g.mu.Lock()
	g.bound.Store(b)
	held := g.endHoldLocked()
	g.mu.Unlock()
	for _, raw := range held {
		go s.handleIncoming(p, raw)
	}
}

// endHoldLocked stops the hold and returns the queue.
func (g *Guard) endHoldLocked() []RawStream {
	if g.stop != nil {
		g.stop()
		g.stop = nil
	}
	g.holding = false
	held := g.held
	g.held = nil
	return held
}

// expireHold ends a hold whose session never bound: the queued streams are refused as §6.2 says.
func (g *Guard) expireHold() {
	g.mu.Lock()
	if !g.holding {
		g.mu.Unlock()
		return
	}
	held := g.endHoldLocked()
	g.mu.Unlock()
	for _, raw := range held {
		g.refuse(raw)
	}
}

// dropHeld releases the queue when the carrier is gone: nothing is left to answer.
func (g *Guard) dropHeld() {
	g.mu.Lock()
	held := g.endHoldLocked()
	g.mu.Unlock()
	for _, raw := range held {
		raw.Reset(frame.CodeProtocolViolation)
	}
}

func (g *Guard) refuse(raw RawStream) {
	g.rejected.Add(1)
	go func() {
		raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeClose,
			Payload: frame.Close{Code: frame.CodeProtocolViolation}.Append(nil)}))
		raw.Reset(frame.CodeProtocolViolation)
	}()
}

func (g *Guard) loop() {
	defer g.dropHeld()
	got0 := false
	for {
		raw, err := g.c.AcceptStream(context.Background())
		if err != nil {
			return
		}
		if b := g.bound.Load(); b != nil {
			go b.s.handleIncoming(b.p, raw)
			continue
		}
		if raw.ID() == 0 && !got0 {
			got0 = true
			g.ch0 <- raw
			continue
		}
		g.mu.Lock()
		if b := g.bound.Load(); b != nil {
			g.mu.Unlock()
			go b.s.handleIncoming(b.p, raw)
			continue
		}
		if g.holding && len(g.held) < g.maxHeld {
			g.held = append(g.held, raw)
			g.mu.Unlock()
			continue
		}
		g.mu.Unlock()
		g.refuse(raw)
	}
}

// Binder holds the sessions of a node by session ID and binds new carriers to them.
type Binder struct {
	mu       sync.Mutex
	sessions map[[16]byte]*Session
	// Timeout bounds the wait for the first frame of stream 0 (default 5 s).
	Timeout time.Duration
}

// NewBinder returns an empty Binder.
func NewBinder() *Binder { return &Binder{sessions: map[[16]byte]*Session{}} }

// Add registers a session; it is removed when the session closes.
func (b *Binder) Add(s *Session) {
	b.mu.Lock()
	b.sessions[s.sid] = s
	b.mu.Unlock()
	s.addOnClose(func() {
		b.mu.Lock()
		if b.sessions[s.sid] == s {
			delete(b.sessions, s.sid)
		}
		b.mu.Unlock()
	})
}

// ErrBindRefused: a PATH_BIND did not verify; the carrier was closed silently.
var ErrBindRefused = errors.New("mux: PATH_BIND refused")

// ErrNewSession: stream 0 starts a new handshake; the caller continues with Pending.
var ErrNewSession = errors.New("mux: stream 0 starts a new session")

// Pending is a carrier whose stream 0 began with HS_HELLO: the handshake package continues on
// Stream and then calls NewSession with Guard and Stream.
type Pending struct {
	Guard  *Guard
	Stream RawStream
	First  frame.Frame
}

// Accept reads the first frame of stream 0 of a carrier the peer opened. A valid PATH_BIND
// binds the carrier as a new path of a registered session and answers PATH_BIND_OK. HS_HELLO is
// returned as Pending with ErrNewSession. Anything else, and any PATH_BIND that does not verify,
// closes the carrier without a word (a hidden node reveals nothing, wire.md §8.2).
func (b *Binder) Accept(ctx context.Context, g *Guard, kind PathKind) (*Session, *Pending, error) {
	timeout := b.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := g.Stream0(ctx)
	if err != nil {
		g.c.Close()
		return nil, nil, err
	}
	raw.SetReadDeadline(time.Now().Add(timeout))
	f, err := frame.NewReader(raw, nil).ReadFrame()
	if err != nil {
		g.c.Close()
		return nil, nil, ErrBindRefused
	}
	raw.SetReadDeadline(time.Time{})
	switch f.Type {
	case frame.TypeHsHello:
		return nil, &Pending{Guard: g, Stream: raw, First: f}, ErrNewSession
	case frame.TypePathBind:
	default:
		g.c.Close()
		return nil, nil, ErrBindRefused
	}
	pb, err := frame.ParsePathBind(f.Payload)
	if err != nil {
		g.c.Close()
		return nil, nil, ErrBindRefused
	}
	b.mu.Lock()
	s := b.sessions[pb.SID]
	b.mu.Unlock()
	if s == nil || !s.acceptBind(pb, g.c.ChannelBinding()) {
		g.c.Close()
		return nil, nil, ErrBindRefused
	}
	// PATH_BIND_OK is the last plaintext frame of the new stream 0; in sealed mode the control
	// stream continues as records.
	ok := frame.AppendFrame(nil, frame.Frame{Type: frame.TypePathBindOK, Payload: frame.AppendVarint(nil, pb.PathID)})
	if _, err := s.attachPath(pb.PathID, kind, g, raw, ok, false); err != nil {
		return nil, nil, err
	}
	return s, nil, nil
}

func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }
