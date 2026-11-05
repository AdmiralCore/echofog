package relay

import (
	"testing"
	"time"
)

// L6 (accept 1): every wait of the server runs on the injected clock (AD13).

// The wait for ACCEPT is bounded by a clock timer, not by a context deadline on the real clock:
// a server whose clock is far from the wall clock still connects circuits.
func TestConnectWorksWhateverTheServerClock(t *testing.T) {
	clk := newFakeClock(time.Now().Add(-48 * time.Hour))
	w := newWorld(t, ServerConfig{Clock: clk})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	circ, err := a.dial(b)
	if err != nil {
		t.Fatalf("circuit through a server on a shifted clock: %v", err)
	}
	_ = circ.Close()
}

// An idle circuit ends when the server clock says so.
func TestIdleCircuitEndsByServerClock(t *testing.T) {
	clk := newFakeClock(time.Now())
	w := newWorld(t, ServerConfig{Clock: clk, Limits: Limits{Idle: 5 * time.Minute}})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	if _, err := a.dial(b); err != nil {
		t.Fatal(err)
	}
	<-b.in
	if w.srv.ActiveCircuits() != 1 {
		t.Fatal("no circuit")
	}
	clk.Advance(2 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if w.srv.ActiveCircuits() != 1 {
		t.Fatal("circuit ended before the idle time")
	}
	advanceUntil(t, clk, time.Minute, 20, func() bool { return w.srv.ActiveCircuits() == 0 })
}

// A stream that never sends its first frame is reset after ConnectTimeout of the server clock.
func TestFirstFrameTimeoutUsesServerClock(t *testing.T) {
	clk := newFakeClock(time.Now())
	w := newWorld(t, ServerConfig{Clock: clk, ConnectTimeout: 8 * time.Second})
	a := w.join(newTNode(t), "10.0.0.1", true)
	st, err := a.open(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.SetLimiter(limiter())
	done := make(chan error, 1)
	go func() { _, err := st.ReadFrame(); done <- err }()
	time.Sleep(100 * time.Millisecond)
	clk.Advance(9 * time.Second)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("silent stream not reset after ConnectTimeout on the server clock")
	}
}
