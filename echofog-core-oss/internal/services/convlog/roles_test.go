package convlog

import (
	"strings"
	"testing"
	"time"
)

func rightsIn(l *Log, c Conv, a account) Rights { return l.RightsOf(c, a.id) }

// A custom role is defined on the log and given like a built-in one; every replica gives its
// members the same rights. Nobody defines a role at or above their own rank, grants a permission
// they do not hold, or changes a role at or above their rank, even by writing the event by hand.
func TestCustomRolesAndTheRankGuard(t *testing.T) {
	f, a, x := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("roles1")
	lf, la, lx := groupReplica(t, f, rules), groupReplica(t, a, rules), groupReplica(t, x, rules)
	all := []*Log{lf, la, lx}
	spread := func(raw []byte) {
		for _, l := range all {
			_, _, _ = l.Ingest(c, raw)
		}
	}
	spread(member(t, lf, c, roster(f, "owner", a, "admin", x, "member")))

	def, _, err := lf.AppendRole(c, &RoleDef{Id: "helper", Title: "Helper", Color: 0x33aa55, Rank: 30, Perms: testWriter | PermPin})
	if err != nil {
		t.Fatal(err)
	}
	spread(def)
	spread(member(t, lf, c, &MemberBody{Action: MemberRole, Member: x.pub, Role: "helper"}))
	for _, l := range all {
		if r := rightsIn(l, c, x); !r.Pin || !r.Send || r.Moderate {
			t.Fatalf("a helper's rights: %+v", r)
		}
	}
	if roles := lx.Roles(c); len(roles) != 1 || roles[0].Title != "Helper" || roles[0].Color != 0x33aa55 {
		t.Fatalf("roles: %+v", roles)
	}

	// the admin: not at or above their own rank, nothing they may not do themselves, not their own role
	for _, d := range []*RoleDef{
		{Id: "boss", Rank: 60, Perms: PermSend},
		{Id: "deputy", Rank: 50, Perms: PermSend},
		{Id: "profile", Rank: 20, Perms: PermManageGroup},
		{Id: "admin", Perms: PermAll},
		{Id: "owner", Perms: PermSend},
	} {
		if _, _, err := la.AppendRole(c, d); err != ErrForbidden {
			t.Fatalf("the admin defined %q: %v", d.Id, err)
		}
		spread(member(t, la, c, &MemberBody{Action: MemberDefine, Def: d})) // written by hand
	}
	for _, l := range all {
		if len(l.Roles(c)) != 1 {
			t.Fatalf("a refused definition counted: %+v", l.Roles(c))
		}
	}

	// the admin narrows the helper: the helper's members lose the pin everywhere
	narrow, _, err := la.AppendRole(c, &RoleDef{Id: "helper", Title: "Helper", Rank: 30, Perms: testWriter})
	if err != nil {
		t.Fatal(err)
	}
	spread(narrow)
	for _, l := range all {
		if rightsIn(l, c, x).Pin {
			t.Fatal("the narrowed role still pins")
		}
	}
	if _, _, err := lx.AppendPin(c, mustSay(t, lf, c, spread), true); err != ErrForbidden {
		t.Fatalf("a helper pinned after losing it: %v", err)
	}

	// a member cannot give themselves a role, nor define one
	if _, _, err := lx.AppendRole(c, &RoleDef{Id: "mine", Rank: 5, Perms: PermSend}); err != ErrForbidden {
		t.Fatalf("a member defined a role: %v", err)
	}

	// the role goes: its member falls back to the default role
	gone, _, err := la.AppendRole(c, &RoleDef{Id: "helper", Removed: true})
	if err != nil {
		t.Fatal(err)
	}
	spread(gone)
	for _, l := range all {
		if r := rightsIn(l, c, x); !r.Send || len(l.Roles(c)) != 0 {
			t.Fatalf("after the role went: %+v %+v", r, l.Roles(c))
		}
	}
	if _, _, err := lf.AppendRole(c, &RoleDef{Id: "member", Removed: true}); err != ErrForbidden {
		t.Fatalf("a built-in role went: %v", err)
	}
}

func mustSay(t *testing.T, l *Log, c Conv, spread func([]byte)) EventID {
	t.Helper()
	raw, id := say(t, l, c, "something to pin")
	spread(raw)
	return id
}

