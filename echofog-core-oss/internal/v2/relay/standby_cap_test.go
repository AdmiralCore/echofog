package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// N4 (T6.1 acceptance): a session that may hold no standby path (low_power) trims every bound path
// at once. Standby must not read that as a dead relay and rebind it every Poll.
func TestStandbyDoesNotChurnOnLowPowerSession(t *testing.T) {
	m := newMulti(t, 3, []int{0, 1, 2})
	binder := mux.NewBinder()
	out := &answered{ok: make(chan struct{})}
	for i := 0; i < 3; i++ {
		m.serveB(i, binder, out)
	}
	ctx := m.worlds[0].ctx
	dial := func(ctx context.Context, c mux.Carrier) (*session.Established, error) {
		return session.Dial(ctx, m.bn.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier}, m.an.local(false),
			session.WithCarrier(c), session.WithPathKind(mux.PathRelay), session.WithLowPower(true))
	}
	est, first, err := Establish(ctx, []Hop{m.hop(0)}, m.b[0].node, dial, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = est.Session.Close() })
	<-out.ok

	var asked atomic.Int32
	sb := &Standby{Est: est, Target: m.b[0].node, Want: 2, Poll: 20 * time.Millisecond,
		Next: func(exclude map[[32]byte]struct{}) (Hop, bool) {
			asked.Add(1)
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
	time.Sleep(600 * time.Millisecond) // ~30 polls
	if n := asked.Load(); n != 0 {
		t.Fatalf("Standby asked for %d relays on a session that may hold only its first path", n)
	}
	if ids, _, _ := est.Paths(); len(ids) != 1 {
		t.Fatalf("paths: %d, want 1", len(ids))
	}
}
