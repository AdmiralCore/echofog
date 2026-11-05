package relay

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// ErrNoRelay: no relay could carry the circuit (none given, or every one failed).
var ErrNoRelay = errors.New("relay: no usable relay")

// Hop is one relay A can reach: its identity and how to open a relay/2 stream on A's session with it.
type Hop struct {
	Node   [32]byte
	Device [32]byte
	Open   Opener
}

// Establish builds the end-to-end session with target through the first relay that works (T6.1
// step 5, "first path"). For each hop in order it asks for a circuit and runs dial (the A side of
// the handshake, session.Dial with the circuit as carrier and PathRelay) on it. A refusal, a
// timeout or a failed handshake moves to the next hop; the last error is returned when none works.
// Refusals are uniform (services.md §6.3): a hop that does not know B looks like a dead one.
func Establish(ctx context.Context, hops []Hop, target [32]byte,
	dial func(ctx context.Context, c mux.Carrier) (*session.Established, error), opt ConnectOptions) (*session.Established, Hop, error) {
	var last error
	for _, h := range hops {
		if ctx.Err() != nil {
			return nil, Hop{}, ctx.Err()
		}
		circ, err := Dial(ctx, h.Open, h.Node, target, opt)
		if err != nil {
			last = err
			continue
		}
		est, err := dial(ctx, circ)
		if err != nil {
			_ = circ.Close()
			last = err
			continue
		}
		return est, h, nil
	}
	if last == nil {
		last = ErrNoRelay
	}
	return nil, Hop{}, last
}

// AcceptCircuit is the B side of one circuit that Answer accepted. A stream circuit is either the
// first path of a new session or a PATH_BIND of one that exists (binder holds the sessions, so the
// bind needs no new handshake). It returns the new session, or session.ErrPathBound for a path.
func AcceptCircuit(ctx context.Context, in Inbound, cfg session.AcceptConfig, binder *mux.Binder) (*session.Established, error) {
	if in.Stream == nil {
		return nil, errors.New("relay: not a stream circuit")
	}
	cfg.Kind = mux.PathRelay
	cfg.Binder = binder
	prev := cfg.OnEstablished
	cfg.OnEstablished = func(s *mux.Session) {
		binder.Add(s)
		if prev != nil {
			prev(s)
		}
	}
	return session.Accept(ctx, &circuitListener{c: in.Stream}, cfg)
}

type circuitListener struct {
	c    mux.Carrier
	mu   sync.Mutex
	used bool
}

func (l *circuitListener) Accept(ctx context.Context) (mux.Carrier, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.c, nil
	}
	l.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}
func (l *circuitListener) Addr() net.Addr { return nil }
func (l *circuitListener) Close() error   { return nil }

var _ carrier.Listener = (*circuitListener)(nil)

// DeadCooldown is how long a relay whose path died is not chosen again for a standby path.
const DeadCooldown = 2 * time.Minute

// Standby keeps a session on Want relay paths, through different relays (T6.1 step 5, "R falls: the
// session moves to another R without a new handshake"). The paths belong to one session, so a
// dead R costs one path, not the session: the others carry on and a new one is bound with
// PATH_BIND through the next relay Next offers (never one that carries a live path of this session
// or died within DeadCooldown). All paths share the session's single nonce counter per direction
// (wire.md §8.4), so adding or losing one never repeats a nonce.
type Standby struct {
	Est    *session.Established
	Target [32]byte
	Want   int // relay paths to hold (default 2)
	// Next returns a relay not in exclude (node IDs), or false when none is left.
	Next    func(exclude map[[32]byte]struct{}) (Hop, bool)
	Connect ConnectOptions
	Clock   carrier.Clock
	// Poll is how often the path set is checked (default 3 s).
	Poll time.Duration

	mu    sync.Mutex
	paths map[uint64][32]byte // live path ID → relay node
	dead  map[[32]byte]time.Time
	adopt [][32]byte
}

// Adopt records that the session's first path (the only one when Run starts) goes through hop.
func (s *Standby) Adopt(h Hop) {
	s.mu.Lock()
	s.adopt = append(s.adopt, h.Node)
	s.mu.Unlock()
}

// Hops returns the relays that carry the session's live paths.
func (s *Standby) Hops() [][32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][32]byte
	for _, n := range s.paths {
		out = append(out, n)
	}
	return out
}

func (s *Standby) clock() carrier.Clock {
	if s.Clock != nil {
		return s.Clock
	}
	return sysClock{}
}

// Run keeps the paths until ctx ends or the session does.
func (s *Standby) Run(ctx context.Context) {
	if s.Want <= 0 {
		s.Want = 2
	}
	if s.Poll <= 0 {
		s.Poll = 3 * time.Second
	}
	s.mu.Lock()
	s.paths = map[uint64][32]byte{}
	s.dead = map[[32]byte]time.Time{}
	adopt := s.adopt
	s.mu.Unlock()
	if ids, _, _ := s.Est.Paths(); len(ids) == 1 && len(adopt) == 1 {
		s.mu.Lock()
		s.paths[ids[0]] = adopt[0]
		s.mu.Unlock()
	}
	for {
		s.step(ctx)
		tick := make(chan struct{})
		t := s.clock().AfterFunc(s.Poll, func() { close(tick) })
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.Est.Session.Done():
			t.Stop()
			return
		case <-tick:
		}
	}
}

func (s *Standby) step(ctx context.Context) {
	now := s.clock().Now()
	live := map[uint64]bool{}
	ids, _, _ := s.Est.Paths()
	for _, id := range ids {
		live[id] = true
	}
	s.mu.Lock()
	for id, node := range s.paths {
		if !live[id] {
			s.dead[node] = now
			delete(s.paths, id)
		}
	}
	for n, at := range s.dead {
		if now.Sub(at) > DeadCooldown {
			delete(s.dead, n)
		}
	}
	need := s.Want - len(s.paths)
	exclude := map[[32]byte]struct{}{}
	for _, n := range s.paths {
		exclude[n] = struct{}{}
	}
	for n := range s.dead {
		exclude[n] = struct{}{}
	}
	s.mu.Unlock()
	for ; need > 0 && ctx.Err() == nil; need-- {
		h, ok := s.Next(exclude)
		if !ok {
			return
		}
		exclude[h.Node] = struct{}{}
		id, err := s.bind(ctx, h)
		s.mu.Lock()
		if err != nil {
			s.dead[h.Node] = now
		} else {
			s.paths[id] = h.Node
		}
		s.mu.Unlock()
	}
}

func (s *Standby) bind(ctx context.Context, h Hop) (uint64, error) {
	circ, err := Dial(ctx, h.Open, h.Node, s.Target, s.Connect)
	if err != nil {
		return 0, err
	}
	id, err := s.Est.BindCarrier(ctx, circ, mux.PathRelay)
	if err != nil {
		return 0, err // BindCarrier closed the circuit
	}
	return id, nil
}
