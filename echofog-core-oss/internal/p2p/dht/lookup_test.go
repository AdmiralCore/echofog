package dht

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A key nobody holds: the lookup ends once the closest peers have all answered, not when its
// time runs out.
func TestMissEndsWhenTheClosestPeersHaveAnswered(t *testing.T) {
	a, b, c := newTestNode(t), newTestNode(t), newTestNode(t)
	a.dial(t, b)
	a.dial(t, c)

	start := time.Now()
	_, err := a.dht.GetFresh(swarm.BundleKey(c.id))
	if !errors.Is(err, ErrValueNotFound) {
		t.Fatalf("GetFresh = %v, want %v", err, ErrValueNotFound)
	}
	if took := time.Since(start); took > valueLookupWait/2 {
		t.Fatalf("a miss took %v: the lookup waited for its deadline instead of converging", took)
	}
	if _, ok := a.dht.misses.Get(string(swarm.BundleKey(c.id))); !ok {
		t.Fatal("a converged miss is remembered")
	}
}

// A peer that does not answer at its address leaves the table and is not dialed by the next
// lookup, however often other nodes keep naming it.
func TestDeadPeersAreEvictedAndNotDialedAgain(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	a.dial(t, b)
	a.sw.SetAutoDial(true)
	deadPub, _ := randomPublicPeer()
	dead := types.PeerPubKeyToID(deadPub)
	b.dht.routingTable.AddOrUpdatePeer(deadPub, "127.0.0.1:9") // nobody listens there
	key := swarm.BundleKey(b.id)

	if _, err := a.dht.GetFresh(key); !errors.Is(err, ErrValueNotFound) {
		t.Fatalf("GetFresh = %v, want %v", err, ErrValueNotFound)
	}
	waitUntil(t, "the dead peer is marked unreachable", func() bool { return a.dht.isUnreachable(dead) })
	for _, id := range a.dht.KnownPeers() {
		if id == dead {
			t.Fatal("a peer that did not answer stays in the table")
		}
	}

	// b still names it; a must not dial it again
	a.dht.ForgetLookups()
	start := time.Now()
	_, _ = a.dht.GetFresh(key)
	if took := time.Since(start); took > lookupDialWait/2 {
		t.Fatalf("the second lookup took %v: the dead peer was dialed again", took)
	}
	for _, id := range a.dht.KnownPeers() {
		if id == dead {
			t.Fatal("an unreachable peer named in an answer went back into the table")
		}
	}
}

// Lookups that want the same peer at the same time share one dial, and the dial reaches its
// verdict even when the lookups stop waiting.
func TestConcurrentDialsOfOnePeerAreOne(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	a.dial(t, b)
	a.sw.SetAutoDial(true)
	deadPub, _ := randomPublicPeer()
	dead := types.PeerPubKeyToID(deadPub)

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.dht.dialOnce(t.Context(), dead, "127.0.0.1:9"); err == nil {
				t.Error("a dial to a port nobody listens on succeeded")
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > lookupDialWait+lookupDialWait/2 {
		t.Fatalf("eight dials of one peer took %v: they were not shared", took)
	}
	if !a.dht.isUnreachable(dead) {
		t.Fatal("a failed dial leaves no verdict")
	}
	if a.dht.isUnreachable(b.id) {
		t.Fatal("a reachable peer was marked unreachable")
	}
}

// What a lookup found is answered from memory for a while; the owner publishing anew forgets it
// on the owner, and a refresh asked for by hand forgets everything.
func TestRecentLookupsAreAnsweredFromMemory(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	b.dial(t, a)
	key := swarm.BundleKey(b.id)
	if err := b.dht.Set(key, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	c := newTestNode(t)
	c.dial(t, a)
	waitUntil(t, "c finds the record through a", func() bool {
		v, err := c.dht.GetFresh(key)
		if err != nil {
			c.dht.ForgetLookups() // a may not have it yet
			return false
		}
		return string(v) == "v1"
	})
	if v, ok := c.dht.recent.Get(string(key)); !ok || string(v) != "v1" {
		t.Fatal("a value found is remembered")
	}
	c.dht.ForgetLookups()
	if _, ok := c.dht.recent.Get(string(key)); ok {
		t.Fatal("ForgetLookups keeps values")
	}
	if _, ok := b.dht.recent.Get(string(key)); ok {
		t.Fatal("the owner remembers its own key")
	}
}
