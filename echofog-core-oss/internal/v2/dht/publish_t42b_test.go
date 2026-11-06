package dht

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestPublishRefusesNonPublicCards(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	net := newFakeNet(t, 20, 10, false, rnd)
	cl := newTestClient(t, net, 8700, rnd, nil)

	peer := buildCard(t, 7000, 1, srvNow, objects.ScopePeer, 0, testAddr("203.0.113.70", 4430))
	if err := cl.Publish(peer.Env.Raw); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("Publish(peer card): %v", err)
	}
	if _, err := cl.Put(ctxT(t), peer.Env.Raw); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("Put(peer card): %v", err)
	}
	if cl.Pending() != 0 {
		t.Fatal("a refused card was registered")
	}
	pub := buildCard(t, 7001, 1, srvNow, objects.ScopePublic, 0, testAddr("203.0.113.71", 4430))
	if err := cl.Publish(pub.Env.Raw); err != nil {
		t.Fatalf("Publish(public card): %v", err)
	}
	if cl.Pending() != 1 {
		t.Fatal("the public card was not registered")
	}
}

func TestWithdrawAllForgetsRegisteredRecords(t *testing.T) {
	rnd := rand.New(rand.NewSource(2))
	net := newFakeNet(t, 20, 10, false, rnd)
	cl := newTestClient(t, net, 8701, rnd, nil)
	for i := uint64(0); i < 2; i++ {
		c := buildCard(t, 7100+i, 1, srvNow, objects.ScopePublic, 0, testAddr("203.0.113.80", 4430))
		if err := cl.Publish(c.Env.Raw); err != nil {
			t.Fatal(err)
		}
	}
	if cl.Pending() != 2 {
		t.Fatalf("pending %d", cl.Pending())
	}
	cl.WithdrawAll()
	if cl.Pending() != 0 {
		t.Fatalf("pending %d after WithdrawAll", cl.Pending())
	}
}

// After a failed attempt the record waits for its retry; PublishNow makes it due at once.
func TestPublishNowOvertakesTheRetryDelay(t *testing.T) {
	rnd := rand.New(rand.NewSource(3))
	net := newFakeNet(t, 20, 10, false, rnd)
	for _, fn := range net.nodes {
		fn.mode = dead
	}
	card := buildCard(t, 7200, 1, srvNow, objects.ScopePublic, 0, testAddr("203.0.113.90", 4430))
	cl := newTestClient(t, net, 8702, rnd, nil)
	if err := cl.Publish(card.Env.Raw); err != nil {
		t.Fatal(err)
	}
	cl.Step(ctxT(t)) // fails: the retry is 5 minutes away
	for _, fn := range net.nodes {
		fn.mode = honest
	}
	cl.Step(ctxT(t))
	if got := storesTotal(net); got != 0 {
		t.Fatalf("stored %d before the retry was due", got)
	}
	cl.PublishNow()
	cl.Step(ctxT(t))
	if got := storesTotal(net); got == 0 {
		t.Fatal("PublishNow did not make the record due")
	}
}

func storesTotal(n *fakeNet) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, c := range n.stores {
		total += c
	}
	return total
}
