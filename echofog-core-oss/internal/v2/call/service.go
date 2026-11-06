package call

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

const (
	// DefaultRequestTimeout is the timeout of a request-like signal (services.md 9.1).
	DefaultRequestTimeout = 2 * time.Second

	// MaxDatagramSignal is the largest SIGNAL wrapper this build duplicates on CALL_SIGNAL. A
	// datagram must fit the 1232-byte floor every path carries (wire.md 6.5); a larger lossy
	// signal still travels on the reliable stream.
	MaxDatagramSignal = 1200

	// dedupeTTL and dedupeMax bound the memory of seen (sender, msg_id) pairs. The same signal
	// may arrive twice: on the stream and on CALL_SIGNAL.
	dedupeTTL = 30 * time.Second
	dedupeMax = 4096
)

// Peer is the authenticated peer of a call stream, in the types this package needs. The node
// adapts node.Peer to it.
type Peer struct {
	Node   [32]byte
	Device [32]byte
	SID    [16]byte
	Path   mux.PathKind
}

// Stream is the part of *mux.Stream the signalling service uses. The node passes a *mux.Stream;
// tests use an in-memory pipe.
type Stream interface {
	WriteFrame(typ uint64, flags byte, payload []byte) error
	ReadFrame() (frame.Frame, error)
	SetLimiter(frame.Limiter)
	Close() error
	CloseWithCode(code uint64, reason string) error
}

// Datagram is one CALL_SIGNAL datagram with the authenticated peer of its session.
type Datagram struct {
	Peer    Peer
	Type    uint64
	Payload []byte
}

// DatagramPort is the node's datagram view for the call service (registries.md 2).
type DatagramPort interface {
	Send(ctx context.Context, peer [32]byte, typ uint64, payload []byte) error
	Receive(ctx context.Context) (Datagram, error)
	Close() error
}

// Config wires one call/2 signalling service.
type Config struct {
	// Origin is the local device akey: the originator key of forwarded signals and the source of
	// the sender device ID. Required, Ed25519.
	Origin keys.AKey
	// Sign signs with the local device key. Required.
	Sign func([]byte) ([]byte, error)
	// Open opens (or reuses) a call/2 stream to a peer. Required for sending.
	Open func(ctx context.Context, device [32]byte) (Peer, Stream, error)
	// Datagrams carries the CALL_SIGNAL duplicates of lossy signals. Optional.
	Datagrams DatagramPort
	// Context ends the service; nil means Background.
	Context context.Context
	// Now is the corrected clock (AD13). Nil means time.Now.
	Now func() time.Time
	// After is the timer source of request timeouts (AD13). Nil means time.After.
	After func(time.Duration) <-chan time.Time
	// RequestTimeout is the timeout of one request-like signal. Zero means the 2 s default.
	RequestTimeout time.Duration
	// Rand returns message IDs; nil means math/rand/v2.
	Rand func() uint64
	// OnSignal receives every verified, non-duplicate signal. A signal that answers a Request
	// goes to the waiting Request instead. It runs on the stream's goroutine and must not block.
	OnSignal func(ctx context.Context, p Peer, m *pb.CallSignal, raw []byte)
	// Log receives diagnostics. Nil is silent.
	Log *slog.Logger
}

// Service serves and drives call/2 signalling.
type Service struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	req    atomic.Uint64

	mu     sync.Mutex
	links  map[[32]byte]*link
	closed bool

	seenMu    sync.Mutex
	seen      map[dedupeKey]time.Time
	seenOrder []dedupeEntry // insertion order: the oldest first
}

type dedupeEntry struct {
	key dedupeKey
	at  time.Time
}

type dedupeKey struct {
	sender [32]byte
	msgID  uint64
}

type link struct {
	device [32]byte

	mu      sync.Mutex
	peer    Peer
	stream  Stream
	pending map[uint64]chan *pb.CallSignal

	openMu sync.Mutex
}

