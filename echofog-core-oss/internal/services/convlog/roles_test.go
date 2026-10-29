package convlog

import "testing"

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
