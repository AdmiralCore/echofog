package cards

import (
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestSetPublicHostReissuesCardsOfFollowingListeners(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Listeners[0].FollowPublic = true
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before, err := b.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}

	n, err := b.SetPublicHost("203.0.113.5")
	if err != nil || n != 1 {
		t.Fatalf("SetPublicHost = %d, %v, want 1", n, err)
	}
	after, err := b.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current after: %v", err)
	}
	if after.Seq <= before.Seq {
		t.Fatalf("seq %d did not grow past %d", after.Seq, before.Seq)
	}
	card := verifyBytes(t, after.Envelope, b.clk.Now())
	if got := card.Body.GetAddrs()[0].GetHost(); got != "203.0.113.5" {
		t.Fatalf("card host = %q", got)
	}

	// the same address again is not a change
	n, err = b.SetPublicHost("203.0.113.5")
	if err != nil || n != 0 {
		t.Fatalf("repeat SetPublicHost = %d, %v, want 0", n, err)
	}
	again, _ := b.Current(t.Context(), objects.ScopePeer)
	if again.Seq != after.Seq {
		t.Fatalf("a repeated address reissued the card")
	}
}

func TestSetPublicHostLeavesFixedListenersAlone(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	want := cfg.Listeners[0].Host
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n, err := b.SetPublicHost("203.0.113.5")
	if err != nil || n != 0 {
		t.Fatalf("SetPublicHost = %d, %v, want 0", n, err)
	}
	cur, _ := b.Current(t.Context(), objects.ScopePeer)
	card := verifyBytes(t, cur.Envelope, b.clk.Now())
	if got := card.Body.GetAddrs()[0].GetHost(); got != want {
		t.Fatalf("card host = %q, want %q", got, want)
	}
}

func TestSetPublicHostRejectsEmpty(t *testing.T) {
	b, err := New(baseConfig(t, newMaterial(t), newMapKV(), newFakeClock()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.SetPublicHost(""); !errors.Is(err, ErrConfig) {
		t.Fatalf("SetPublicHost(\"\") = %v, want ErrConfig", err)
	}
}

func TestSetPublicPortReissuesCardsOfMatchingFollowers(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Listeners[0].FollowPublic = true
	port := cfg.Listeners[0].Port
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before, _ := b.Current(t.Context(), objects.ScopePeer)

	// a TCP mapping of the same number does not touch a UDP suite
	if n, err := b.SetPublicPort(false, port, 50000); err != nil || n != 0 {
		t.Fatalf("tcp SetPublicPort = %d, %v, want 0", n, err)
	}
	// another internal port is not ours
	if n, err := b.SetPublicPort(true, port+1, 50000); err != nil || n != 0 {
		t.Fatalf("foreign port SetPublicPort = %d, %v, want 0", n, err)
	}
	n, err := b.SetPublicPort(true, port, 50000)
	if err != nil || n != 1 {
		t.Fatalf("SetPublicPort = %d, %v, want 1", n, err)
	}
	after, _ := b.Current(t.Context(), objects.ScopePeer)
	if after.Seq <= before.Seq {
		t.Fatalf("seq %d did not grow past %d", after.Seq, before.Seq)
	}
	if got := verifyBytes(t, after.Envelope, b.clk.Now()).Body.GetAddrs()[0].GetPort(); got != 50000 {
		t.Fatalf("card port = %d", got)
	}
	if n, _ := b.SetPublicPort(true, port, 50000); n != 0 {
		t.Fatal("a repeated mapping counted as a change")
	}

	// the lookup keeps working after the card moved to the mapped port, and 0 restores
	n, err = b.SetPublicPort(true, port, 0)
	if err != nil || n != 1 {
		t.Fatalf("reset = %d, %v, want 1", n, err)
	}
	reset, _ := b.Current(t.Context(), objects.ScopePeer)
	if got := verifyBytes(t, reset.Envelope, b.clk.Now()).Body.GetAddrs()[0].GetPort(); got != port {
		t.Fatalf("card port after reset = %d, want %d", got, port)
	}
}

func TestSetPublicPortRejectsBadPortsAndLeavesFixedListenersAlone(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	port := cfg.Listeners[0].Port
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n, err := b.SetPublicPort(true, port, 50000); err != nil || n != 0 {
		t.Fatalf("fixed listener followed a mapping: %d, %v", n, err)
	}
	cfg.Listeners[0].FollowPublic = true
	b, _ = New(cfg)
	if _, err := b.SetPublicPort(true, port, 70000); !errors.Is(err, ErrConfig) {
		t.Fatalf("port 70000: %v, want ErrConfig", err)
	}
}
