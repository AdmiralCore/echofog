package mux

import (
	"testing"
	"time"
)

// TestSessionActivityAndStreamCounts covers the two reads the session table needs to close
// idle sessions and to cap the number of sessions.
func TestSessionActivityAndStreamCounts(t *testing.T) {
	pr := sessionPair(t, false, nil)
	ctx := ctxT(t)

	if l, p := pr.a.StreamCounts(); l != 0 || p != 0 {
		t.Fatalf("fresh session streams = %d/%d, want 0/0", l, p)
	}
	before := pr.a.LastActivity()
	if before.IsZero() {
		t.Fatal("fresh session has no activity time")
	}
	time.Sleep(20 * time.Millisecond) // the platform clock tick is coarser than the work below

	st, err := pr.a.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := pr.b.AcceptStream(ctx); err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	waitFor(t, func() bool {
		l, p := pr.a.StreamCounts()
		return l == 1 && p == 0
	})
	if l, p := pr.b.StreamCounts(); l != 0 || p != 1 {
		t.Fatalf("peer streams = %d/%d, want 0/1", l, p)
	}
	if !pr.a.LastActivity().After(before) {
		t.Fatalf("opening a stream did not advance the activity time: before=%v after=%v", before, pr.a.LastActivity())
	}

	// A stream the peer opens counts on this side as a peer stream.
	back, err := pr.b.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("peer OpenStream: %v", err)
	}
	if _, err := pr.a.AcceptStream(ctx); err != nil {
		t.Fatalf("AcceptStream on A: %v", err)
	}
	waitFor(t, func() bool {
		l, p := pr.a.StreamCounts()
		return l == 1 && p == 1
	})

	_ = st.Close()
	_ = back.Close()
}

func TestDefaultTimers(t *testing.T) {
	d := DefaultTimers()
	if d.GoAwayDrain != 15*time.Second {
		t.Fatalf("GoAwayDrain = %v, want 15 s (wire.md §7.3)", d.GoAwayDrain)
	}
	if d.KeepaliveMin != 12*time.Second || d.KeepaliveMax != 24*time.Second {
		t.Fatalf("keepalive = %v..%v, want 12..24 s", d.KeepaliveMin, d.KeepaliveMax)
	}
	if d.PathDead != 60*time.Second {
		t.Fatalf("PathDead = %v, want 60 s", d.PathDead)
	}
}
