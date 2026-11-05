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