// New returns a call signalling service.
func New(cfg Config) (*Service, error) {
	if len(cfg.Origin) == 0 {
		return nil, fmt.Errorf("%w: no origin key", ErrBadSignature)
	}
	if _, err := keys.ParseKind(cfg.Origin, keys.Ed25519); err != nil {
		return nil, fmt.Errorf("%w: origin: %v", ErrBadSignature, err)
	}
	if err := keys.CheckEd25519Public(cfg.Origin.Key()); err != nil {
		return nil, fmt.Errorf("%w: origin: %v", ErrBadSignature, err)
	}
	if cfg.Sign == nil {
		return nil, ErrNoSigner
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Uint64
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	parent := cfg.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Service{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		links:  map[[32]byte]*link{},
		seen:   map[dedupeKey]time.Time{},
	}, nil
}

// Handle serves one incoming call/2 stream. It owns the stream and ends it.
func (s *Service) Handle(ctx context.Context, p Peer, st Stream) {
	s.serve(ctx, p, st)
}

func (s *Service) serve(ctx context.Context, p Peer, st Stream) {
	if spec, ok := service.Builtin(service.Call); ok {
		st.SetLimiter(spec.Versions[Version].Limiter())
	}
	l := s.link(p.Device)
	l.attach(p, st)
	defer func() {
		l.detach(st)
		_ = st.Close()
	}()

	for {
		f, err := st.ReadFrame()
		if err != nil {
			return
		}
		if f.Type != TypeSignal {
			_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
			return
		}
		err = s.Process(ctx, p, f.Payload)
		switch {
		case err == nil, errors.Is(err, ErrDuplicate),
			errors.Is(err, ErrBadSignature), errors.Is(err, ErrMissingSignature),
			errors.Is(err, ErrUnexpectedSignature), errors.Is(err, ErrSpoofedSender),
			errors.Is(err, ErrUnknownAction), errors.Is(err, ErrBadJoin):
			// A forged or unusable signal is dropped; the stream stays usable.
		default:
			if errors.Is(err, ErrMalformed) {
				_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
				return
			}
		}
	}
}

// Process verifies one SIGNAL wrapper and delivers it. It is used by the stream handler and by
// the CALL_SIGNAL datagram loop; a bad signature, a wrong sender, an unknown action and a
// duplicate are dropped with an error, never delivered.
func (s *Service) Process(ctx context.Context, p Peer, raw []byte) error {
	sig, err := Parse(raw)
	if err != nil {
		return err
	}
	if err := sig.Verify(p.Device); err != nil {
		return err
	}
	if s.duplicate(sig.Msg.Sender, sig.Msg.MsgId) {
		return ErrDuplicate
	}
	if sig.Msg.Req != 0 {
		l := s.link(p.Device)
		l.mu.Lock()
		ch := l.pending[sig.Msg.Req]
		delete(l.pending, sig.Msg.Req)
		l.mu.Unlock()
		if ch != nil {
			select {
			case ch <- sig.Msg:
			default:
			}
			return nil
		}
	}
	if s.cfg.OnSignal != nil {
		s.cfg.OnSignal(ctx, p, sig.Msg, raw)
	}
	return nil
}

// Send signs a forwarded signal (or leaves a hop-local one unsigned), fills the sender, message
// id and timestamp, and writes it. A lossy signal (NACK, reports) is also duplicated on a
// CALL_SIGNAL datagram when it fits.
func (s *Service) Send(ctx context.Context, p Peer, m *pb.CallSignal) error {
	if m == nil {
		return fmt.Errorf("%w: nil signal", ErrMalformed)
	}
	self := s.cfg.Origin.ID()
	m.Sender = self[:]
	if m.MsgId == 0 {
		m.MsgId = s.cfg.Rand()
		if m.MsgId == 0 {
			m.MsgId = 1
		}
	}
	if m.Timestamp == 0 {
		m.Timestamp = s.cfg.Now().UnixMilli()
	}
	var origin keys.AKey
	var sign func([]byte) ([]byte, error)
	if IsForwarded(m.Action) {
		origin, sign = s.cfg.Origin, s.cfg.Sign
	}
	raw, err := Encode(m, origin, sign)
	if err != nil {
		return err
	}
	return s.sendRaw(ctx, p, raw, m.Action)
}

// Forward writes a received wrapper unchanged: a relay never re-encodes the signed bytes.
func (s *Service) Forward(ctx context.Context, p Peer, raw []byte) error {
	return s.sendStream(ctx, p, raw)
}

// Request sends a request-like signal and waits for its answer, which echoes Req. A request
// that gets no answer in RequestTimeout is abandoned; the stream stays usable (services.md 9.1).
func (s *Service) Request(ctx context.Context, p Peer, m *pb.CallSignal) (*pb.CallSignal, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil signal", ErrMalformed)
	}
	req := s.req.Add(1)
	if req == 0 {
		req = s.req.Add(1)
	}
	m.Req = req
	l := s.link(p.Device)
	ch := make(chan *pb.CallSignal, 1)
	l.mu.Lock()
	if l.pending == nil {
		l.pending = map[uint64]chan *pb.CallSignal{}
	}
	l.pending[req] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.pending[req] == ch {
			delete(l.pending, req)
		}
		l.mu.Unlock()
	}()

	if err := s.Send(ctx, p, m); err != nil {
		return nil, err
	}
	select {
	case answer := <-ch:
		return answer, nil
	case <-s.cfg.After(s.cfg.RequestTimeout):
		return nil, ErrTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrClosed
	}
}

