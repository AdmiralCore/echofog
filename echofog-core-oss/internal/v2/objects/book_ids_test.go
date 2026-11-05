package objects

import (
	"errors"
	"testing"
)

// TestCardBookMisbehavingIDs covers the read and the restore a persistent card book needs: the
// signers whose equivocation was observed are enumerable and can be marked again after a
// restart without replaying both objects.
func TestCardBookMisbehavingIDs(t *testing.T) {
	f := newFixture(t)
	book := NewCardBook()
	mk := func(seq uint64, release string) *Card {
		c := f.public(t)
		c.Release = release
		card, err := VerifyCard(sealBody(t, f.device, seq, fixedTime+6*day, c), vectorNow)
		if err != nil {
			t.Fatal(err)
		}
		return card
	}
	c1 := mk(fixedTime, "0.3.0")
	c2 := mk(fixedTime+5, "0.3.1")
	c3 := mk(fixedTime+5, "0.3.2")
	for _, c := range []*Card{c1, c2} {
		if _, err := book.Add(c); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if _, err := book.Add(c3); !errors.Is(err, ErrEquivocation) {
		t.Fatalf("Add c3 = %v, want ErrEquivocation", err)
	}
	ids := book.MisbehavingIDs()
	if len(ids) != 1 || ids[0] != c1.DeviceID {
		t.Fatalf("MisbehavingIDs = %x, want [%x]", ids, c1.DeviceID)
	}

	restored := NewCardBook()
	restored.MarkMisbehaving(ids[0])
	if !restored.Misbehaving(c1.DeviceID) {
		t.Fatal("the misbehaving marker was not restored")
	}
	if restored.Misbehaving(c2.NodeID) {
		t.Fatal("the account was marked although only the device equivocated")
	}
}
