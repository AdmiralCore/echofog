package node_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// FIX-RACE-NODE: a session can be handed to the runtime from a goroutine the runtime does not
// track (a service handler: relay onIncoming -> takeAccepted -> ensureServing) while Stop is
// waiting for the tracked ones. Counting new goroutines then races with wg.Wait. This test parks
// such a call right before the goroutines are counted, lets Stop drain everything else, and then
// releases it; run under -race it reports the WaitGroup race, and after the fix the late call is
// refused because the runtime has stopped.
func TestStopDoesNotRaceWithALateServeStart(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	mod := &fakeModule{got: make(chan node.Peer, 1), errs: make(chan error, 1)}
	registerFakeModule(t, a.rt, mod)
	registerFakeModule(t, b.rt, mod)
	b.start(t)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.Chat, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = stream.Close()

	var sessions []*session.Established
	for deadline := time.Now().Add(5 * time.Second); len(sessions) == 0; sessions = node.TableSessions(b.rt) {
		if time.Now().After(deadline) {
			t.Fatal("b has no session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Call A is counted before Stop and stays inside its goroutine, so Stop enters wg.Wait with a
	// positive count. Call B is parked before it is counted. When A finishes the count is zero and
	// B starts counting, with nothing ordering it after the Wait.
	var calls atomic.Int32
	aIn, releaseA := make(chan struct{}), make(chan struct{})
	bIn, releaseB := make(chan struct{}), make(chan struct{})
	node.SetBeforeServe(func(stage int) {
		switch {
		case stage == 0 && calls.Add(1) == 2:
			close(bIn)
			<-releaseB
		case stage == 1 && calls.Load() == 1:
			close(aIn)
			<-releaseA
		}
	})
	t.Cleanup(func() { node.SetBeforeServe(nil) })

	var late sync.WaitGroup
	start := func(in chan struct{}) {
		late.Add(1)
		go func() {
			defer late.Done()
			node.ReServe(b.rt, sessions[0])
		}()
		select {
		case <-in:
		case <-time.After(5 * time.Second):
			t.Error("a serve start did not reach its hook")
		}
	}
	start(aIn)
	start(bIn)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = b.rt.Stop(sctx)
	}()
	time.Sleep(500 * time.Millisecond) // Stop is in wg.Wait, held by call A
	close(releaseA)
	time.Sleep(300 * time.Millisecond) // the count reached zero
	close(releaseB)
	late.Wait()
	<-stopped
}
