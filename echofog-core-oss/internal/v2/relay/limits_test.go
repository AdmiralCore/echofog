package relay

import (
	"io"
	"testing"
	"time"
)

func readN(t *testing.T, c *Circuit, n int) {
	t.Helper()
	buf := make([]byte, n)
	_ = c.pipe.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c.pipe, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
}

// L8: 16 GiB per peer per day (lowered): the bytes a peer sends count across its circuits for the
// day and the budget comes back with the next day.
func TestPerPeerDayLimit(t *testing.T) {
	clk := newFakeClock(dayStart(testDay))
	w := newWorld(t, ServerConfig{Clock: clk, Limits: Limits{PerPeerDay: 3000, Idle: 24 * time.Hour}})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	chunk := make([]byte, 2000)

	circ, err := a.dial(b)
	if err != nil {
		t.Fatal(err)
	}
	in := <-b.in
	_, _ = circ.pipe.Write(chunk)
	readN(t, in.Stream, 2000)
	if w.srv.ActiveCircuits() != 1 {
		t.Fatal("circuit ended within the day budget")
	}
	_, _ = circ.pipe.Write(chunk) // 4000 > 3000
	waitFor(t, func() bool { return w.srv.ActiveCircuits() == 0 })

	// The same day, a new circuit of the same sender has only 1000 bytes left.
	circ, err = a.dial(b)
	if err != nil {
		t.Fatal(err)
	}
	<-b.in
	_, _ = circ.pipe.Write(chunk)
	waitFor(t, func() bool { return w.srv.ActiveCircuits() == 0 })

	// Next day: a full budget again.
	clk.Advance(24 * time.Hour)
	circ, err = a.dial(b)
	if err != nil {
		t.Fatal(err)
	}
	in = <-b.in
	_, _ = circ.pipe.Write(chunk)
	readN(t, in.Stream, 2000)
	if w.srv.ActiveCircuits() != 1 {
		t.Fatal("the daily budget did not come back")
	}
}

// L8: the default rate is 50 Mbit/s per peer, and a stream over it is paced on the server clock
// (not dropped).
func TestRateLimitPacesStreams(t *testing.T) {
	var d Limits
	d.fill()
	if d.RateBytes != 50_000_000/8 {
		t.Fatalf("default rate %d B/s, want 50 Mbit/s", d.RateBytes)
	}
	clk := newFakeClock(time.Now())
	w := newWorld(t, ServerConfig{Clock: clk, Limits: Limits{RateBytes: 10_000, Idle: 24 * time.Hour}})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	circ, err := a.dial(b)
	if err != nil {
		t.Fatal(err)
	}
	in := <-b.in
	// 25000 bytes at 10000 B/s with a one second burst: 15000 are paced.
	go func() { _, _ = circ.pipe.Write(make([]byte, 25_000)) }()
	got := make(chan struct{})
	go func() { readN(t, in.Stream, 25_000); close(got) }()
	select {
	case <-got:
		t.Fatal("paced stream arrived at once")
	case <-time.After(300 * time.Millisecond):
	}
	advanceUntil(t, clk, 500*time.Millisecond, 100, func() bool {
		select {
		case <-got:
			return true
		default:
			return false
		}
	})
}