// The rights of the members' role decide what their messages may hold: links, files, comments in
// threads, reactions. An owner who takes them from the built-in member role takes them from all
// the members, on every replica.
func TestPermissionBitsOfMessages(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("roles2")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	ingest(t, lx, c, member(t, lf, c, roster(f, "owner", x, "member")))
	post, postID := say(t, lf, c, "a post")
	ingest(t, lx, c, post)

	def, _, err := lf.AppendRole(c, &RoleDef{Id: "member", Perms: PermSend | PermVoice})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, def)
	if _, _, err := lx.AppendMessage(c, "see https://example.org", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a link without the right: %v", err)
	}
	if _, _, err := lx.AppendMessage(c, "a file", 0, `{"group_file":{}}`, EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a file without the right: %v", err)
	}
	if _, _, err := lx.AppendMessage(c, "a comment", 0, "", EventID{}, postID); err != ErrForbidden {
		t.Fatalf("a comment without the right: %v", err)
	}
	if _, _, err := lx.AppendReact(c, postID, "👍", true); err != ErrForbidden {
		t.Fatalf("a reaction without the right: %v", err)
	}
	plain, plainID := say(t, lx, c, "plain words")
	if _, _, err := lx.AppendEdit(c, plainID, "now www.example.org"); err != ErrForbidden {
		t.Fatalf("a link put in by an edit: %v", err)
	}
	ingest(t, lf, c, plain)
	if !texts(t, lf, c)["plain words"] {
		t.Fatal("a plain message was refused")
	}
	// the owner keeps every right
	if _, _, err := lf.AppendMessage(c, "https://example.org", 0, "", EventID{}, EventID{}); err != nil {
		t.Fatal(err)
	}
}

