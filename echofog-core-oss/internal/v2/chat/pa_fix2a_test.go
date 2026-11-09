package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/dht"
)

// ---- ЧМ-2: directory lookups of an incoming init are cached, bounded and timed -------------------

func totalResolves(w *world) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, v := range w.resolves {
		n += v
	}
	return n
}

// A lookup answer is remembered for the contact: repeated init envelopes, including ones that name a
// device the account does not have, do not become repeated DHT searches (bounded amplification).
func TestIncomingLookupIsCachedPerAccount(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	for i := 0; i < 5; i++ {
		env := craft(t, b, b, a.self.Account, fakeDevice(), []byte("init bytes"), true)
		if st := b.svc.Receive(context.Background(), env); st != chatpb.StatusRefused {
			t.Fatalf("init %d: status %d", i, st)
		}
	}
	if n := w.resolves[a.self.Node]; n != 1 {
		t.Fatalf("%d lookups for five inits of one account, want 1", n)
	}
	// after the TTL one fresh lookup is allowed
	w.clock.Add(2 * time.Minute)
	b.svc.Receive(context.Background(), craft(t, b, b, a.self.Account, fakeDevice(), []byte("init bytes"), true))
	if n := w.resolves[a.self.Node]; n != 2 {
		t.Fatalf("%d lookups after the cache TTL, want 2", n)
	}
}

// blockingDir waits for its context and records whether the caller gave it a deadline.
type blockingDir struct {
	deadline bool
}

func (d *blockingDir) Resolve(ctx context.Context, _ [32]byte) (*dht.Resolved, error) {
	_, d.deadline = ctx.Deadline()
	if !d.deadline {
		return nil, errors.New("no deadline") // fail fast instead of hanging the red test
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A directory lookup made for an incoming envelope has a time limit of its own: the Tick context
// alone may have none.
func TestIncomingLookupHasATimeLimit(t *testing.T) {
	w := newWorld(t)
	d := &blockingDir{}
	w.tune = func(sc *ServiceConfig) {
		sc.Dir = d
		sc.ResolveTimeout = 20 * time.Millisecond
	}
	b := w.newPeer()
	env := craft(t, b, b, fakeAccount(), fakeDevice(), []byte("init bytes"), true)
	start := time.Now()
	if st := b.svc.Receive(context.Background(), env); st != chatpb.StatusRefused {
		t.Fatalf("status %d, want refused", st)
	}
	if !d.deadline {
		t.Fatal("the lookup ran without a deadline")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the lookup took %v with a 20 ms limit", elapsed)
	}
}

// Lookups caused by incoming envelopes are bounded per window: a flood of envelopes for new accounts
// cannot turn into an unbounded number of DHT searches.
func TestIncomingLookupsAreBoundedPerWindow(t *testing.T) {
	w := newWorld(t)
	w.tune = func(sc *ServiceConfig) { sc.MaxResolvePerMinute = 2 }
	b := w.newPeer()
	for i := 0; i < 3; i++ {
		env := craft(t, b, b, fakeAccount(), fakeDevice(), []byte("init bytes"), true)
		if st := b.svc.Receive(context.Background(), env); st != chatpb.StatusRefused {
			t.Fatalf("init %d: status %d", i, st)
		}
	}
	if n := totalResolves(w); n != 2 {
		t.Fatalf("%d lookups for three accounts with a budget of two, want 2", n)
	}
	// the next window allows a fresh lookup
	w.clock.Add(2 * time.Minute)
	b.svc.Receive(context.Background(), craft(t, b, b, fakeAccount(), fakeDevice(), []byte("init bytes"), true))
	if n := totalResolves(w); n != 3 {
		t.Fatalf("%d lookups after the window, want 3", n)
	}
}
