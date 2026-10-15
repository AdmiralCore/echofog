package dht

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestValueLookupClaim(t *testing.T) {
	var self, a, b types.PeerID
	self[0], a[0], b[0] = 1, 2, 3
	l := newValueLookup(self)

	if l.claim(self) {
		t.Fatal("a lookup must never ask this node itself")
	}
	if !l.claim(a) || !l.claim(b) {
		t.Fatal("new peers are asked")
	}
	if l.claim(a) {
		t.Fatal("a peer is asked at most once per lookup, so peers pointing at each other cannot loop")
	}

	l = newValueLookup(self)
	asked := 0
	for i := 0; i < 1000; i++ {
		var p types.PeerID
		p[0], p[1] = byte(i), byte(i>>8)
		p[2] = 0xff
		if l.claim(p) {
			asked++
		}
	}
	if asked != maxLookupQueries {
		t.Fatalf("asked %d peers, want the budget %d", asked, maxLookupQueries)
	}
}
