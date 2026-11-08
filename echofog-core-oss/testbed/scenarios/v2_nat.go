package scenarios

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/punch"
	"github.com/echofog/echofog-core/testbed/gate"
)

func natScenario() Scenario {
	return Scenario{Name: "v2-nat", Title: "NAT matrix: punch where it can, the relay where it cannot", Live: liveNAT, Run: runNat}
}

// scaledClock runs the product's timers at a fraction of their length (AttemptTime is 5 s, the pause
// between rounds 90 s); the order of events does not change, only how long the run waits.
type scaledClock struct{ div time.Duration }

func (c scaledClock) Now() time.Time { return time.Now() }
func (c scaledClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	return time.AfterFunc(d/c.div, f)
}

// punchSide is one end of a punch: its NAT, what it has written to, and the punch.Net the engine
// drives. The engine is the product's; the NAT is the model's.
type punchSide struct {
	nat   *natBox
	offer netip.AddrPort // the address the relay saw: what the engine offers
	peer  *punchSide

	mu       sync.Mutex
	ports    map[uint16]bool // every destination port this side wrote to
	inits    int
	junk     int
	attempts int
}

func (s *punchSide) note(to netip.AddrPort) {
	s.mu.Lock()
	s.ports[to.Port()] = true
	s.mu.Unlock()
}

func (s *punchSide) OwnAddrs() []netip.AddrPort { return []netip.AddrPort{s.offer} }

func (s *punchSide) SendJunk(to netip.AddrPort, _ []byte) error {
	s.note(to)
	s.mu.Lock()
	s.junk++
	s.mu.Unlock()
	s.nat.Send(to)
	return nil
}

