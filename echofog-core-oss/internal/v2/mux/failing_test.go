package mux

import (
	"sync/atomic"
	"testing"
	"time"
)

// A session that is left with one failing path says so as soon as the path is declared failing,
// so the layer above can repair it long before the path is dead (PathDead, a minute by default).

func TestOnlyPathFailingIsReported(t *testing.T) {
	pr := fastPair(t, nil)
	var got atomic.Int64
	var cause atomic.Value
	pr.a.SetOnPathFailing(func(f PathFailure) {
		got.Add(1)
		cause.Store(f.Cause)
	})
	st, err := pr.a.OpenStream(ctxT(t), 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	pr.path0.tb.frozen.Store(true)
	st.WriteFrame(0x40, 0, deliver(1, "nobody answers"))
	start := time.Now()
	waitFor(t, func() bool { return got.Load() == 1 })
	if d := time.Since(start); d > 4*fastTimers().FailSilence {
		t.Fatalf("reported after %v", d)
	}
	if c, _ := cause.Load().(error); c == nil {
		t.Fatal("the report carries no cause")
	}
	time.Sleep(2 * fastTimers().FailSilence)
	if got.Load() != 1 {
		t.Fatalf("reported %d times for one episode", got.Load())
	}
}

func TestFailingPathWithASpareIsNotReported(t *testing.T) {
	pr := fastPair(t, nil)
	bindNewPath(t, pr, false)
	var got atomic.Int64
	pr.a.SetOnPathFailing(func(PathFailure) { got.Add(1) })
	pr.path0.tb.frozen.Store(true)
	waitFor(t, func() bool { return activeOf(pr.a) == 2 })
	time.Sleep(2 * fastTimers().FailSilence)
	if got.Load() != 0 {
		t.Fatalf("reported %d times although a healthy path took over", got.Load())
	}
}

func TestReportedAgainAfterARecoveredEpisode(t *testing.T) {
	pr := fastPair(t, func(a, b *Config) {
		for _, c := range []*Config{a, b} {
			c.Timers.PathDead = 30 * time.Second
		}
	})
	var got atomic.Int64
	pr.a.SetOnPathFailing(func(PathFailure) { got.Add(1) })
	for episode := int64(1); episode <= 2; episode++ {
		st, err := pr.a.OpenStream(ctxT(t), 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		pr.path0.tb.frozen.Store(true)
		st.WriteFrame(0x40, 0, deliver(1, "nobody answers"))
		waitFor(t, func() bool { return got.Load() == episode })
		pr.path0.tb.frozen.Store(false)
		waitFor(t, func() bool { return !isFailing(pr.a, 0) })
	}
}
