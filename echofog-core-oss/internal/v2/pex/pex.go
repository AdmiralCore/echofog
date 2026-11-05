// Package pex implements pex/2 (docs/protocol/v2/services.md §3): peer exchange of signed public
// cards and the node's candidate table.
//
// The package is the service logic only. It knows nothing about the runtime: the node supplies its
// own card, its identity and the persistent store through Config, and registers Handle over the
// service registry. The client side (Run/Ask) drives Get over a live session.
package pex

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// Service identity on the wire.
const (
	// Version is the pex/2 service version (services.md §3, versioning.md §3.4).
	Version uint64 = 2
	// TypeGet and TypeCards are the pex/2 frame types.
	TypeGet   uint64 = 0x40
	TypeCards uint64 = 0x41
)

const (
	// MaxCards is the normative maximum of cards in one CARDS frame (services.md §3).
	MaxCards = 16
	// MaxCardsPayload is the CARDS frame maximum (services.md §3).
	MaxCardsPayload = 98304
	// GetTimeout is the client timeout of one GET (services.md §3).
	GetTimeout = 4 * time.Second
	// GetInterval is the answering limit: one GET per 30 s per session.
	GetInterval = 30 * time.Second
	// GetBurst is the answering burst of two GETs per session.
	GetBurst = 2
	// LazyInterval is the client limit: at most one GET per 30 s per session.
	LazyInterval = 30 * time.Second
	// LazyDelayMin and LazyDelayMax bound the uniform delay from session start (services.md §3).
	LazyDelayMin = 1 * time.Second
	LazyDelayMax = 10 * time.Second
	// LazyThreshold is the number of working peers below which a node may ask.
	LazyThreshold = 8
	// LazyThresholdLowPower is the low_power threshold (platforms.md §2, T5.4).
	LazyThresholdLowPower = 4
)

// Peer is the authenticated peer of a pex stream, in the types this package needs. The node
// adapts node.Peer to it.
type Peer struct {
	Node   [32]byte
	Device [32]byte
	Card   *objects.Card
	SID    [16]byte
}

// Request is one GET.
type Request struct {
	// Max is the requested number of cards; 0 means the maximum.
	Max uint32
	// WantFlags requires every bit to be set on the card's flags (registries.md §11).
	WantFlags uint32
	// WantRole filters by card role; 0 means any role (the user role itself is 0).
	WantRole uint32
	// Region filters exit cards by ExitOffer.region.
	Region string
	// Release asks for the latest release manifest the answerer holds (T4.5).
	Release bool
}

// Config wires one pex service.
type Config struct {
	// Store is the candidate table. Required.
	Store *Store
	// SelfNode and SelfDevice identify the local node; it never adds its own card as a candidate.
	SelfNode   [32]byte
	SelfDevice [32]byte
	// OwnCard returns the node's current verified public card, or nil when it has none (D37).
	OwnCard func(now time.Time) *objects.Card
	// Release returns the release manifest to hand out for release = true. Nil until T4.5.
	Release func() []byte
	// Now is the corrected clock (AD13). Nil means time.Now.
	Now func() time.Time
	// Rand returns a value in [0,n). Nil means math/rand/v2.
	Rand func(n int) int
	// Timers drives the lazy client; nil means time.After.
	Timers Timers
	// Log receives diagnostics. Nil is silent.
	Log *slog.Logger
}

// Timers is the injectable timer source of the lazy client (AD13).
type Timers interface {
	After(d time.Duration) <-chan time.Time
}

type realTimers struct{}

func (realTimers) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Service serves and drives pex/2.
type Service struct {
	cfg Config

	mu      sync.Mutex
	buckets map[[16]byte]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a pex service. The store is required; every other source may be absent.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: no candidate store", ErrConfig)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.IntN
	}
	if cfg.Timers == nil {
		cfg.Timers = realTimers{}
	}
	return &Service{cfg: cfg, buckets: map[[16]byte]*bucket{}}, nil
}

// EarlySafe reports whether GET may travel in 0-RTT data (registries.md §3, services.md §3). The
// session-level plumbing is T3.10; this is the service's statement of the rule.
func EarlySafe() bool { return service.EarlySafe(service.PEX, Version, TypeGet) }

