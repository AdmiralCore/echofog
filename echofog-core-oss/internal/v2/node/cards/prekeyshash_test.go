package cards

import (
	"bytes"
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// The card carries the hash of the current prekeys/2 (objects.md 3); a new hash reissues the cards.
func TestSetPrekeysHashReissuesCards(t *testing.T) {
	m := newMaterial(t)
	b, err := New(baseConfig(t, m, newMapKV(), newFakeClock()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := b.Current(t.Context(), objects.ScopePeer)
	h := bytes.Repeat([]byte{7}, 32)
	if err := b.SetPrekeysHash(h); err != nil {
		t.Fatalf("SetPrekeysHash: %v", err)
	}
	after, err := b.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatal(err)
	}
	if after.Seq <= before.Seq {
		t.Fatalf("seq %d did not grow past %d", after.Seq, before.Seq)
	}
	card := verifyBytes(t, after.Envelope, b.clk.Now())
	if !bytes.Equal(card.Body.GetPrekeysHash(), h) {
		t.Fatalf("prekeys_hash = %x", card.Body.GetPrekeysHash())
	}
	// the same hash again is not a change
	if err := b.SetPrekeysHash(h); err != nil {
		t.Fatal(err)
	}
	same, _ := b.Current(t.Context(), objects.ScopePeer)
	if same.Seq != after.Seq {
		t.Fatalf("an unchanged hash reissued the card: %d -> %d", after.Seq, same.Seq)
	}
	// nil clears it, other lengths are refused
	if err := b.SetPrekeysHash(nil); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPrekeysHash([]byte{1, 2, 3}); !errors.Is(err, ErrConfig) {
		t.Fatalf("a hash of 3 bytes: %v", err)
	}
}