// Dial sends Init datagrams to the targets, a few at a time, until one gets through the peer's NAT
// (then the inner handshake completes: a carrier) or the budget or the attempt time is spent.
func (s *punchSide) Dial(ctx context.Context, targets []netip.AddrPort, budget int) (mux.Carrier, error) {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	for i := 0; i < budget; i++ {
		for _, t := range targets {
			s.note(t)
			s.mu.Lock()
			s.inits++
			s.mu.Unlock()
			src := s.nat.Send(t)
			if t.Addr() == s.peer.nat.ext && s.peer.nat.Accepts(src, t.Port()) {
				pair := memcarrier.NewQUIC()
				_ = pair.Server.Close()
				return pair.Client, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// controlLink carries the engines' control frames to the other engine, as the relayed session does.
type controlLink struct {
	to   func() *punch.Engine
	rtt  time.Duration
	sent atomic.Int64
}

func (l *controlLink) SendControl(typ uint64, flags byte, payload []byte) error {
	l.sent.Add(1)
	p := append([]byte(nil), payload...)
	go func() {
		if e := l.to(); e != nil {
			e.HandleControl(frame.Frame{Type: typ, Flags: flags, Payload: p})
		}
	}()
	return nil
}

func (l *controlLink) Ping(ctx context.Context) (time.Duration, error) { return l.rtt, ctx.Err() }

// pairOutcome is what the product's engine did for one initiator and responder behind two NATs.
type pairOutcome struct {
	a, b   NATType
	direct bool
	sides  [2]*punchSide
}

// punchPair runs the product's punch engines on both ends until a direct path is bound or the time
// is up.
func punchPair(ctx context.Context, a, b NATType, within time.Duration) pairOutcome {
	relay := netip.MustParseAddrPort("198.51.100.1:443")
	sa := &punchSide{ports: map[uint16]bool{}}
	sb := &punchSide{ports: map[uint16]bool{}}
	sa.nat = newNATBox(a, netip.MustParseAddr("203.0.113.10"), 4433)
	sb.nat = newNATBox(b, netip.MustParseAddr("203.0.113.20"), 4433)
	sa.peer, sb.peer = sb, sa
	sa.offer, sb.offer = sa.nat.Send(relay), sb.nat.Send(relay) // each end told the relay its address

	var ea, eb *punch.Engine
	var bound atomic.Bool
	clk := scaledClock{div: 50}
	rules := func(own, peer *punchSide) func() punch.Rules {
		return func() punch.Rules {
			return punch.Rules{PeerIP: peer.nat.ext, OwnIP: own.nat.ext, CardIPs: []netip.Addr{peer.nat.ext}}
		}
	}
	ea = punch.New(punch.Config{Role: punch.Initiator, Link: &controlLink{to: func() *punch.Engine { return eb }, rtt: 20 * time.Millisecond},
		Net: sa, Clock: clk, Rules: rules(sa, sb), HasDirect: bound.Load,
		Bind: func(context.Context, mux.Carrier) error { bound.Store(true); return nil }})
	eb = punch.New(punch.Config{Role: punch.Responder, Link: &controlLink{to: func() *punch.Engine { return ea }, rtt: 20 * time.Millisecond},
		Net: sb, Clock: clk, Rules: rules(sb, sa)})
	defer ea.Close()
	defer eb.Close()

	rctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	_ = ea.Run(rctx)
	return pairOutcome{a: a, b: b, direct: bound.Load(), sides: [2]*punchSide{sa, sb}}
}

// feasible is what the physics of the two NATs allows to an engine that writes only to the offered
// addresses: the responder's junk opens the responder's NAT towards the initiator's offered address,
// and the initiator's Init has to arrive from the address that was opened.
func feasible(a, b NATType) bool {
	switch {
	case b == Symmetric:
		return false // the offered port belongs to the relay's mapping; the peer's Init meets another port
	case a == Symmetric:
		return b == FullCone || b == AddressRestricted // the Init leaves from a new port: only a NAT that does not look at it lets it in
	}
	return true
}

func runNat(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-nat", Mode: "model", Seed: env.Seed}
	types := []NATType{FullCone, AddressRestricted, PortRestricted, Symmetric}
	var mu sync.Mutex
	var outs []pairOutcome
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, a := range types {
		for _, b := range types {
			a, b := a, b
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				o := punchPair(ctx, a, b, 3*time.Second)
				mu.Lock()
				outs = append(outs, o)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	group := func(id, limit string, in func(pairOutcome) bool) {
		total, ok := 0, 0
		var bad []string
		for _, o := range outs {
			if !in(o) {
				continue
			}
			total++
			if o.direct == feasible(o.a, o.b) {
				ok++
			} else {
				bad = append(bad, fmt.Sprintf("%s to %s: %s (the NATs allow %s)", o.a, o.b, path(o.direct), path(feasible(o.a, o.b))))
			}
		}
		st, note := gate.Green, ""
		if total == 0 || ok != total {
			st = gate.Red
			if len(bad) > 0 {
				note = fmt.Sprint(bad)
			}
		}
		res.Rows = append(res.Rows, newRow(id, "pairs as the physics allows", fmt.Sprintf("%d of %d", ok, total), limit, st, note))
	}
	cone := func(t NATType) bool { return t != Symmetric }
	group("v2-nat/cone-pairs", "direct for all 9 cone pairs", func(o pairOutcome) bool { return cone(o.a) && cone(o.b) })
	group("v2-nat/symmetric-initiator", "direct against full and address-restricted, relay against port-restricted and symmetric", func(o pairOutcome) bool { return o.a == Symmetric })
	group("v2-nat/symmetric-responder", "relay: the offered port is the relay's", func(o pairOutcome) bool { return o.b == Symmetric && o.a != Symmetric })

	// the engine writes to the addresses it was given, within its budget, and to nothing else
	spray, over := 0, 0
	for _, o := range outs {
		allowed := map[uint16]bool{o.sides[0].offer.Port(): true, o.sides[1].offer.Port(): true}
		for _, s := range o.sides {
			s.mu.Lock()
			for p := range s.ports {
				if !allowed[p] {
					spray++
				}
			}
			per := s.attempts
			if per < 1 {
				per = 1
			}
			// per attempt: MaxInitDatagrams rounds of at most MaxTargets targets; junk JunkMax per target
			if s.inits > per*punch.MaxInitDatagrams*punch.MaxTargets || s.junk > punch.MaxAttempts*punch.JunkMax*punch.MaxTargets {
				over++
			}
			s.mu.Unlock()
		}
	}
	st := gate.Green
	if spray != 0 || over != 0 {
		st = gate.Red
	}
	res.Rows = append(res.Rows, newRow("v2-nat/no-port-spray", "ports written to beyond the offered ones / sides over budget",
		fmt.Sprintf("%d / %d", spray, over), "0 / 0", st, ""))
	res.NotChecked = []string{
		"symmetric-narrow (a carrier's CGNAT with a small port block): the engine treats it as symmetric, so it ends on the relay; the birthday punch that reaches it is the owner's (T6.7) and is not run",
		"the relay itself and the message that follows on the relayed session: the engine is driven with a control link between the two engines, not through a relay node",
		"mapping that changes over time, hairpinning, port-preserving and port-overloading NATs, IPv6 and UPnP/PCP mapping: not modelled",
		"the engine runs on a clock sped up 50 times; the order of its steps is the product's, the waits are not real",
	}
	return res
}

func path(direct bool) string {
	if direct {
		return "direct"
	}
	return "relay"
}
