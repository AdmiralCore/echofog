package cards

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestInviteOnlyTheseListeners pins the explicit decision on the T4.2a Medium risk: an invitation
// card can replace the node's listeners instead of adding to them, so it never carries the contacts
// masking key or the contacts listener (objects.md §10.3).
func TestInviteOnlyTheseListeners(t *testing.T) {
	clk := newFakeClock()
	m := newMaterial(t)
	b, err := New(baseConfig(t, m, nil, clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	extra := Listener{
		Suite: objects.SuiteNoise3,
		Host:  "198.51.100.20",
		Port:  4242,
		Mask:  newMask(t),
	}
	id := make([]byte, inviteIDLen)

	inv, err := b.IssueInvite(t.Context(), InviteOptions{InviteID: id, Listeners: []Listener{extra}, OnlyThese: true})
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}
	c := verifyBytes(t, inv.Envelope, clk.Now())
	if len(c.Body.GetAddrs()) != 1 || len(c.Body.GetMaskKeys()) != 1 {
		t.Fatalf("%d addrs, %d mask keys", len(c.Body.GetAddrs()), len(c.Body.GetMaskKeys()))
	}
	if a := c.Body.GetAddrs()[0]; a.GetHost() != extra.Host || a.GetPort() != extra.Port {
		t.Fatalf("address %+v", a)
	}
	if k := c.Body.GetMaskKeys()[0]; string(k.GetKey()) != string(extra.Mask) {
		t.Fatal("the invitation key is not the only masking key")
	}

	// Without OnlyThese the node's own listeners are added, which is the old behaviour.
	inv, err = b.IssueInvite(t.Context(), InviteOptions{InviteID: id, Listeners: []Listener{extra}})
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}
	c = verifyBytes(t, inv.Envelope, clk.Now())
	if len(c.Body.GetAddrs()) != 2 {
		t.Fatalf("%d addrs without OnlyThese", len(c.Body.GetAddrs()))
	}
}
