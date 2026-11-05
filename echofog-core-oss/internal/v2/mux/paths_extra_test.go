package mux

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// countPadding counts complete PADDING frames in a captured carrier stream. A trailing partial
// frame (the writer was still busy) ends the count.
func countPadding(b []byte) int {
	n := 0
	for len(b) > 0 {
		f, used, err := frame.ParseFrame(b, nil)
		if err != nil {
			return n
		}
		b = b[used:]
		switch f.Type {
		case frame.TypePadding:
			n++
		case frame.TypeMuxStream:
			if m, merr := frame.ParseMuxStream(f.Payload); merr == nil {
				n += countPadding(m.Data)
			}
		}
	}
	return n
}

// TestIdlePaddingFollowsPolicy pins wire.md §6.4, §9 and registries.md §5: with the feature
// negotiated, the sender emits PADDING-only frames when the active path is idle, at the gap drawn
// from its shape policy.
func TestIdlePaddingFollowsPolicy(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) {
		for _, c := range []*Config{a, b} {
			c.Features = append(c.Features, frame.FeatureIdlePadding)
			c.Timers = fastTimers()
			c.Timers.Tick = 2 * time.Millisecond
			c.Timers.KeepaliveMin, c.Timers.KeepaliveMax = 30*time.Second, 30*time.Second
			c.Timers.PathDead = 30 * time.Second
		}
		a.IdlePad = func() (time.Duration, int, bool) { return 5 * time.Millisecond, 16, true }
	})
	waitFor(t, func() bool { return countPadding(pr.path0.ta.Bytes()) > 0 })
}

// TestIdlePaddingNeedsTheFeature: a sender policy alone is not enough; without the negotiated
// feature there are no idle PADDING frames.
func TestIdlePaddingNeedsTheFeature(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) {
		for _, c := range []*Config{a, b} {
			c.Timers = fastTimers()
			c.Timers.Tick = 2 * time.Millisecond
			c.Timers.KeepaliveMin, c.Timers.KeepaliveMax = 30*time.Second, 30*time.Second
			c.Timers.PathDead = 30 * time.Second
		}
		a.IdlePad = func() (time.Duration, int, bool) { return time.Millisecond, 16, true }
	})
	time.Sleep(100 * time.Millisecond)
	if n := countPadding(pr.path0.ta.Bytes()); n != 0 {
		t.Fatalf("%d idle PADDING frames without the idle-padding feature", n)
	}
}

// TestPathFailureCallback pins the hook the path policy needs: a path that dies reports its ID,
// kind and cause; a path abandoned on purpose is distinguishable from a broken one (wire.md §8.3).
func TestPathFailureCallback(t *testing.T) {
	failed := make(chan PathFailure, 4)
	pr := fastPair(t, func(a, b *Config) {
		a.OnPathFailed = func(f PathFailure) { failed <- f }
	})
	pr.path0.ca.Close()
	select {
	case f := <-failed:
		if f.PathID != 0 || !errors.Is(f.Cause, ErrPathFailed) {
			t.Fatalf("failure %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no path-failure callback for a broken carrier")
	}

	pr2 := fastPair(t, func(a, b *Config) {
		a.OnPathFailed = func(f PathFailure) { failed <- f }
	})
	id, _ := bindNewPath(t, pr2, false)
	if err := pr2.a.AbandonPath(id, frame.CodeNoError); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-failed:
		if f.PathID != id || !errors.Is(f.Cause, ErrPathAbandoned) {
			t.Fatalf("abandon %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no path-failure callback for an abandoned path")
	}
}

// TestRelayPathAlwaysSealed pins wire.md §8.4: relay paths carry sealed records even when the
// fake circuit reports a classical (not sealed) carrier.
func TestRelayPathAlwaysSealed(t *testing.T) {
	pr := sessionPair(t, false, nil)
	e := newCarrier(t, false)
	ctx := ctxT(t)
	ch := make(chan error, 1)
	go func() { _, err := pr.a.BindPath(ctx, e.ga, PathRelay); ch <- err }()
	if _, _, err := pr.binder.Accept(ctx, e.gb, PathRelay); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	st, err := pr.a.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	st.WriteFrame(0x40, 0, deliver(1, "RELAY-SECRET"))
	rs, err := pr.b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := rs.ReadFrame(); err != nil || !bytes.Contains(f.Payload, []byte("RELAY-SECRET")) {
		t.Fatalf("relay stream: %+v %v", f, err)
	}
	if bytes.Contains(e.ta.Bytes(), []byte("RELAY-SECRET")) {
		t.Fatal("plaintext on a relay path")
	}
}