// Allow applies the answering limit: one GET per 30 s per session with a burst of two.
func (s *Service) Allow(sid [16]byte, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[sid]
	if b == nil {
		if len(s.buckets) > 1024 {
			s.pruneLocked(now)
		}
		b = &bucket{tokens: GetBurst, last: now}
		s.buckets[sid] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() / GetInterval.Seconds()
		if b.tokens > GetBurst {
			b.tokens = GetBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Service) pruneLocked(now time.Time) {
	for sid, b := range s.buckets {
		if now.Sub(b.last) > 10*GetInterval {
			delete(s.buckets, sid)
		}
	}
}

// Response builds the cards of one GET. The node's own card comes first; only verified public
// cards that were not issued to the asking node are returned. A received card never reaches this
// list before it is tried (services.md §3).
func (s *Service) Response(p Peer, req Request, now time.Time) ([]*objects.Card, []byte) {
	max := int(req.Max)
	if max <= 0 || max > MaxCards {
		max = MaxCards
	}
	out := make([]*objects.Card, 0, max)

	ownDevice := [32]byte{}
	if s.cfg.OwnCard != nil {
		if own := s.cfg.OwnCard(now); own != nil {
			ownDevice = own.DeviceID
			if own.NodeID != p.Node && matches(own, req) {
				out = append(out, own)
			}
		}
	}
	if len(out) < max {
		for _, c := range s.cfg.Store.Serve(now) {
			if len(out) == max {
				break
			}
			if c.DeviceID == p.Device || c.NodeID == p.Node || c.DeviceID == ownDevice {
				continue
			}
			if !matches(c, req) {
				continue
			}
			out = append(out, c)
		}
	}

	var release []byte
	if req.Release && s.cfg.Release != nil {
		release = s.cfg.Release()
	}
	return out, release
}

// matches applies the GET filters. want_flags requires every requested bit; want_role and region
// are ignored when zero/empty.
func matches(c *objects.Card, req Request) bool {
	if c == nil || !c.Publishable() {
		return false
	}
	if req.WantFlags != 0 && c.Body.GetFlags()&req.WantFlags != req.WantFlags {
		return false
	}
	if req.WantRole != 0 && c.Role() != req.WantRole {
		return false
	}
	if req.Region != "" {
		offer := c.ExitOffer()
		if offer == nil || offer.GetRegion() != req.Region {
			return false
		}
	}
	return true
}

// shouldAsk is the lazy PEX trigger: fewer than eight working peers or no relay; low_power lowers
// the peer threshold to four (platforms.md §2).
func shouldAsk(working int, hasRelay, lowPower bool) bool {
	threshold := LazyThreshold
	if lowPower {
		threshold = LazyThresholdLowPower
	}
	return working < threshold || !hasRelay
}

// Run is the lazy PEX client of one session. It waits uniform(1, 10) s from session start and then
// asks at most once per 30 s while the trigger holds. It stops with the session or the runtime.
func (s *Service) Run(ctx context.Context, sess Session, lowPower bool) {
	if sess == nil {
		return
	}
	if !s.wait(ctx, sess, s.delay()) {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if shouldAsk(s.cfg.Store.Working(), s.cfg.Store.HasRelay(), lowPower) {
			if err := s.Ask(ctx, sess, lowPower); err != nil && s.cfg.Log != nil && ctx.Err() == nil {
				s.cfg.Log.Debug("pex: lazy GET failed", "err", err)
			}
		}
		if !s.wait(ctx, sess, LazyInterval) {
			return
		}
	}
}

// delay returns the uniform initial delay in [1 s, 10 s].
func (s *Service) delay() time.Duration {
	steps := int((LazyDelayMax - LazyDelayMin) / time.Second)
	n := s.cfg.Rand(steps + 1)
	if n < 0 {
		n = 0
	}
	return LazyDelayMin + time.Duration(n)*time.Second
}

func (s *Service) wait(ctx context.Context, sess Session, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-sess.Done():
		return false
	case <-s.cfg.Timers.After(d):
		return true
	}
}

// Session is the part of a live session the client uses.
type Session interface {
	OpenStream(ctx context.Context, service, version uint64) (*mux.Stream, error)
	Done() <-chan struct{}
}

// Ask sends one GET and adds the verified cards to the new table. The answerer's rate limit and
// the 4 s timeout mean a refused request simply times out here (services.md §3).
func (s *Service) Ask(ctx context.Context, sess Session, lowPower bool) error {
	_ = lowPower // the trigger is checked by Run; Ask itself is policy-free
	st, err := sess.OpenStream(ctx, service.PEX, Version)
	if err != nil {
		return err
	}
	defer st.Close()
	if spec, ok := service.Builtin(service.PEX); ok {
		st.SetLimiter(spec.Versions[Version].Limiter())
	}

	req := Request{Max: MaxCards}
	if !s.cfg.Store.HasRelay() {
		// No relay among the working peers: ask specifically for relay cards.
		req.WantFlags = objects.FlagRelay
	}
	payload, err := marshalGet(req)
	if err != nil {
		return err
	}
	if err := st.SetReadDeadline(s.cfg.Now().Add(GetTimeout)); err != nil {
		return err
	}
	if err := st.WriteFrame(TypeGet, 0, payload); err != nil {
		return err
	}

	for {
		f, err := st.ReadFrame()
		if err != nil {
			return err
		}
		if f.Type != TypeCards {
			_ = st.CloseWithCode(protocolViolation, "")
			return fmt.Errorf("pex: unexpected frame 0x%x", f.Type)
		}
		msg, err := unmarshalCards(f.Payload)
		if err != nil {
			_ = st.CloseWithCode(protocolViolation, "")
			return err
		}
		if msg.GetReq() != 1 {
			continue
		}
		if len(msg.GetCards()) > MaxCards {
			return fmt.Errorf("pex: %d cards in one answer", len(msg.GetCards()))
		}
		now := s.cfg.Now()
		for _, raw := range msg.GetCards() {
			c, err := objects.VerifyCard(raw, now)
			if err != nil || !c.Publishable() {
				continue
			}
			if c.NodeID == s.cfg.SelfNode || c.DeviceID == s.cfg.SelfDevice {
				continue
			}
			s.cfg.Store.AddCandidate(c, now)
		}
		// release is for T4.5; a client ignores it until then.
		return nil
	}
}
