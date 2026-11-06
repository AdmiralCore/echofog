package convsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// ---- a small network of engines --------------------------------------------------------------------------

// the roles of the group service
const writer = convlog.PermSend | convlog.PermMedia | convlog.PermVoice | convlog.PermLinks | convlog.PermThreads | convlog.PermReact | convlog.PermInvite

var roles = map[string]convlog.Role{
	"owner":      {Name: "owner", Rank: 100, Perms: convlog.PermAll},
	"admin":      {Name: "admin", Rank: 50, Perms: convlog.PermAll &^ convlog.PermManageGroup},
	"member":     {Name: "member", Rank: 10, Perms: writer},
	"subscriber": {Name: "subscriber", Rank: 10, Perms: convlog.PermReact},
}

type rules struct{ founder types.PeerID }

func (rules) Current(convlog.Conv, types.PeerID) convlog.Rights { return convlog.Rights{} }
func (r rules) Founder(convlog.Conv) (types.PeerID, bool)       { return r.founder, true }
func (rules) Role(n string) (convlog.Role, bool)                { x, ok := roles[n]; return x, ok }
func (rules) DefaultRole(convlog.Conv) string                   { return "member" }

type acct struct {
	pub  [32]byte
	priv ed25519.PrivateKey
	id   types.PeerID
	node [32]byte
}

func newAcct(t *testing.T) acct {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var a acct
	copy(a.pub[:], pub)
	a.priv = priv
	a.id = types.PeerPubKeyToID(types.PeerPublicKey(a.pub))
	a.node = NodeOf(a.pub)
	return a
}

type letter struct {
	from, to [32]byte
	pm       *chatpb.PersonalMessageProto
}

// netw delivers messages when the test says so, and can cut accounts off from each other.
type netw struct {
	mu      sync.Mutex
	queue   []letter
	engines map[[32]byte]*Engine // by node
	pubs    map[[32]byte][32]byte
	cut     func(from, to [32]byte) bool
	sent    int
	sentBy  map[[32]byte]int
}

type port struct {
	n    *netw
	self [32]byte
}

func (p port) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	p.n.sent++
	p.n.sentBy[p.self]++
	if p.n.cut != nil && p.n.cut(p.self, to) {
		return errors.New("unreachable")
	}
	p.n.queue = append(p.n.queue, letter{p.self, to, proto.Clone(m).(*chatpb.PersonalMessageProto)})
	return nil
}

func (n *netw) Pub(ctx context.Context, node [32]byte) ([32]byte, error) {
	if pub, ok := n.pubs[node]; ok {
		return pub, nil
	}
	return [32]byte{}, errors.New("unknown")
}

// drain delivers until nothing is left to deliver.
func (n *netw) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		n.mu.Lock()
		if len(n.queue) == 0 {
			n.mu.Unlock()
			return
		}
		l := n.queue[0]
		n.queue = n.queue[1:]
		e := n.engines[l.to]
		n.mu.Unlock()
		if e != nil {
			e.Receive(context.Background(), l.from, l.pm)
		}
	}
	t.Fatal("the network never went quiet")
}

type member struct {
	a acct
	e *Engine
}

type cluster struct {
	t       *testing.T
	n       *netw
	members []*member
	group   convlog.Conv
	clock   time.Time
}

