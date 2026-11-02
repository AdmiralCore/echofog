package convlog

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

// A stranger's membership event that is refused must not vouch for its own author: nothing the
// stranger writes afterwards may be kept in a group that takes an invitation to join.
func TestRefusedSelfJoinDoesNotVouchForTheStranger(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id, private: true}
	c := Group("probe")
	lf := groupReplica(t, f, rules)
	member(t, lf, c, roster(f, "owner"))
	body, _ := proto.Marshal(&MemberBody{Action: MemberJoin, Member: x.pub, Role: "member"})
	e := &Event{Conv: c.ID[:], Author: x.pub, Seq: 1, Lamport: 5, TsMs: time.Now().UnixMilli(), Kind: KindMember, Body: body}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(x, m), nil }); err != nil {
		t.Fatal(err)
	}
	join, _ := Encode(e)
	_, _, jerr := lf.Ingest(c, join)
	msg := forgeAt(t, x, c, 2, 6, time.Now().UnixMilli(), "stranger text")
	_, _, merr := lf.Ingest(c, msg)
	t.Logf("join: %v, message: %v, author seq kept: %d", jerr, merr, lf.AuthorSeq(c, x.id))
	if lf.AuthorSeq(c, x.id) != 0 {
		t.Fatalf("a stranger's refused events are kept (seq %d)", lf.AuthorSeq(c, x.id))
	}
}

func TestStrangerMessageAloneIsNotKept(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	c := Group("probe2")
	lf := groupReplica(t, f, testRules{founder: f.id, private: true})
	member(t, lf, c, roster(f, "owner"))
	_, _, _ = lf.Ingest(c, forgeAt(t, x, c, 1, 6, time.Now().UnixMilli(), "stranger text"))
	if lf.AuthorSeq(c, x.id) != 0 {
		t.Fatalf("kept: seq %d", lf.AuthorSeq(c, x.id))
	}
}

// A newcomer with an invitation whose join and first message arrive before the invitation itself
// (they wrote at once) is still let in once the invitation comes, and nothing they wrote is lost.
func TestNewcomerBeforeTheirInvitationIsLetIn(t *testing.T) {
	f, n := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id, private: true}
	c := Group("probe3")
	lf := groupReplica(t, f, rules)
	r := member(t, lf, c, roster(f, "owner"))
	inv, _, err := lf.AppendInvite(c, &InviteDef{N: strings.Repeat("ab", 16), Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := proto.Marshal(&MemberBody{Action: MemberJoin, Member: n.pub, Via: strings.Repeat("ab", 16)})
	e := &Event{Conv: c.ID[:], Author: n.pub, Seq: 1, Lamport: 10, TsMs: time.Now().UnixMilli(), Kind: KindMember, Body: body}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(n, m), nil }); err != nil {
		t.Fatal(err)
	}
	join, _ := Encode(e)
	hello := forgeAt(t, n, c, 2, 11, time.Now().UnixMilli(), "hi all")
	l := groupReplica(t, newAccount(t), rules)
	ingestAll(l, c, [][]byte{join, hello, r, inv}) // twice over, as sync brings what was refused again
	l.RebuildIfNeeded(c)
	if !texts(t, l, c)["hi all"] || !l.Participant(c, n.id) {
		t.Fatalf("the newcomer is not in, or lost their first message: %v", texts(t, l, c))
	}
}
