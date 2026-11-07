package convlog

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// @all takes the right to write it; a message that has it without the right is refused the same way
// by everybody (it is checked at the point of the event, like the other rights).
func TestMentionAllNeedsTheRight(t *testing.T) {
	f, x, m, y := newAccount(t), newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("mentions")
	lf, lx, lm, ly := groupReplica(t, f, rules), groupReplica(t, x, rules), groupReplica(t, m, rules), groupReplica(t, y, rules)
	r := member(t, lf, c, roster(f, "owner", x, "member", m, "moderator", y, "member"))
	ingest(t, lx, c, r)
	ingest(t, lm, c, r)
	ingest(t, ly, c, r)

	if _, _, err := lx.AppendMessageMentioning(c, "", "hey all", Mentions{All: true}, EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a member wrote @all: %v", err)
	}
	for name, l := range map[string]*Log{"owner": lf, "moderator": lm} {
		if _, _, err := l.AppendMessageMentioning(c, "", "hey all", Mentions{All: true}, EventID{}, EventID{}); err != nil {
			t.Fatalf("the %s cannot write @all: %v", name, err)
		}
	}
	// a member makes the event by hand: everybody refuses it, none shows it
	forged, id, err := lx.AppendKept(c, Draft{Kind: KindMsg, Body: encodeBody(&MsgBody{Text: "forged", All: true})})
	if err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]*Log{"owner": lf, "other member": ly} {
		if _, _, err := l.Ingest(c, forged); err != ErrForbidden {
			t.Fatalf("%s took a forged @all: %v", name, err)
		}
		if _, ok := l.Message(c, id); ok {
			t.Fatalf("%s shows a forged @all", name)
		}
	}
}

// A message that names me is marked, counted apart from the unread ones and cleared by reading.
func TestMentionsAreMarkedAndCountedApart(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("mentions2")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	ingest(t, lx, c, member(t, lf, c, roster(f, "owner", x, "member")))
	talk, _, err := lf.AppendChannel(c, ChannelSpec{ID: "talk", Name: "talk"})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, talk)

	var last EventID
	for _, w := range []struct {
		text string
		m    Mentions
	}{
		{"named", Mentions{Who: []types.PeerID{f.id}}},
		{"plain", Mentions{}},
		{"everybody", Mentions{All: true}},
		{"somebody else", Mentions{Who: []types.PeerID{x.id}}},
	} {
		raw, id, err := lx.AppendMessageMentioning(c, "", w.text, w.m, EventID{}, EventID{})
		if err != nil {
			t.Fatal(err)
		}
		ingest(t, lf, c, raw)
		last = id
	}
	raw, _, err := lx.AppendMessageMentioning(c, "talk", "in the channel", Mentions{Who: []types.PeerID{f.id}}, EventID{}, EventID{})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lf, c, raw)

	meta, _ := lf.Meta(c)
	if meta.Unread != 4 || meta.Mentions != 2 {
		t.Fatalf("main history: unread %d mentions %d, want 4 and 2", meta.Unread, meta.Mentions)
	}
	if st := meta.Chans["talk"]; st == nil || st.Unread != 1 || st.Mentions != 1 {
		t.Fatalf("channel: %+v", st)
	}
	msgs, _, _ := lf.History(c, "", 10)
	got := map[string]bool{}
	for _, m := range msgs {
		got[m.Text] = m.Mentioned
	}
	if !got["named"] || got["plain"] || !got["everybody"] || got["somebody else"] {
		t.Fatalf("marks: %v", got)
	}
	// the author is never told they were named by their own @all
	own, _, _ := lx.History(c, "", 10)
	for _, m := range own {
		if m.Mentioned {
			t.Fatalf("my own message %q marks me", m.Text)
		}
	}
	if err := lf.MarkRead(c, last); err != nil {
		t.Fatal(err)
	}
	if meta, _ = lf.Meta(c); meta.Unread != 0 || meta.Mentions != 0 {
		t.Fatalf("after reading: unread %d mentions %d", meta.Unread, meta.Mentions)
	}
}

// The list of names is bounded and each name is a whole peer id.
func TestMentionListIsBounded(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("mentions3")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	ingest(t, lx, c, member(t, lf, c, roster(f, "owner", x, "member")))
	var many []types.PeerID
	for i := 0; i <= MaxMentions; i++ {
		var id types.PeerID
		id[0], id[1] = byte(i), 1
		many = append(many, id)
	}
	if _, _, err := lx.AppendMessageMentioning(c, "", "too many", Mentions{Who: many}, EventID{}, EventID{}); err != ErrBadBody {
		t.Fatalf("%d names: %v", len(many), err)
	}
	if _, _, err := lx.AppendMessageMentioning(c, "", "ok", Mentions{Who: many[:MaxMentions]}, EventID{}, EventID{}); err != nil {
		t.Fatalf("%d names: %v", MaxMentions, err)
	}
	// by hand: a name that is not 32 bytes, and a list that is too long
	for name, body := range map[string]*MsgBody{
		"short name": {Text: "x", Mentions: [][]byte{make([]byte, 31)}},
		"long list":  {Text: "x", Mentions: make([][]byte, MaxMentions+1)},
	} {
		if name == "long list" {
			for i := range body.Mentions {
				body.Mentions[i] = make([]byte, 32)
			}
		}
		raw, _, err := lx.AppendKept(c, Draft{Kind: KindMsg, Body: encodeBody(body)})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := lf.Ingest(c, raw); err != ErrBadBody {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
