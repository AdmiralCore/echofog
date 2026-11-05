package relay

import (
	"testing"
	"time"
)

// M4 (accept 1): a datagram over the rate limit is dropped, never slept on. HandleDatagram runs
// in the read loop of the sender's session; sleeping there delays every other service of it.
func TestDatagramOverRateIsDroppedNotDelayed(t *testing.T) {
	w := newWorld(t, ServerConfig{Limits: Limits{RateBytes: 1000}})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	da, err := DialDatagram(w.ctx, a.open, a.hub, func(typ uint64, p []byte) error { return a.toR.SendDatagram(typ, p) }, w.rNode, b.node, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-b.in
	payload := dgramPayload(da.CID(), string(make([]byte, 1000)))
	start := time.Now()
	for i := 0; i < 20; i++ {
		w.srv.HandleDatagram(w.peerOf(a), payload)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("20 datagrams over the rate took %v: the handler sleeps", took)
	}
	if got := w.srv.Stats().DatagramsDropped.Load(); got < 10 {
		t.Fatalf("dropped %d, want most of the 20 over the 1000 B/s limit", got)
	}
}

// A paced stream copy stops waiting as soon as its circuit is killed.
func TestPacingSleepEndsWithTheCircuit(t *testing.T) {
	s := NewServer(ServerConfig{Directory: nil})
	c := &circuit{done: make(chan struct{})}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(c.done)
	}()
	start := time.Now()
	if s.sleep(c, time.Hour) {
		t.Fatal("sleep reported a full wait on a dead circuit")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("sleep took %v after the circuit ended", time.Since(start))
	}
}
