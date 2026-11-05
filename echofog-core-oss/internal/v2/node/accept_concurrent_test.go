package node_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// deepQueue is a listener with room for many pending carriers.
type deepQueue struct {
	ch   chan mux.Carrier
	done chan struct{}
}

func newDeepQueue(n int) *deepQueue {
	return &deepQueue{ch: make(chan mux.Carrier, n), done: make(chan struct{})}
}

func (q *deepQueue) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (q *deepQueue) Addr() net.Addr { return nil }
func (q *deepQueue) Close() error {
	select {
	case <-q.done:
	default:
		close(q.done)
	}
	return nil
}

var _ carrier.Listener = (*deepQueue)(nil)

// T3.5 acceptance F1: the node runs handshakes of one listener concurrently. Carriers that never
// send a byte must neither hold up an honest initiator nor keep the pre-authentication counters at
// one (handshake §11: the limits count handshakes in flight).
func TestSlowCarriersDoNotBlockAnHonestHandshake(t *testing.T) {
	const stalled = 6
	queue := newDeepQueue(64)
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	mod := &fakeModule{got: make(chan node.Peer, 1), errs: make(chan error, 1)}
	registerFakeModule(t, a.rt, mod)
	registerFakeModule(t, b.rt, mod)
	b.start(t)
	a.start(t)

	var idle []*memcarrier.Pair
	for i := 0; i < stalled; i++ {
		p := memcarrier.NewQUIC()
		idle = append(idle, p)
		queue.ch <- p.Server
	}
	t.Cleanup(func() {
		for _, p := range idle {
			_ = p.Client.Close()
		}
	})

	lim := node.LimitsOf(b.rt)
	deadline := time.Now().Add(3 * time.Second)
	for lim.InFlight() < stalled {
		if time.Now().After(deadline) {
			t.Fatalf("%d handshakes in flight, want %d: the accept loop serves one carrier at a time", lim.InFlight(), stalled)
		}
		time.Sleep(10 * time.Millisecond)
	}

	pair := memcarrier.NewQUIC()
	queue.ch <- pair.Server
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, st, err := a.rt.Open(ctx, b.id.DeviceID, service.Chat, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client}); err != nil {
		t.Fatalf("honest Open behind %d silent carriers: %v", stalled, err)
	} else {
		_ = st.Close()
	}
}