func (s *Service) sendRaw(ctx context.Context, p Peer, raw []byte, action Action) error {
	streamErr := s.sendStream(ctx, p, raw)
	lossy := IsLossy(action) && s.cfg.Datagrams != nil && len(raw) <= MaxDatagramSignal
	if !lossy {
		return streamErr
	}
	dgErr := s.cfg.Datagrams.Send(ctx, p.Device, frame.DgramCallSignal, raw)
	if streamErr != nil && dgErr != nil {
		return streamErr
	}
	return nil
}

func (s *Service) sendStream(ctx context.Context, p Peer, raw []byte) error {
	if s.isClosed() {
		return ErrClosed
	}
	st, err := s.streamFor(ctx, p.Device)
	if err != nil {
		return err
	}
	return st.WriteFrame(TypeSignal, 0, raw)
}

func (s *Service) streamFor(ctx context.Context, device [32]byte) (Stream, error) {
	l := s.link(device)
	l.mu.Lock()
	st := l.stream
	l.mu.Unlock()
	if st != nil {
		return st, nil
	}
	l.openMu.Lock()
	defer l.openMu.Unlock()
	l.mu.Lock()
	st = l.stream
	l.mu.Unlock()
	if st != nil {
		return st, nil
	}
	if s.cfg.Open == nil {
		return nil, ErrNoTransport
	}
	p, st, err := s.cfg.Open(ctx, device)
	if err != nil {
		return nil, err
	}
	l.attach(p, st)
	go s.serve(s.ctx, p, st)
	return st, nil
}

// ReceiveDatagrams delivers CALL_SIGNAL datagrams until the port or ctx ends. Malformed and
// forged datagrams are dropped silently: a datagram is never answered with an error.
func (s *Service) ReceiveDatagrams(ctx context.Context) error {
	if s.cfg.Datagrams == nil {
		return ErrNoDatagrams
	}
	for {
		d, err := s.cfg.Datagrams.Receive(ctx)
		if err != nil {
			return err
		}
		if d.Type != frame.DgramCallSignal {
			continue
		}
		_ = s.Process(ctx, d.Peer, d.Payload)
	}
}

// Close stops the service, closes its streams and the datagram port.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	links := make([]*link, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()

	s.cancel()
	for _, l := range links {
		l.mu.Lock()
		st := l.stream
		l.stream = nil
		l.mu.Unlock()
		if st != nil {
			_ = st.Close()
		}
	}
	if s.cfg.Datagrams != nil {
		return s.cfg.Datagrams.Close()
	}
	return nil
}

func (s *Service) link(device [32]byte) *link {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.links[device]
	if l == nil {
		l = &link{device: device, pending: map[uint64]chan *pb.CallSignal{}}
		s.links[device] = l
	}
	return l
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (l *link) attach(p Peer, st Stream) {
	l.mu.Lock()
	l.peer = p
	l.stream = st
	l.mu.Unlock()
}

func (l *link) detach(st Stream) {
	l.mu.Lock()
	if l.stream == st {
		l.stream = nil
	}
	l.mu.Unlock()
}

func (s *Service) duplicate(sender []byte, msgID uint64) bool {
	if msgID == 0 || len(sender) != 32 {
		return false
	}
	var k dedupeKey
	copy(k.sender[:], sender)
	k.msgID = msgID
	now := s.cfg.Now()
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if t, ok := s.seen[k]; ok && now.Sub(t) < dedupeTTL {
		return true
	}
	// Make room by dropping the oldest entries only: forgetting everything would let a peer that
	// fills the table replay signals it saw just before (T9.1 finding 2).
	for len(s.seenOrder) > 0 && (len(s.seen) >= dedupeMax || now.Sub(s.seenOrder[0].at) >= dedupeTTL) {
		e := s.seenOrder[0]
		s.seenOrder = s.seenOrder[1:]
		if s.seen[e.key] == e.at {
			delete(s.seen, e.key)
		}
	}
	s.seen[k] = now
	s.seenOrder = append(s.seenOrder, dedupeEntry{key: k, at: now})
	return false
}

// SenderID is the device ID of the local origin key.
func (s *Service) SenderID() [32]byte { return s.cfg.Origin.ID() }
