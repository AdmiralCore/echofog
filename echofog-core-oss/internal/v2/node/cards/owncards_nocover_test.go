package cards

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestOwnCardsWithoutCoverOmitsPublic pins D37 on the card side: a node configured as visible but
// without a camouflaged listener has no public card. It must still answer handshakes with its peer
// card and keep the issued-card memory, not fail the whole OwnCards call.
func TestOwnCardsWithoutCoverOmitsPublic(t *testing.T) {
	m := newMaterial(t)
	cfg := baseConfig(t, m, newMapKV(), newFakeClock())
	cfg.Listeners[0].Suite = objects.SuiteNoise3
	cfg.Listeners[0].SNI = ""

	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Visibility() != Visible {
		t.Fatalf("visibility %v, want visible", b.Visibility())
	}

	own, err := b.OwnCards(context.Background())
	if err != nil {
		t.Fatalf("OwnCards without a cover: %v", err)
	}
	if _, ok := own.Current[uint32(objects.ScopePublic)]; ok {
		t.Fatal("OwnCards published a public card without a camouflaged listener")
	}
	peer, ok := own.Current[uint32(objects.ScopePeer)]
	if !ok || len(peer.Envelope) == 0 {
		t.Fatal("OwnCards lost the peer card")
	}
	if _, ok := own.Issued[peer.Seq]; !ok {
		t.Fatal("OwnCards lost the issued-card memory of the peer card")
	}
}