// Categories hold channels; the main channel has settings of its own (read-only, slow mode, the
// channel the group opens on); nothing is posted in a category.
func TestCategoriesAndTheMainChannel(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("space3")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	ingest(t, lx, c, member(t, lf, c, roster(f, "owner", x, "member")))
	for _, s := range []ChannelSpec{
		{ID: "talk", Name: "Talk", Category: true},
		{ID: "general2", Name: "chat", Parent: "talk"},
		{ID: "lounge", Name: "Lounge", Voice: true, Parent: "talk"},
		{ID: "", Name: "Announcements", Topic: "news only", ReadOnly: true, SlowSecs: 30, Open: "general2"},
	} {
		raw, _, err := lf.AppendChannel(c, s)
		if err != nil {
			t.Fatalf("%q: %v", s.ID, err)
		}
		ingest(t, lx, c, raw)
	}
	chans := lx.Channels(c)
	if len(chans) != 3 || !chans[0].Category || chans[1].Parent != "talk" || chans[2].Parent != "talk" {
		t.Fatalf("channels: %+v", chans)
	}
	main := lx.MainChannel(c)
	if main.Name != "Announcements" || !main.ReadOnly || main.SlowSecs != 30 || main.Open != "general2" {
		t.Fatalf("main channel: %+v", main)
	}
	if _, _, err := lx.AppendMessage(c, "in the read-only main", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a member posted in a read-only main channel: %v", err)
	}
	post, postID := say(t, lf, c, "release")
	ingest(t, lx, c, post)
	if _, _, err := lx.AppendMessage(c, "a comment", 0, "", EventID{}, postID); err != nil {
		t.Fatalf("a member cannot comment in the main channel: %v", err)
	}
	if _, _, err := lx.AppendMessageIn(c, "talk", "into a category", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a message in a category: %v", err)
	}
	if _, _, err := lx.AppendMessageIn(c, "general2", "hello", 0, "", EventID{}, EventID{}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ChannelSpec{
		{ID: "", Voice: true},
		{ID: "", Remove: true},
		{ID: "cat2", Name: "x", Category: true, Voice: true},
		{ID: "cat3", Name: "x", Category: true, Parent: "talk"},
		{ID: "c4", Name: "x", Open: "talk"},
		{ID: "c5", Name: "x", SlowSecs: maxSlowSecs + 1},
	} {
		if _, _, err := lf.AppendChannel(c, bad); err != ErrBadBody {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, _, err := lx.AppendChannel(c, ChannelSpec{ID: "", Name: "mine"}); err != ErrForbidden {
		t.Fatalf("a member changed the main channel: %v", err)
	}
}

// A channel's overrides decide what roles may do in it, on every replica: here everyone is
// denied writing and reacting in #news but may comment, moderators may write; the owner is never
// held back. Who manages channels sets overrides only for roles below their own rank and grants
// nothing they may not do themselves.
func TestChannelOverrides(t *testing.T) {
	f, a, m, x := newAccount(t), newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("over1")
	lf, la, lm, lx := groupReplica(t, f, rules), groupReplica(t, a, rules), groupReplica(t, m, rules), groupReplica(t, x, rules)
	all := []*Log{lf, la, lm, lx}
	spread := func(raw []byte) {
		for _, l := range all {
			_, _, _ = l.Ingest(c, raw)
		}
	}
	spread(member(t, lf, c, roster(f, "owner", a, "admin", m, "moderator", x, "member")))
	news, _, err := lf.AppendChannel(c, ChannelSpec{ID: "news", Name: "news", Overrides: []Override{
		{Role: Everyone, Deny: PermSend | PermReact}, {Role: "moderator", Allow: PermSend}}})
	if err != nil {
		t.Fatal(err)
	}
	spread(news)
	if _, _, err := lx.AppendMessageIn(c, "news", "a member posts", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a member posted where everyone is denied: %v", err)
	}
	post, postID, err := lm.AppendMessageIn(c, "news", "a moderator posts", 0, "", EventID{}, EventID{})
	if err != nil {
		t.Fatalf("a moderator allowed to post cannot: %v", err)
	}
	spread(post)
	if _, _, err := lx.AppendMessageIn(c, "news", "a comment", 0, "", EventID{}, postID); err != nil {
		t.Fatalf("a member cannot comment: %v", err)
	}
	if _, _, err := lx.AppendReact(c, postID, "👍", true); err != ErrForbidden {
		t.Fatalf("a member reacted where everyone is denied: %v", err)
	}
	if _, _, err := lf.AppendMessageIn(c, "news", "the owner posts", 0, "", EventID{}, EventID{}); err != nil {
		t.Fatalf("the owner was held back: %v", err)
	}
	if r := lx.RightsIn(c, x.id, "news"); r.Send || !r.Threads || r.React {
		t.Fatalf("a member's rights in #news: %+v", r)
	}
	if r := lx.RightsIn(c, x.id, ""); !r.Send || !r.React {
		t.Fatalf("a member's rights in the main channel: %+v", r)
	}
	// a member's message posted elsewhere still counts there
	hi, _, err := lx.AppendMessage(c, "in the main one", 0, "", EventID{}, EventID{})
	if err != nil {
		t.Fatal(err)
	}
	spread(hi)
	for _, l := range all {
		if !texts(t, l, c)["in the main one"] {
			t.Fatal("a message in the main channel was refused")
		}
	}

	// the admin: not for a role at their own rank, nothing they may not do
	for _, ov := range [][]Override{
		{{Role: "admin", Deny: PermSend}},
		{{Role: Everyone, Deny: PermSend | PermReact}, {Role: "moderator", Allow: PermSend}, {Role: "owner", Deny: PermSend}},
	} {
		if _, _, err := la.AppendChannel(c, ChannelSpec{ID: "news", Name: "news", Overrides: ov}); err != ErrForbidden {
			t.Fatalf("the admin set %+v: %v", ov, err)
		}
	}
	if _, _, err := la.AppendChannel(c, ChannelSpec{ID: "news", Name: "news", Overrides: []Override{{Role: Everyone, Allow: PermKick}}}); err != ErrBadBody {
		t.Fatalf("a group permission in a channel override: %v", err)
	}
	// a channel manager below the admin, who may not delete others' messages, cannot grant that
	def, _, err := lf.AppendRole(c, &RoleDef{Id: "chman", Rank: 30, Perms: testWriter | PermManageChannels})
	if err != nil {
		t.Fatal(err)
	}
	spread(def)
	spread(member(t, lf, c, &MemberBody{Action: MemberRole, Member: m.pub, Role: "chman"}))
	if _, _, err := lm.AppendChannel(c, ChannelSpec{ID: "news", Name: "news", Overrides: []Override{
		{Role: Everyone, Deny: PermSend | PermReact}, {Role: "member", Allow: PermDelete}}}); err != ErrForbidden {
		t.Fatalf("a channel manager granted what they may not do: %v", err)
	}
	// the admin may rename the channel: the overrides they did not change stay
	ren, _, err := la.AppendChannel(c, ChannelSpec{ID: "news", Name: "announcements", Overrides: []Override{
		{Role: Everyone, Deny: PermSend | PermReact}, {Role: "moderator", Allow: PermSend}}})
	if err != nil {
		t.Fatalf("the admin cannot rename a channel with overrides: %v", err)
	}
	spread(ren)
	if ch, ok := lx.ChannelOf(c, "news"); !ok || ch.Name != "announcements" || len(ch.Overrides) != 2 {
		t.Fatalf("after the rename: %+v", ch)
	}
}

func nonce(b byte) string { return strings.Repeat(string("0123456789abcdef"[b%16]), 32) }

// An invitation on the log counts its uses where everyone sees them: past its limit, or once it is
// revoked, a join that names it is refused on every replica, whoever let the newcomer in. Only who
// may invite makes one, only who manages roles gives it a role, and only its maker or who manages
// roles revokes it.
func TestInvitationsCountTheirUses(t *testing.T) {
	f, a, x, r := newAccount(t), newAccount(t), newAccount(t), newAccount(t)
	ys := []account{newAccount(t), newAccount(t), newAccount(t)}
	rules := testRules{founder: f.id}
	c := Group("inv1")
	lf, la, lx := groupReplica(t, f, rules), groupReplica(t, a, rules), groupReplica(t, x, rules)
	all := []*Log{lf, la, lx}
	spread := func(raw []byte) {
		for _, l := range all {
			_, _, _ = l.Ingest(c, raw)
		}
	}
	spread(member(t, lf, c, roster(f, "owner", a, "admin", x, "member", r, "subscriber")))
	exp := time.Now().Add(time.Hour).Unix()
	inv, _, err := lf.AppendInvite(c, &InviteDef{N: nonce(1), Exp: exp, Uses: 2, Label: "for the team"})
	if err != nil {
		t.Fatal(err)
	}
	spread(inv)
	mine, _, err := lx.AppendInvite(c, &InviteDef{N: nonce(2), Exp: exp})
	if err != nil {
		t.Fatalf("a member cannot invite: %v", err)
	}
	spread(mine)
	if _, _, err := lx.AppendInvite(c, &InviteDef{N: nonce(3), Exp: exp, Role: "moderator"}); err != ErrForbidden {
		t.Fatalf("a member made an invitation that gives a role: %v", err)
	}
	if _, _, err := la.AppendInvite(c, &InviteDef{N: nonce(4), Exp: exp, Role: "admin"}); err != ErrForbidden {
		t.Fatalf("an admin made an invitation to their own rank: %v", err)
	}

	// two come in with the team's invitation, let in by different members; the third is refused
	for i, y := range ys {
		admitter := lf
		if i == 1 {
			admitter = la
		}
		spread(member(t, admitter, c, &MemberBody{Action: MemberJoin, Member: y.pub, Via: nonce(1)}))
		in := i < 2
		for _, l := range all {
			if l.Participant(c, y.id) != in {
				t.Fatalf("newcomer %d in: %v, want %v", i, !in, in)
			}
		}
	}
	if i, ok := lx.InviteOf(c, nonce(1)); !ok || i.Used != 2 || !i.Spent() || i.Label != "for the team" {
		t.Fatalf("the team's invitation: %+v", i)
	}

	// revoking: not somebody else's without managing roles; the maker's own, or by an admin
	if _, _, err := lx.AppendInvite(c, &InviteDef{N: nonce(1), Exp: exp, Revoked: true}); err != ErrForbidden {
		t.Fatalf("a member revoked somebody else's invitation: %v", err)
	}
	rev, _, err := la.AppendInvite(c, &InviteDef{N: nonce(2), Exp: exp, Revoked: true})
	if err != nil {
		t.Fatalf("an admin cannot revoke: %v", err)
	}
	spread(rev)
	late := newAccount(t)
	spread(member(t, lf, c, &MemberBody{Action: MemberJoin, Member: late.pub, Via: nonce(2)}))
	for _, l := range all {
		if l.Participant(c, late.id) {
			t.Fatal("a revoked invitation let somebody in")
		}
	}
	if n := len(lf.Invites(c)); n != 2 {
		t.Fatalf("invitations: %d", n)
	}
}