func newCluster(t *testing.T, size int, groupMembers int) *cluster {
	t.Helper()
	c := &cluster{t: t, n: &netw{engines: map[[32]byte]*Engine{}, pubs: map[[32]byte][32]byte{}, sentBy: map[[32]byte]int{}}, group: convlog.Group("g1")}
	var accts []acct
	for i := 0; i < size; i++ {
		accts = append(accts, newAcct(t))
		c.n.pubs[accts[i].node] = accts[i].pub
	}
	founder := accts[0].id
	var gm [][32]byte
	for i := 0; i < groupMembers; i++ {
		gm = append(gm, accts[i].pub)
	}
	// one store and one blinding key for all members (each has its own namespace): every secret locks
	// memory, and a Windows process has little quota for it
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sk := secret.New(key)
	st, err := store.Open(store.Options{Dir: filepath.Join(t.TempDir(), "v2"), Key: sk})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := store.NewIDs(sk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ids.Close(); _ = st.Close(); sk.Destroy() })
	for i, a := range accts {
		a := a
		e, err := New(Config{
			Pub: a.pub, Sign: func(b []byte) ([]byte, error) { return ed25519.Sign(a.priv, b), nil },
			Store: st.Store(fmt.Sprintf("v2/convlog/%d", i)), IDs: ids, Rules: rules{founder: founder},
			Net: port{c.n, a.node}, Dir: c.n,
			Groups:       func(id convlog.ConvID) (convlog.Conv, bool) { return c.group, id == c.group.ID },
			Members:      func(convlog.Conv) [][32]byte { return gm },
			SharedGroups: func([32]byte) []convlog.Conv { return []convlog.Conv{c.group} },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		c.n.engines[a.node] = e
		c.members = append(c.members, &member{a, e})
	}
	return c
}

// rosterOf makes the founder write the roster of the first k accounts and hands it to all of them.
func (c *cluster) rosterOf(k int, extra map[int]string) {
	c.t.Helper()
	body := &convlog.MemberBody{Action: convlog.MemberRoster}
	for i := 0; i < k; i++ {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		if r, ok := extra[i]; ok {
			role = r
		}
		body.Roster = append(body.Roster, &convlog.RosterEntry{Member: c.members[i].a.pub[:], Role: role})
	}
	raw, err := proto.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	ev, _, err := c.members[0].e.Log().AppendKept(c.group, convlog.Draft{Kind: convlog.KindMember, Body: raw})
	if err != nil {
		c.t.Fatalf("roster: %v", err)
	}
	if err := c.members[0].e.Publish(context.Background(), c.group, ev); err != nil {
		c.t.Fatal(err)
	}
	c.n.drain(c.t)
}

func (c *cluster) say(i int, text string) convlog.EventID {
	c.t.Helper()
	raw, id, err := c.members[i].e.Log().AppendMessage(c.group, text, 0, "", convlog.EventID{}, convlog.EventID{})
	if err != nil {
		c.t.Fatalf("%d says %q: %v", i, text, err)
	}
	// an unreachable group is not an error of the author: the event stays in the log and the sync brings it
	_ = c.members[i].e.Publish(context.Background(), c.group, raw)
	return id
}

func (c *cluster) texts(i int) map[string]bool {
	c.t.Helper()
	msgs, _, err := c.members[i].e.Log().History(c.group, "", 1000)
	if err != nil {
		c.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range msgs {
		out[m.Text] = true
	}
	return out
}

// syncAll has every member open a sync with every other one, until nothing moves.
func (c *cluster) syncAll(k int) {
	c.t.Helper()
	for round := 0; round < 3; round++ {
		for i := 0; i < k; i++ {
			for j := 0; j < k; j++ {
				if i != j {
					_ = c.members[i].e.Hello(context.Background(), c.members[j].a.pub)
				}
			}
		}
		c.n.drain(c.t)
	}
}

// ---- tests ---------------------------------------------------------------------------------------------

func TestNodeOfIsTheHashOfTheAkey(t *testing.T) {
	a := newAcct(t)
	if NodeOf(a.pub) == ([32]byte{}) || NodeOf(a.pub) == NodeOf(newAcct(t).pub) {
		t.Fatal("node IDs are not distinct")
	}
}

func TestDirectChatEventsReachThePeer(t *testing.T) {
	c := newCluster(t, 2, 0)
	a, b := c.members[0], c.members[1]
	dm := a.e.DM(b.a.pub)
	if dm.ID != b.e.DM(a.a.pub).ID {
		t.Fatal("the two sides name the direct chat differently")
	}
	raw, id, err := a.e.Log().AppendMessage(dm, "hello b", 0, "", convlog.EventID{}, convlog.EventID{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.e.Publish(context.Background(), dm, raw); err != nil {
		t.Fatal(err)
	}
	c.n.drain(t)
	m, ok := b.e.Log().Message(b.e.DM(a.a.pub), id)
	if !ok || m.Text != "hello b" {
		t.Fatalf("b has %+v %v", m, ok)
	}
	// an edit and a delete follow
	edit, _, _ := a.e.Log().AppendEdit(dm, id, "hello b!")
	_ = a.e.Publish(context.Background(), dm, edit)
	c.n.drain(t)
	if m, _ := b.e.Log().Message(b.e.DM(a.a.pub), id); m.Text != "hello b!" {
		t.Fatalf("after the edit: %q", m.Text)
	}
}

// A group of ten converges after the network was cut in two and healed.
func TestGroupOfTenConvergesAfterAPartition(t *testing.T) {
	c := newCluster(t, 10, 10)
	c.rosterOf(10, nil)
	half := func(x [32]byte) int {
		for i, m := range c.members {
			if m.a.node == x {
				return i / 5
			}
		}
		return -1
	}
	c.n.cut = func(from, to [32]byte) bool { return half(from) != half(to) }
	for i := 0; i < 10; i++ {
		c.say(i, "line from "+string(rune('a'+i)))
	}
	c.n.drain(t)
	if len(c.texts(0)) != 5 || len(c.texts(9)) != 5 {
		t.Fatalf("during the cut a sees %d, j sees %d lines", len(c.texts(0)), len(c.texts(9)))
	}
	c.n.cut = nil
	c.syncAll(10)
	want := c.texts(0)
	if len(want) != 10 {
		t.Fatalf("a sees %d of 10 lines after the sync", len(want))
	}
	digest, _ := c.members[0].e.Log().Meta(c.group)
	for i := 1; i < 10; i++ {
		got := c.texts(i)
		for k := range want {
			if !got[k] {
				t.Fatalf("member %d lacks %q", i, k)
			}
		}
		if m, _ := c.members[i].e.Log().Meta(c.group); !bytes.Equal(m.Digest(), digest.Digest()) {
			t.Fatalf("member %d has another digest", i)
		}
	}
}

// What an author may do is decided at the point of the event: a reader writes nothing, a removed member
// keeps what they wrote while in, and loses what they write after.
func TestRightsAreCheckedAtThePointOfTheEvent(t *testing.T) {
	c := newCluster(t, 4, 4)
	c.rosterOf(4, map[int]string{3: "subscriber"})
	c.say(1, "from a member")
	// a reader cannot write: the author's own log refuses it ...
	if _, _, err := c.members[3].e.Log().AppendMessage(c.group, "from a reader", 0, "", convlog.EventID{}, convlog.EventID{}); err == nil {
		t.Fatal("a reader wrote a message")
	}
	// ... and so does everybody else, when the event is made by hand and sent
	body, _ := proto.Marshal(&convlog.MsgBody{Text: "from a reader"})
	forged, _, err := c.members[3].e.Log().AppendKept(c.group, convlog.Draft{Kind: convlog.KindMsg, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.members[3].e.Publish(context.Background(), c.group, forged)
	c.n.drain(t)
	c.syncAll(4)
	for i := 0; i < 4; i++ {
		got := c.texts(i)
		if !got["from a member"] || got["from a reader"] {
			t.Fatalf("member %d sees %v", i, got)
		}
	}
	// the founder removes member 2; what 2 wrote before stays, what they write after is refused
	c.say(2, "written while in")
	c.n.drain(t)
	body, _ = proto.Marshal(&convlog.MemberBody{Action: convlog.MemberRemove, Member: c.members[2].a.pub[:]})
	rm, _, err := c.members[0].e.Log().AppendKept(c.group, convlog.Draft{Kind: convlog.KindMember, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.members[0].e.Publish(context.Background(), c.group, rm)
	c.n.drain(t)
	// the removed member's own log refuses to write; an event made by hand is refused by the others
	if _, _, err := c.members[2].e.Log().AppendMessage(c.group, "written after the removal", 0, "", convlog.EventID{}, convlog.EventID{}); err == nil {
		t.Fatal("a removed member wrote a message")
	}
	body, _ = proto.Marshal(&convlog.MsgBody{Text: "written after the removal"})
	late, _, err := c.members[2].e.Log().AppendKept(c.group, convlog.Draft{Kind: convlog.KindMsg, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.members[2].e.Publish(context.Background(), c.group, late)
	c.n.drain(t)
	c.syncAll(4)
	for _, i := range []int{0, 1, 3} {
		got := c.texts(i)
		if !got["written while in"] || got["written after the removal"] {
			t.Fatalf("member %d sees %v", i, got)
		}
	}
}

// A stranger gets no answer to a sync, and the silence is the same whether the group exists or not.
func TestStrangerGetsNoAnswerToASync(t *testing.T) {
	c := newCluster(t, 4, 3) // the fourth account is not in the group
	c.rosterOf(3, nil)
	c.say(1, "private line")
	c.n.drain(t)
	stranger := c.members[3]
	before := c.n.sentBy[c.members[1].a.node] + c.n.sentBy[c.members[0].a.node] + c.n.sentBy[c.members[2].a.node]
	_ = stranger.e.Hello(context.Background(), c.members[1].a.pub)
	// and a Want built by hand
	want := stranger.e.Log().Want(c.group)
	b, _ := convlog.EncodeSync(want)
	c.members[1].e.Receive(context.Background(), stranger.a.node, &chatpb.PersonalMessageProto{ConvSync: b})
	c.n.drain(t)
	after := c.n.sentBy[c.members[1].a.node] + c.n.sentBy[c.members[0].a.node] + c.n.sentBy[c.members[2].a.node]
	if after != before {
		t.Fatalf("the members sent %d messages to a stranger's sync", after-before)
	}
	if len(c.texts(3)) != 0 {
		t.Fatal("the stranger has the history")
	}
}

// An event of a kind this build does not know is stored, counted in the sync, passed on unchanged and
// shown as a stub (conversation-log.md section 2.5, versioning.md 3.9).
func TestUnknownKindsAreStoredSyncedAndForwardedUnchanged(t *testing.T) {
	c := newCluster(t, 3, 3)
	c.rosterOf(3, nil)
	a, b, d := c.members[0], c.members[1], c.members[2]
	c.n.cut = func(from, to [32]byte) bool {
		return (from == a.a.node && to == d.a.node) || (from == d.a.node && to == a.a.node)
	}

	// a writes an event of kind 77 with a body this build cannot read, and hands it to b only
	raw, _, err := a.e.Log().Append(c.group, convlog.Draft{Kind: 77, Body: []byte("a future feature")})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// ... with a field this build does not know appended to the encoding: it must survive as it is
	raw = append(raw, 0xfa, 0x06, 0x03, 'n', 'e', 'w') // field 111, length-delimited
	if err := a.e.Publish(context.Background(), c.group, raw); err != nil {
		t.Fatal(err)
	}
	c.n.drain(t)

	if m, ok := b.e.Log().Meta(c.group); !ok || m.VV[hexOf(a.a.id)] != 2 { // the roster is a's first event
		t.Fatalf("b did not count the event in its vector: %+v", m)
	}
	stubs := b.e.Stubs(c.group)
	if len(stubs) != 1 || stubs[0].Kind != 77 || stubs[0].Text != StubText || stubs[0].Author != a.a.pub {
		t.Fatalf("b's stubs: %+v", stubs)
	}
	// b passes it on in sync, byte for byte; d still cannot reach a, so b is the only source
	for round := 0; round < 3; round++ {
		_ = b.e.Hello(context.Background(), d.a.pub)
		_ = d.e.Hello(context.Background(), b.a.pub)
		c.n.drain(t)
	}
	for name, m := range map[string]*member{"b": b, "d": d} {
		var found bool
		for _, r := range m.e.Log().Export(c.group, 1<<20) {
			if bytes.Equal(r, raw) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s holds the event changed, or not at all", name)
		}
	}
	if len(d.e.Stubs(c.group)) != 1 {
		t.Fatalf("d's stubs: %+v", d.e.Stubs(c.group))
	}
	// the chat history is intact, and once a reaches d the digest agrees everywhere
	c.n.cut = nil
	c.syncAll(3)
	ma, _ := a.e.Log().Meta(c.group)
	md, _ := d.e.Log().Meta(c.group)
	if !bytes.Equal(ma.Digest(), md.Digest()) {
		t.Fatal("the digests differ")
	}
}

func hexOf(id types.PeerID) string { return hex.EncodeToString(id[:]) }

// A large group is not flooded: the author sends to MaxFanout members, the rest get it from sync.
func TestLargeGroupPushIsBounded(t *testing.T) {
	size := MeshLimit + 10
	c := newCluster(t, size, size)
	c.rosterOf(size, nil)
	before := c.n.sentBy[c.members[1].a.node]
	c.say(1, "to a large group")
	if got := c.n.sentBy[c.members[1].a.node] - before; got != MaxFanout {
		t.Fatalf("the author sent %d messages, want %d", got, MaxFanout)
	}
	// a small group gets all of them
	small := newCluster(t, 6, 6)
	small.rosterOf(6, nil)
	before = small.n.sentBy[small.members[1].a.node]
	small.say(1, "to a small group")
	if got := small.n.sentBy[small.members[1].a.node] - before; got != 5 {
		t.Fatalf("the author sent %d messages in a group of 6, want 5", got)
	}
}

// A peer cannot drown the log: past a burst its events are dropped (sync gets them later).
func TestEventsFromOnePeerAreRateLimited(t *testing.T) {
	c := newCluster(t, 2, 0)
	a, b := c.members[0], c.members[1]
	dm := a.e.DM(b.a.pub)
	var ids []convlog.EventID
	n := EventBurst + 50
	for i := 0; i < n; i++ {
		raw, id, err := a.e.Log().AppendMessage(dm, "spam", 0, "", convlog.EventID{}, convlog.EventID{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		b.e.Receive(context.Background(), a.a.node, &chatpb.PersonalMessageProto{ConvEvent: raw})
	}
	m, _ := b.e.Log().Meta(b.e.DM(a.a.pub))
	if got := m.VV[hexOf(a.a.id)]; got == 0 || got > uint64(EventBurst)+2 {
		t.Fatalf("b took %d of %d events from one peer in an instant", got, n)
	}
	// the sync gets the rest
	_ = b.e.Hello(context.Background(), a.a.pub)
	c.n.drain(t)
	_ = a.e.Hello(context.Background(), b.a.pub)
	c.n.drain(t)
	if m, _ := b.e.Log().Meta(b.e.DM(a.a.pub)); m.VV[hexOf(a.a.id)] != uint64(n) {
		t.Fatalf("after the sync b has %d of %d", m.VV[hexOf(a.a.id)], n)
	}
	_ = ids
}
