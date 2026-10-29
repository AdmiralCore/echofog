package convlog

import (
	"crypto/ed25519"
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// testRules: the group service's roles, a founder per group, and no present state (the log decides).
type testRules struct {
	founder types.PeerID
	channel bool
}

// the group service's built-in roles
const testWriter = PermSend | PermMedia | PermVoice | PermLinks | PermThreads | PermReact | PermInvite

var testRoles = map[string]Role{
	"owner":      {Name: "owner", Rank: 100, Perms: PermAll},
	"admin":      {Name: "admin", Rank: 50, Perms: PermAll &^ PermManageGroup},
	"moderator":  {Name: "moderator", Rank: 25, Perms: testWriter | PermDelete | PermKick | PermPin},
	"member":     {Name: "member", Rank: 10, Perms: testWriter},
	"subscriber": {Name: "subscriber", Rank: 10, Perms: PermReact},
}

func (r testRules) Current(Conv, types.PeerID) Rights { return Rights{} }
func (r testRules) Founder(Conv) (types.PeerID, bool) { return r.founder, true }
func (testRules) Role(name string) (Role, bool)       { x, ok := testRoles[name]; return x, ok }
func (r testRules) DefaultRole(Conv) string {
	if r.channel {
		return "subscriber"
	}
	return "member"
}

func groupReplica(t *testing.T, a account, rules testRules) *Log {
	l := replica(t, a)
	l.SetGroupRules(rules)
	return l
}

func member(t *testing.T, l *Log, c Conv, b *MemberBody) []byte {
	t.Helper()
	body, err := proto.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := l.AppendKept(c, Draft{Kind: KindMember, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func say(t *testing.T, l *Log, c Conv, text string) ([]byte, EventID) {
	t.Helper()
	raw, id, err := l.AppendMessage(c, text, 0, "", EventID{}, EventID{})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return raw, id
}

func texts(t *testing.T, l *Log, c Conv) map[string]bool {
	t.Helper()
	msgs, _, err := l.History(c, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range msgs {
		if !m.Deleted {
			out[m.Text] = true
		}
	}
	return out
}

// ingestAll delivers raws in the given order, then once more as the next sync does: an event from
// an author the replica could not place yet (it came before the roster) is dropped the first time
// and taken when sync offers it again.
func ingestAll(l *Log, c Conv, raws [][]byte) {
	for pass := 0; pass < 2; pass++ {
		for _, r := range raws {
			_, _, _ = l.Ingest(c, r)
		}
	}
}

func roster(entries ...any) *MemberBody {
	b := &MemberBody{Action: MemberRoster}
	for i := 0; i < len(entries); i += 2 {
		b.Roster = append(b.Roster, &RosterEntry{Member: entries[i].(account).pub, Role: entries[i+1].(string)})
	}
	return b
}

// A member who was removed keeps the messages they wrote while in the group: a newcomer who syncs
// the history later sees them, whatever order the events come in. What they write after the
// removal is refused everywhere, and refused events do not leave holes in the version vector.
func TestMembershipDecidesByPositionInAnyOrder(t *testing.T) {
	f, x, y := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("g1")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)

	var all [][]byte
	r := member(t, lf, c, roster(f, "owner", x, "member"))
	ingest(t, lx, c, r)
	all = append(all, r)
	hi, _ := say(t, lx, c, "x was here")
	ingest(t, lf, c, hi)
	all = append(all, hi)
	rm := member(t, lf, c, &MemberBody{Action: MemberRemove, Member: x.pub})
	all = append(all, rm)
	_, _, _ = lx.Ingest(c, rm)
	join := member(t, lf, c, &MemberBody{Action: MemberJoin, Member: y.pub})
	all = append(all, join)
	// x writes on after being removed (its own log no longer lets it, so forge the next event)
	late := forgeNext(t, x, c, 2, 100, "after removal")
	all = append(all, late)

	for round := 0; round < 20; round++ {
		order := append([][]byte(nil), all...)
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		ly := groupReplica(t, y, rules)
		ingestAll(ly, c, order)
		ly.RebuildIfNeeded(c)
		got := texts(t, ly, c)
		if !got["x was here"] || got["after removal"] {
			t.Fatalf("round %d: %v", round, got)
		}
		meta, _ := ly.Meta(c)
		if meta.VV[peerHex(x.id)] != 2 {
			t.Fatalf("round %d: x's sequence has a hole: vv=%v", round, meta.VV)
		}
	}
}

// forgeNext signs an event of a's directly, at a chosen place, bypassing a's own log.
func forgeNext(t *testing.T, a account, c Conv, seq, lamport uint64, text string) []byte {
	t.Helper()
	e := &Event{Conv: c.ID[:], Author: a.pub, Seq: seq, Lamport: lamport, TsMs: 1, Kind: KindMsg,
		Body: encodeBody(&MsgBody{Text: text})}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(a, m), nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A membership event that arrives late but sits before events already applied rebuilds the
// conversation: a message of a member removed before it was written disappears, and one from a
// member let in before it was written appears.
func TestLateMembershipEventRebuilds(t *testing.T) {
	f, x, y := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("g2")
	lf, lx, ly := groupReplica(t, f, rules), groupReplica(t, x, rules), groupReplica(t, y, rules)
	r := member(t, lf, c, roster(f, "owner", x, "member", y, "member"))
	ingest(t, lx, c, r)
	ingest(t, ly, c, r)
	rm := member(t, lf, c, &MemberBody{Action: MemberRemove, Member: x.pub})
	// x has not heard of its removal and writes on
	afterRm := forgeNext(t, x, c, 1, 50, "x after removal")
	ymsg, _ := say(t, ly, c, "y stays")

	other := groupReplica(t, newAccount(t), rules)
	ingest(t, other, c, r, ymsg)
	if _, _, err := other.Ingest(c, afterRm); err != nil {
		t.Fatalf("x is still a member here: %v", err)
	}
	if !texts(t, other, c)["x after removal"] {
		t.Fatal("x's message not shown before the removal is known")
	}
	ingest(t, other, c, rm) // sits before x's message: it was never allowed
	got := texts(t, other, c)
	if got["x after removal"] || !got["y stays"] {
		t.Fatalf("after the late removal: %v", got)
	}
}

// A moderator's delete counts if it came before the moderator was demoted, and not after; the same
// on every device whatever the order.
func TestModerationFollowsTheRoleAtThatPoint(t *testing.T) {
	f, m, w := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("g3")
	lf, lm, lw := groupReplica(t, f, rules), groupReplica(t, m, rules), groupReplica(t, w, rules)
	r := member(t, lf, c, roster(f, "owner", m, "moderator", w, "member"))
	ingest(t, lm, c, r)
	ingest(t, lw, c, r)
	a1, id1 := say(t, lw, c, "first")
	a2, id2 := say(t, lw, c, "second")
	ingest(t, lm, c, a1, a2)
	ingest(t, lf, c, a1, a2)
	del1, _, err := lm.AppendDelete(c, id1)
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lf, c, del1)
	demote := member(t, lf, c, &MemberBody{Action: MemberRole, Member: m.pub, Role: "member"})
	ingest(t, lm, c, demote)
	// the demoted moderator tries again (forged after the demotion)
	del2 := forgeOp(t, m, c, 3, 100, KindDelete, id2)

	all := [][]byte{r, a1, a2, del1, demote, del2}
	for round := 0; round < 20; round++ {
		order := append([][]byte(nil), all...)
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		l := groupReplica(t, newAccount(t), rules)
		ingestAll(l, c, order)
		l.RebuildIfNeeded(c)
		got := texts(t, l, c)
		if got["first"] || !got["second"] {
			t.Fatalf("round %d: %v", round, got)
		}
	}
}

func forgeOp(t *testing.T, a account, c Conv, seq, lamport uint64, kind uint32, target EventID) []byte {
	t.Helper()
	e := &Event{Conv: c.ID[:], Author: a.pub, Seq: seq, Lamport: lamport, TsMs: 1, Kind: kind, Target: target[:]}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(a, m), nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := Encode(e)
	return raw
}

// In a channel the readers who join on their own read but do not write.
func TestChannelReadersDoNotWrite(t *testing.T) {
	f, s := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id, channel: true}
	c := Group("ch")
	lf, ls := groupReplica(t, f, rules), groupReplica(t, s, rules)
	r := member(t, lf, c, roster(f, "owner"))
	ingest(t, ls, c, r)
	join := member(t, ls, c, &MemberBody{Action: MemberJoin, Member: s.pub})
	ingest(t, lf, c, join)
	if _, _, err := ls.AppendMessage(c, "reader writes", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a reader wrote: %v", err)
	}
	post, id := say(t, lf, c, "news")
	ingest(t, ls, c, post)
	if _, _, err := ls.AppendReact(c, id, "👍", true); err != nil {
		t.Fatalf("a reader cannot react: %v", err)
	}
	if !ls.Participant(c, s.id) {
		t.Fatal("the reader is not in the channel")
	}
}

// A refused event of a participant fills its place in the author's sequence, so two replicas
// agree on the version vector and a sync between them ends.
func TestRefusedEventDoesNotStallSync(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	m1, _ := say(t, la, ca, "one")
	bad := forgeBadBody(t, a, ca, 2, 2)
	m3 := forgeNext(t, a, ca, 3, 3, "three")
	ingest(t, lb, cb, m1)
	if _, _, err := lb.Ingest(cb, bad); err != ErrBadBody {
		t.Fatalf("bad body: %v", err)
	}
	ingest(t, lb, cb, m3)
	meta, _ := lb.Meta(cb)
	if meta.VV[peerHex(a.id)] != 3 {
		t.Fatalf("hole in the vector: %v", meta.VV)
	}
	// a replica that has only the first message catches up with b and the sync settles: both open
	// with a Hello, as on every connection
	lc := replica(t, a)
	ingest(t, lc, ca, m1)
	type msg struct {
		toB bool
		m   *Sync
	}
	queue := []msg{{true, lc.Hello([]Conv{ca})}, {false, lb.Hello([]Conv{cb})}}
	steps := 0
	for len(queue) > 0 && steps < 50 {
		steps++
		q := queue[0]
		queue = queue[1:]
		if q.toB {
			for _, r := range lb.HandleSync(a.id, q.m, nil) {
				queue = append(queue, msg{false, r})
			}
		} else {
			for _, r := range lc.HandleSync(b.id, q.m, nil) {
				queue = append(queue, msg{true, r})
			}
		}
	}
	if steps >= 50 {
		t.Fatal("the sync never settled")
	}
	if !texts(t, lc, ca)["three"] {
		t.Fatal("the message after the refused one did not arrive")
	}
}

func forgeBadBody(t *testing.T, a account, c Conv, seq, lamport uint64) []byte {
	t.Helper()
	e := &Event{Conv: c.ID[:], Author: a.pub, Seq: seq, Lamport: lamport, TsMs: 1, Kind: KindMsg, Body: []byte{0xff, 0xff, 0xff}}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(a, m), nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := Encode(e)
	return raw
}

// History imported from before the log survives a rebuild, with the reactions on it.
func TestLegacyHistorySurvivesRebuild(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("g4")
	lf := groupReplica(t, f, rules)
	if _, err := lf.AddLegacy(c, []Legacy{{ID: "old-1", Author: x.id, TS: 1000, Text: "from before"}}, false); err != nil {
		t.Fatal(err)
	}
	member(t, lf, c, roster(f, "owner", x, "member"))
	if _, _, err := lf.AppendReact(c, LegacyID("old-1"), "🔥", true); err != nil {
		t.Fatal(err)
	}
	lf.mu.Lock()
	_, err := lf.rebuildLocked(c, lf.bc(c.ID))
	lf.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := lf.Message(c, LegacyID("old-1"))
	if !ok || m.Text != "from before" || len(m.Reactions) != 1 {
		raw, _ := json.Marshal(m)
		t.Fatalf("legacy message after rebuild: %v %s", ok, raw)
	}
}

func signWith(a account, m []byte) []byte { return ed25519.Sign(a.priv, m) }

// A newcomer who joined with the invitation and wrote at once: the event letting it in comes from an
// admin concurrently and lands after its message in the log's order. The message counts once the
// admission is there; a removed member's later message still does not.
func TestNewcomersFirstMessageCountsOnceLetIn(t *testing.T) {
	f, y := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("g5")
	lf := groupReplica(t, f, rules)
	r := member(t, lf, c, roster(f, "owner"))
	early := forgeNext(t, y, c, 1, 2, "first words")
	// the admin's admission, written later in the log's order than y's message
	for i := 0; i < 3; i++ {
		say(t, lf, c, "filler")
	}
	join := member(t, lf, c, &MemberBody{Action: MemberJoin, Member: y.pub})

	other := groupReplica(t, newAccount(t), rules)
	ingest(t, other, c, r)
	_, _, _ = other.Ingest(c, early) // y is nobody yet
	ingest(t, other, c, join)
	_, _, _ = other.Ingest(c, early) // offered again by sync
	other.RebuildIfNeeded(c)
	if !texts(t, other, c)["first words"] {
		t.Fatal("the newcomer's first message was lost")
	}
}

// A banned member cannot write "before" the ban: they pick the Lamport of their events, but the
// ban records the last of their sequence numbers that counts, and a later one is refused wherever
// it is placed, until they are let in again (and then only after that point).
func TestBannedMemberCannotBackdate(t *testing.T) {
	f, x, y := newAccount(t), newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("g1")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)

	var all [][]byte
	r := member(t, lf, c, roster(f, "owner", x, "member", y, "member"))
	ingest(t, lx, c, r)
	all = append(all, r)
	hi, _ := say(t, lx, c, "before the ban")
	ingest(t, lf, c, hi)
	all = append(all, hi)
	ban := member(t, lf, c, &MemberBody{Action: MemberBan, Member: x.pub, UptoSeq: lf.AuthorSeq(c, x.id)})
	all = append(all, ban)
	if got := lf.AuthorSeq(c, x.id); got != 1 {
		t.Fatalf("x's last seq seen by the admin = %d, want 1", got)
	}
	// x, banned, signs its next event with a Lamport from before the ban (2: right after its first)
	backdated := forgeNext(t, x, c, 2, 2, "backdated")
	all = append(all, backdated)

	for round := 0; round < 20; round++ {
		order := append([][]byte(nil), all...)
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		ly := groupReplica(t, y, rules)
		ingestAll(ly, c, order)
		ly.RebuildIfNeeded(c)
		got := texts(t, ly, c)
		if !got["before the ban"] || got["backdated"] {
			t.Fatalf("round %d: %v", round, got)
		}
	}

	// let in again: what x writes after that point counts, what it places before still does not
	unban := member(t, lf, c, &MemberBody{Action: MemberUnban, Member: x.pub})
	join := member(t, lf, c, &MemberBody{Action: MemberJoin, Member: x.pub})
	after := forgeNext(t, x, c, 3, 100, "back in")
	early := forgeNext(t, x, c, 4, 3, "placed before the rejoin")
	ly := groupReplica(t, y, rules)
	ingestAll(ly, c, append(all, unban, join, after, early))
	ly.RebuildIfNeeded(c)
	got := texts(t, ly, c)
	if !got["back in"] || got["placed before the rejoin"] || got["backdated"] {
		t.Fatalf("after the rejoin: %v", got)
	}
}
