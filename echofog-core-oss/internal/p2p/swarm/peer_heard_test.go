package swarm

import (
	"testing"
	"time"
)

// A peer counts as heard from when it connected and whenever it sends something, and stops
// counting once it has been quiet for longer than asked.
func TestHeardWithin(t *testing.T) {
	p := &Peer{connectedAt: time.Now()}
	if !p.HeardWithin(time.Second) {
		t.Fatal("a peer that just connected was not heard from")
	}
	p.connectedAt = time.Now().Add(-time.Minute)
	if p.HeardWithin(15 * time.Second) {
		t.Fatal("a peer quiet for a minute still counts as heard")
	}
	p.heard()
	if !p.HeardWithin(15 * time.Second) {
		t.Fatal("a frame from the peer did not count")
	}
}

// A connected peer answers the ping; one that is not connected does not.
func TestAnswers(t *testing.T) {
	_, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()
	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	if err := trTwo.DialEarly(ctxTwo, trOne.Addr().String()); err != nil {
		t.Fatalf("dial: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(swOne.GetAllPeers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	peers := swOne.GetAllPeers()
	if len(peers) == 0 {
		t.Fatal("not connected")
	}
	p := peers[0]
	p.lastHeard.Store(0)
	if !swOne.Answers(p.ID()) {
		t.Fatal("a connected peer did not answer")
	}
	if p.lastHeard.Load() == 0 {
		t.Fatal("the answer did not count as hearing from the peer")
	}
	if swOne.Answers(swOne.GetSelfID()) {
		t.Fatal("a peer that is not connected answered")
	}
}
