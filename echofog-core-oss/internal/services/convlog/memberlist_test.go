package convlog

import "testing"

// Members lists who is in by the log, with the keys the events name, the owner first; Banned lists the bans.
func TestMemberListFollowsTheLog(t *testing.T) {
	f, a, x := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("list1")
	lf := groupReplica(t, f, rules)
	if got := lf.Members(c); got != nil {
		t.Fatalf("a group without a roster has members: %+v", got)
	}
	member(t, lf, c, roster(f, "owner", a, "admin", x, "member"))

	got := lf.Members(c)
	if len(got) != 3 || got[0].Peer != f.id || got[0].Role.Name != "owner" || got[1].Peer != a.id {
		t.Fatalf("members %+v", got)
	}
	for _, m := range got {
		if m.Pub == ([32]byte{}) {
			t.Fatalf("the roster names the keys: %+v", m)
		}
	}
	member(t, lf, c, &MemberBody{Action: MemberBan, Member: x.pub})
	if got := lf.Members(c); len(got) != 2 {
		t.Fatalf("after the ban: %+v", got)
	}
	if b := lf.Banned(c); len(b) != 1 || b[0].Peer != x.id || b[0].Pub != ([32]byte)(x.pub) {
		t.Fatalf("bans %+v", b)
	}
	if lf.Members(DM(f.id, a.id)) != nil || lf.Banned(DM(f.id, a.id)) != nil {
		t.Fatal("a direct chat has no members")
	}
}
