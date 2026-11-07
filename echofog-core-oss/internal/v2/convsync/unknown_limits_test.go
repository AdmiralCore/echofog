package convsync

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// An event of an unknown kind is held to the rights of a known one: a reader, a member in a timeout
// or one writing into an announcements channel gets no stub into anybody's log.
func TestUnknownKindNeedsTheRightToSend(t *testing.T) {
	c := newCluster(t, 4, 4)
	c.rosterOf(4, map[int]string{3: "subscriber"})
	reader := c.members[3]
	if _, _, err := reader.e.Log().Append(c.group, convlog.Draft{Kind: 77, Body: []byte("x")}); err == nil {
		t.Fatal("a reader wrote an event of an unknown kind")
	}
	forged, _, err := reader.e.Log().AppendKept(c.group, convlog.Draft{Kind: 77, Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.e.Publish(context.Background(), c.group, forged)
	c.n.drain(t)
	c.syncAll(4)
	for i := 0; i < 3; i++ {
		if s := c.members[i].e.Stubs(c.group); len(s) != 0 {
			t.Fatalf("member %d shows %d stubs of a reader's event", i, len(s))
		}
	}
	// a member who may write still gets theirs through
	raw, _, err := c.members[1].e.Log().Append(c.group, convlog.Draft{Kind: 77, Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.members[1].e.Publish(context.Background(), c.group, raw)
	c.n.drain(t)
	if s := c.members[2].e.Stubs(c.group); len(s) != 1 {
		t.Fatalf("a member's event: %d stubs", len(s))
	}
}

func withUnknown(raw []byte, n int) []byte {
	out := append([]byte(nil), raw...)
	out = append(out, 0xfa, 0x06) // field 111, length-delimited
	l := n
	for l >= 0x80 {
		out = append(out, byte(l)|0x80)
		l >>= 7
	}
	out = append(out, byte(l))
	return append(out, make([]byte, n)...)
}

// The unknown fields of an event are not signed, so a relay can append any amount: past a small
// ceiling the event is refused whole, and the real copy that follows is taken.
func TestUnknownFieldsAreCapped(t *testing.T) {
	c := newCluster(t, 2, 2)
	c.rosterOf(2, nil)
	a, b := c.members[0], c.members[1]
	raw, _, err := a.e.Log().Append(c.group, convlog.Draft{Kind: 77, Body: []byte("real")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b.e.Receive(ctx, a.a.node, &chatpb.PersonalMessageProto{ConvEvent: withUnknown(raw, 60<<10)})
	if got := b.e.Stubs(c.group); len(got) != 0 {
		t.Fatal("an event with 60 KB of unknown fields was kept")
	}
	b.e.Receive(ctx, a.a.node, &chatpb.PersonalMessageProto{ConvEvent: withUnknown(raw, convlog.MaxUnknownFields+1)})
	if got := b.e.Stubs(c.group); len(got) != 0 {
		t.Fatal("an event just over the ceiling was kept")
	}
	b.e.Receive(ctx, a.a.node, &chatpb.PersonalMessageProto{ConvEvent: raw})
	got := b.e.Log().Export(c.group, 1<<20)
	var kept bool
	for _, r := range got {
		if string(r) == string(raw) {
			kept = true
		}
		if len(r) > len(raw)+200 && len(r) > 1<<10 {
			t.Fatalf("a bloated copy is held: %d bytes", len(r))
		}
	}
	if !kept {
		t.Fatal("the real copy was not taken")
	}
	// a little unknown data is fine
	raw2, _, err := a.e.Log().Append(c.group, convlog.Draft{Kind: 78, Body: []byte("real2")})
	if err != nil {
		t.Fatal(err)
	}
	b.e.Receive(ctx, a.a.node, &chatpb.PersonalMessageProto{ConvEvent: withUnknown(raw2, 500)})
	if len(b.e.Stubs(c.group)) != 2 {
		t.Fatal("an event with a few unknown fields was refused")
	}
}

// One author cannot fill a conversation with events of kinds nobody can read.
func TestUnknownKindsHaveAQuotaPerAuthor(t *testing.T) {
	c := newCluster(t, 2, 2)
	c.rosterOf(2, nil)
	a, b := c.members[0], c.members[1]
	for i := 0; i < convlog.MaxUnknownPerAuthor+5; i++ {
		raw, _, err := a.e.Log().Append(c.group, convlog.Draft{Kind: 77, Body: []byte("x")})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _ = b.e.Log().Ingest(c.group, raw)
	}
	if n := len(b.e.Stubs(c.group)); n != convlog.MaxUnknownPerAuthor {
		t.Fatalf("b holds %d events of unknown kinds, want %d", n, convlog.MaxUnknownPerAuthor)
	}
}

// The record of events already passed on holds hashes, not the events.
func TestPassedKeysAreHashes(t *testing.T) {
	size := MeshLimit + 10
	c := newCluster(t, size, size)
	c.rosterOf(size, nil)
	c.catchUp(size)
	c.say(1, "to a large group")
	c.n.drain(t)
	var seen int
	for _, m := range c.members {
		m.e.mu.Lock()
		for k := range m.e.passed {
			seen++
			if len(k) != 32 {
				t.Errorf("a key of %d bytes", len(k))
			}
		}
		m.e.mu.Unlock()
	}
	if seen == 0 {
		t.Fatal("nobody passed anything on")
	}
}
