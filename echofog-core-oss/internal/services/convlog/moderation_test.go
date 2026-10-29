package convlog

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

// forgeAt is an event of a by hand, numbered and stamped as its author likes.
func forgeAt(t *testing.T, a account, c Conv, seq, lamport uint64, tsMs int64, text string) []byte {
	t.Helper()
	body, _ := proto.Marshal(&MsgBody{Text: text})
	e := &Event{Conv: c.ID[:], Author: a.pub, Seq: seq, Lamport: lamport, TsMs: tsMs, Kind: KindMsg, Body: body}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(a, m), nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A member in a timeout stays in the group but what they write meanwhile is refused everywhere,
// placed before the timeout or not; once it ends they write again. Lifting it early keeps refused
// what came meanwhile. Only who may remove members times out, and only a lower rank.
func TestTimeouts(t *testing.T) {
	f, m, x := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("to1")
	lf, lm, lx := groupReplica(t, f, rules), groupReplica(t, m, rules), groupReplica(t, x, rules)
	all := []*Log{lf, lm, lx}
	spread := func(raw []byte) {
		for _, l := range all {
			_, _, _ = l.Ingest(c, raw)
		}
	}
	spread(member(t, lf, c, roster(f, "owner", m, "moderator", x, "member")))
	before, _ := say(t, lx, c, "before")
	spread(before)
	until := time.Now().Add(10 * time.Minute).Unix()
	if _, _, err := lx.AppendKept(c, Draft{Kind: KindMember, Body: mustBody(t, &MemberBody{Action: MemberTimeout, Member: m.pub, Until: until})}); err != nil {
		t.Fatal(err)
	}
	spread(member(t, lm, c, &MemberBody{Action: MemberTimeout, Member: x.pub, Until: until, UptoSeq: lm.AuthorSeq(c, x.id)}))
	if _, _, err := lx.AppendMessage(c, "during", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a member in a timeout wrote: %v", err)
	}
	if r := lx.RightsOf(c, x.id); r.Send || r.React || !r.Member {
		t.Fatalf("rights in a timeout: %+v", r)
	}
	if lf.TimeoutOf(c, x.id) != until {
		t.Fatal("the timeout's end is not known")
	}
	// written by hand, placed before the timeout in the log's order
	sneak := forgeAt(t, x, c, 10, 1, time.Now().UnixMilli(), "sneaked in")
	spread(sneak)
	for _, l := range all {
		if texts(t, l, c)["sneaked in"] {
			t.Fatal("a message written during the timeout was let in")
		}
		if !texts(t, l, c)["before"] || !l.Participant(c, x.id) {
			t.Fatal("the timeout took what came before, or the member")
		}
	}
	// the moderator does not time out the owner, and a member nobody
	spread(member(t, lm, c, &MemberBody{Action: MemberTimeout, Member: f.pub, Until: until}))
	if lf.TimeoutOf(c, f.id) != 0 {
		t.Fatal("a moderator timed out the owner")
	}
	if lf.TimeoutOf(c, m.id) != 0 {
		t.Fatal("a member timed out a moderator")
	}
	// lifted: they write again; what came meanwhile stays out
	spread(member(t, lm, c, &MemberBody{Action: MemberTimeout, Member: x.pub, Until: time.Now().Unix() - 1, UptoSeq: lm.AuthorSeq(c, x.id)}))
	after, _ := say(t, lx, c, "after")
	spread(after)
	for _, l := range all {
		got := texts(t, l, c)
		if !got["after"] || got["sneaked in"] {
			t.Fatalf("after the lift: %v", got)
		}
	}
	// the moderation log has it
	var acts []string
	for _, e := range lf.ModLog(c, 0) {
		acts = append(acts, e.Action)
	}
	if len(acts) < 3 || acts[0] != "timeoutLifted" || acts[1] != "timeout" || acts[len(acts)-1] != "roster" {
		t.Fatalf("moderation log: %v", acts)
	}
}

func mustBody(t *testing.T, b *MemberBody) []byte {
	t.Helper()
	raw, err := proto.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// In a private group a member's own join counts only with an invitation that lets them in by
// itself; one that asks for approval does not, and neither does none at all. Who may let members
// in still admits the newcomer.
func TestSelfJoinTakesAnInvitation(t *testing.T) {
	f, s1, s2, s3 := newAccount(t), newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id, private: true}
	c := Group("sj1")
	lf := groupReplica(t, f, rules)
	member(t, lf, c, roster(f, "owner"))
	exp := time.Now().Add(time.Hour).Unix()
	if _, _, err := lf.AppendInvite(c, &InviteDef{N: nonce(5), Exp: exp}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lf.AppendInvite(c, &InviteDef{N: nonce(6), Exp: exp, Approval: true}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		a   account
		via string
		in  bool
	}{{s1, "", false}, {s2, nonce(6), false}, {s3, nonce(5), true}} {
		l := groupReplica(t, tc.a, rules)
		for _, raw := range eventsOf(t, lf, c) {
			_, _, _ = l.Ingest(c, raw)
		}
		raw := member(t, l, c, &MemberBody{Action: MemberJoin, Member: tc.a.pub, Via: tc.via})
		_, _, _ = lf.Ingest(c, raw)
		if lf.Participant(c, tc.a.id) != tc.in {
			t.Fatalf("self-join via %q: in %v, want %v", tc.via, !tc.in, tc.in)
		}
	}
	// admitted by the owner, with the approval invitation: in
	member(t, lf, c, &MemberBody{Action: MemberJoin, Member: s2.pub, Via: nonce(6)})
	if !lf.Participant(c, s2.id) {
		t.Fatal("the owner could not admit with an approval invitation")
	}
}

// eventsOf is every event l has of c, for a newcomer's replica.
func eventsOf(t *testing.T, l *Log, c Conv) [][]byte {
	t.Helper()
	var out [][]byte
	for _, en := range mustFind(t, l, memberPrefix(l.bc(c.ID))) {
		out = append(out, en)
	}
	return out
}

func mustFind(t *testing.T, l *Log, prefix []byte) [][]byte {
	t.Helper()
	entries, err := l.st.FindValues(prefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Value)
	}
	return out
}
