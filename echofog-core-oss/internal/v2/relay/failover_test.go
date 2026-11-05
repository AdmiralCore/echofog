package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// multi is A and B, both connected to several relays and to nothing else: no direct route exists
// (the symmetric-NAT case of T6.1: only outbound sessions to relays).
type multi struct {
	t      *testing.T
	worlds []*world
	a, b   []*side
	an, bn *tnode
}

func newMulti(t *testing.T, relays int, bAt []int) *multi {
	m := &multi{t: t, an: newTNode(t), bn: newTNode(t)}
	for i := 0; i < relays; i++ {
		w := newWorld(t, ServerConfig{})
		m.worlds = append(m.worlds, w)
		m.a = append(m.a, w.join(m.an, "10.0.0.1", true))
		var sb *side
		for _, j := range bAt {
			if j == i {
				sb = w.join(m.bn, "10.0.0.2", true)
			}
		}
		m.b = append(m.b, sb)
	}
	return m
}

func (m *multi) hop(i int) Hop {
	return Hop{Node: m.worlds[i].rNode, Device: m.worlds[i].rNode, Open: m.a[i].open}
}

// answered holds B's session once its first circuit completed.
type answered struct {
	mu   sync.Mutex
	est  *session.Established
	ok   chan struct{}
	once sync.Once
}

// serveB is B's loop on relay i: every INCOMING circuit is either the first session or a path of
// the session that is already there.
func (m *multi) serveB(i int, binder *mux.Binder, out *answered) {
	sb := m.b[i]
	go func() {
		for {
			select {
			case in := <-sb.in:
				go func() {
					est, err := AcceptCircuit(sb.w.ctx, in, session.AcceptConfig{Local: m.bn.local(true)}, binder)
					if err == nil && est != nil {
						out.mu.Lock()
						out.est = est
						out.mu.Unlock()
						out.once.Do(func() { close(out.ok) })
					} else if err != nil && !errors.Is(err, session.ErrPathBound) {
						m.t.Errorf("B accept on relay %d: %v", i, err)
					}
				}()
			case <-sb.w.ctx.Done():
				return
			}
		}
	}()
}

func (m *multi) dialSession(ctx context.Context, c mux.Carrier) (*session.Established, error) {
	return session.Dial(ctx, m.bn.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier}, m.an.local(false),
		session.WithCarrier(c), session.WithPathKind(mux.PathRelay))
}

func waitPaths(t *testing.T, e *session.Established, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ids, _, _ := e.Paths(); len(ids) == n {
			return
		}
		if time.Now().After(deadline) {
			ids, _, _ := e.Paths()
			t.Fatalf("paths: %d, want %d", len(ids), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A hop that does not know B refuses with the uniform "unavailable"; the next hop is tried and the
// session is built there.
func TestEstablishFallsThroughToTheNextRelay(t *testing.T) {
	m := newMulti(t, 2, []int{1}) // B is connected to relay 1 only
	binder := mux.NewBinder()
	out := &answered{ok: make(chan struct{})}
	m.serveB(1, binder, out)
	est, hop, err := Establish(m.worlds[0].ctx, []Hop{m.hop(0), m.hop(1)}, m.b[1].node, m.dialSession, ConnectOptions{})
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	t.Cleanup(func() { _ = est.Session.Close() })
	if hop.Node != m.worlds[1].rNode {
		t.Fatal("the session was built through the wrong relay")
	}
	<-out.ok
}

func TestEstablishAllRelaysRefuse(t *testing.T) {
	m := newMulti(t, 2, nil)
	_, _, err := Establish(m.worlds[0].ctx, []Hop{m.hop(0), m.hop(1)}, id(9), m.dialSession, ConnectOptions{})
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a RefusedError", err)
	}
}

func TestEstablishWithoutHops(t *testing.T) {
	_, _, err := Establish(context.Background(), nil, id(9), nil, ConnectOptions{})
	if !errors.Is(err, ErrNoRelay) {
		t.Fatalf("got %v, want ErrNoRelay", err)
	}
}

// Both ends reach only relays. The session is built through R0; Standby adds a second relay path
// through R1; R0 dies and the same session (same SID, no new handshake) keeps working; Standby
// then restores the second path through R2.
func TestSymmetricNATSessionSurvivesRelayLoss(t *testing.T) {
	m := newMulti(t, 3, []int{0, 1, 2})
	binder := mux.NewBinder()
	out := &answered{ok: make(chan struct{})}
	for i := 0; i < 3; i++ {
		m.serveB(i, binder, out)
	}
	ctx := m.worlds[0].ctx
	est, first, err := Establish(ctx, []Hop{m.hop(0)}, m.b[0].node, m.dialSession, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = est.Session.Close() })
	<-out.ok
	out.mu.Lock()
	bEst := out.est
	out.mu.Unlock()
	sid := est.SID
	echoOnce(t, est.Session, bEst.Session, []byte("through R0"))

	sb := &Standby{Est: est, Target: m.b[0].node, Want: 2, Poll: 50 * time.Millisecond,
		Next: func(exclude map[[32]byte]struct{}) (Hop, bool) {
			for i := 0; i < 3; i++ {
				h := m.hop(i)
				if _, no := exclude[h.Node]; !no {
					return h, true
				}
			}
			return Hop{}, false
		}}
	sb.Adopt(first)
	sctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go sb.Run(sctx)
	waitPaths(t, est, 2)

	// R0 goes away: its circuits die.
	w0 := m.worlds[0]
	w0.srv.mu.Lock()
	var cs []*circuit
	for _, c := range w0.srv.circuits {
		cs = append(cs, c)
	}
	w0.srv.mu.Unlock()
	for _, c := range cs {
		w0.srv.kill(c)
	}
	waitPaths(t, est, 1)
	echoOnce(t, est.Session, bEst.Session, []byte("after R0 died"))
	if est.SID != sid || est.Session.Err() != nil {
		t.Fatalf("session changed or ended: err=%v", est.Session.Err())
	}
	// A new standby appears through R2 (R0 is excluded as dead, R1 is in use).
	waitPaths(t, est, 2)
	for _, h := range sb.Hops() {
		if h == m.worlds[0].rNode {
			t.Fatal("standby hops include the dead relay")
		}
	}
	echoOnce(t, est.Session, bEst.Session, []byte("on the restored standby"))
}
