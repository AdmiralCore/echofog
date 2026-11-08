package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
	"google.golang.org/protobuf/proto"
)

// ---- several nodes with groups over one in-memory chat/2 --------------------------------------------------------

// gclock is a clock the tests move.
type gclock struct{ off atomic.Int64 }

func (c *gclock) Now() time.Time { return testNow.Add(time.Duration(c.off.Load())) }

type gNode struct {
	pub       [32]byte
	priv      ed25519.PrivateKey
	node      [32]byte
	eng       *convsync.Engine
	app       *app.App
	book      *app.GroupBook
	rig       *rig
	bookStore *memKV
	world     *gWorld
	sent      int // letters sent to the node
}

type gLetter struct {
	from, to [32]byte
	pm       *chatpb.PersonalMessageProto
}

type gWorld struct {
	t      *testing.T
	mu     sync.Mutex
	queue  []gLetter
	nodes  map[[32]byte]*gNode
	pubs   map[[32]byte][32]byte
	seq    uint64
	clk    *gclock
	toNode map[[32]byte]int // letters delivered to each node
}

type gPort struct {
	w    *gWorld
	self [32]byte
}

func (p *gPort) Send(_ context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.seq++
	var id [16]byte
	binary.BigEndian.PutUint64(id[8:], p.w.seq)
	p.w.queue = append(p.w.queue, gLetter{p.self, to, proto.Clone(m).(*chatpb.PersonalMessageProto)})
	p.w.toNode[to]++
	return id, nil
}

type engNet struct{ p *gPort }

func (n engNet) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error {
	_, err := n.p.Send(ctx, to, m)
	return err
}

func (w *gWorld) Pub(_ context.Context, node [32]byte) ([32]byte, error) {
	if pub, ok := w.pubs[node]; ok {
		return pub, nil
	}
	return [32]byte{}, errors.New("unknown")
}

func (w *gWorld) drain() {
	w.t.Helper()
	for i := 0; i < 20000; i++ {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.mu.Unlock()
			return
		}
		l := w.queue[0]
		w.queue = w.queue[1:]
		dst := w.nodes[l.to]
		w.mu.Unlock()
		if dst == nil {
			continue
		}
		dst.app.OnIncoming(context.Background(), l.from, l.pm)
	}
	w.t.Fatal("the network never went quiet")
}

// groupWorldConfig lets a test change the configuration of every app of the next world (tests run one
// after the other, and a test puts it back).
var groupWorldConfig func(*app.Config)

func newGroupWorld(t *testing.T, n int) (*gWorld, []*gNode) {
	t.Helper()
	w := &gWorld{t: t, nodes: map[[32]byte]*gNode{}, pubs: map[[32]byte][32]byte{}, clk: &gclock{}, toNode: map[[32]byte]int{}}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sk := secret.New(key)
	ids, err := store.NewIDs(sk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ids.Close(); sk.Destroy() })
	var out []*gNode
	for i := 0; i < n; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		g := &gNode{priv: priv, world: w}
		copy(g.pub[:], pub)
		g.node = convsync.NodeOf(g.pub)
		w.pubs[g.node] = g.pub
		port := &gPort{w: w, self: g.node}
		sign := func(b []byte) ([]byte, error) { return ed25519.Sign(g.priv, b), nil }
		rg := newRig(t, app.ModePublic)
		g.bookStore = newKV()
		book, err := app.NewGroupBook(app.BookConfig{Pub: g.pub, Sign: sign, Store: g.bookStore, Clock: w.clk})
		if err != nil {
			t.Fatal(err)
		}
		g.book = book
		g.rig = rg
		eng, err := convsync.New(convsync.Config{
			Pub: g.pub, Sign: sign, Store: newKV(), IDs: ids, Net: engNet{port}, Dir: w,
			Rules: book.Rules(), Groups: book.Conv, Members: book.Members, SharedGroups: book.SharedGroups,
			Now: func() time.Time { return testNow },
		})
		if err != nil {
			t.Fatal(err)
		}
		g.eng = eng
		cfg := app.Config{
			Runtime: rg.rt, Links: rg.ln, Store: rg.kv, Clock: w.clk, Self: g.node, Mode: app.ModePublic, Version: "0.3.0",
			Convs: eng, Groups: book, Chat: port,
			Files: &holder{}, FileNet: &fakeFiles{w: newWorld(), self: g.node}, FileDir: t.TempDir(),
			Prefs: convsync.NewPrefs(newKV()),
			PubOf: func(ctx context.Context, n [32]byte) ([32]byte, error) { return w.Pub(ctx, n) },
		}
		if groupWorldConfig != nil {
			groupWorldConfig(&cfg)
		}
		a, err := app.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		g.app = a
		w.nodes[g.node] = g
		out = append(out, g)
	}
	return w, out
}

// befriend adds o to g's contacts.
func (g *gNode) befriend(t *testing.T, o *gNode) {
	t.Helper()
	url := "echofog://join?v=2&card=" + hex.EncodeToString(o.node[:4])
	g.rig.ln.preview[url] = &links.Preview{Kind: links.KindContact, Node: o.node, Device: o.node, NotAfter: testNow.Add(time.Hour)}
	g.rig.ln.peer = node.Peer{Node: node.NodeID(o.node), Device: node.DeviceID(o.node)}
	if _, err := g.app.AddByLink(context.Background(), url); err != nil {
		t.Fatalf("AddByLink: %v", err)
	}
}

func (g *gNode) nodeHex() string { return hex.EncodeToString(g.node[:]) }

func gref(id, channel string) app.ChatRef {
	return app.ChatRef{Kind: "group", ID: id, Channel: channel}
}

// team makes a private group of alice and the others, all let in by open invitations.
func team(t *testing.T, n int) (*gWorld, []*gNode, string) {
	t.Helper()
	w, ns := newGroupWorld(t, n)
	ctx := context.Background()
	g, err := ns[0].app.CreateGroup(ctx, "Команда", "рабочий чат", app.GroupPrivate)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	for _, m := range ns[1:] {
		inv, err := ns[0].app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
		if err != nil {
			t.Fatalf("GroupInviteCreate: %v", err)
		}
		if _, err := m.app.JoinGroup(ctx, inv.Link); err != nil {
			t.Fatalf("JoinGroup: %v", err)
		}
		w.drain()
	}
	return w, ns, g.ID
}

func memberRoles(t *testing.T, n *gNode, id string) map[string]string {
	t.Helper()
	ms, err := n.app.GroupMembers(id)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	out := map[string]string{}
	for _, m := range ms {
		out[m.Node] = m.Role
	}
	return out
}

// ---- tests -----------------------------------------------------------------------------------------------------

func TestCreateGroupListsItWithItsOwner(t *testing.T) {
	_, ns := newGroupWorld(t, 1)
	a := ns[0]
	g, err := a.app.CreateGroup(context.Background(), "Друзья", "обо всём", app.GroupPrivate)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if g.ID == "" || g.Title != "Друзья" || g.Kind != app.GroupPrivate || !g.Owner || g.Members != 1 || g.Role != "owner" {
		t.Fatalf("group %+v", g)
	}
	list, err := a.app.Groups()
	if err != nil || len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("Groups %+v, %v", list, err)
	}
	ms, err := a.app.GroupMembers(g.ID)
	if err != nil || len(ms) != 1 || !ms[0].Self || !ms[0].Owner || ms[0].Node != a.nodeHex() {
		t.Fatalf("members %+v, %v", ms, err)
	}
	chats, err := a.app.Conversations()
	if err != nil || len(chats) != 1 || chats[0].Kind != "group" || chats[0].Name != "Друзья" || chats[0].Ref != gref(g.ID, "") {
		t.Fatalf("chat list %+v, %v", chats, err)
	}
}

func TestCreateGroupRefusesBadData(t *testing.T) {
	_, ns := newGroupWorld(t, 1)
	ctx := context.Background()
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	for name, c := range map[string][3]string{
		"empty title": {"", "", app.GroupPrivate},
		"blank title": {"   ", "", app.GroupPrivate},
		"long title":  {string(long), "", app.GroupPrivate},
		"control":     {"a\x00b", "", app.GroupPrivate},
		"bad kind":    {"x", "", "swarm"},
	} {
		if _, err := ns[0].app.CreateGroup(ctx, c[0], c[1], c[2]); !errors.Is(err, app.ErrBadGroup) {
			t.Errorf("%s: got %v, want ErrBadGroup", name, err)
		}
	}
}

func TestInvitedMembersJoinAndTalk(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	for _, n := range ns {
		if got := len(memberRoles(t, n, id)); got != 3 {
			t.Fatalf("a member sees %d members, want 3", got)
		}
	}
	if _, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "всем привет"}); err != nil {
		t.Fatalf("alice sends: %v", err)
	}
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "и вам"}); err != nil {
		t.Fatalf("bob sends: %v", err)
	}
	w.drain()
	for _, n := range []*gNode{alice, bob, carol} {
		page, err := n.app.ChatHistory(ctx, gref(id, ""), "", 20)
		if err != nil || len(page.Messages) != 2 {
			t.Fatalf("history %+v, %v", page, err)
		}
	}
	list, _ := carol.app.Groups()
	if len(list) != 1 || list[0].Members != 3 || list[0].Pending || list[0].Role != "member" || list[0].Owner {
		t.Fatalf("carol's group %+v", list)
	}
}

func TestLocalInviteFailuresAreOneRefusal(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Закрытая", "", app.GroupPrivate)
	good, err := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	forged := good.Link[:len(good.Link)-6] + "AAAAAA"
	links := map[string]string{
		"empty":   "",
		"garbage": "echofog://x",
		"prefix":  "echofog-group:2:",
		"forged":  forged,
		"long":    "echofog-group:2:" + string(make([]byte, 9000)),
	}
	var want string
	for name, l := range links {
		_, jerr := bob.app.JoinGroup(ctx, l)
		_, ierr := bob.app.InspectGroupInvite(l)
		if !errors.Is(jerr, app.ErrInvite) || !errors.Is(ierr, app.ErrInvite) {
			t.Errorf("%s: join %v, inspect %v, want ErrInvite", name, jerr, ierr)
			continue
		}
		if want == "" {
			want = jerr.Error()
		} else if jerr.Error() != want || ierr.Error() != want {
			t.Errorf("%s: the refusals differ: %q, %q, %q", name, jerr, ierr, want)
		}
	}
	// a term that ran out is refused the same way
	w.clk.off.Store(int64(2 * time.Minute))
	if _, err := bob.app.JoinGroup(ctx, good.Link); !errors.Is(err, app.ErrInvite) || err.Error() != want {
		t.Errorf("expired: %v", err)
	}
}

func TestRemoteInviteFailuresLookTheSameToTheJoiner(t *testing.T) {
	// A revoked invitation, one used up, and a ban say nothing to the newcomer: the call works, the
	// group waits as pending, no message comes back, and the group does not grow (AD20).
	w, ns := newGroupWorld(t, 5)
	alice, bob, carol, dave, erin := ns[0], ns[1], ns[2], ns[3], ns[4]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Закрытая", "", app.GroupPrivate)

	revoked, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if err := alice.app.GroupInviteRevoke(ctx, g.ID, revoked.Nonce); err != nil {
		t.Fatal(err)
	}
	once, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour, Uses: 1})
	if _, err := erin.app.JoinGroup(ctx, once.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if len(memberRoles(t, alice, g.ID)) != 2 {
		t.Fatal("the first use of the invitation must let erin in")
	}
	open, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if err := alice.app.GroupRemove(ctx, g.ID, erin.nodeHex(), true); err != nil {
		t.Fatal(err)
	}
	w.drain()

	before := len(memberRoles(t, alice, g.ID))
	type outcome struct {
		err     error
		pending bool
		replies int
	}
	try := func(n *gNode, link string) outcome {
		w.mu.Lock()
		base := w.toNode[n.node]
		w.mu.Unlock()
		info, err := n.app.JoinGroup(ctx, link)
		w.drain()
		w.mu.Lock()
		got := w.toNode[n.node] - base
		w.mu.Unlock()
		return outcome{err, info.Pending, got}
	}
	o1 := try(bob, revoked.Link)
	o2 := try(carol, once.Link)
	o3 := try(erin, open.Link) // banned
	for i, o := range []outcome{o1, o2, o3} {
		if o.err != nil || !o.pending || o.replies != 0 {
			t.Errorf("case %d: %+v, want no error, pending, nothing sent back", i, o)
		}
	}
	if got := len(memberRoles(t, alice, g.ID)); got != before {
		t.Errorf("the group grew from %d to %d", before, got)
	}
	for _, n := range []*gNode{bob, carol, erin} {
		list, _ := n.app.Groups()
		if len(list) != 1 || !list[0].Pending {
			t.Errorf("a refused newcomer's group list: %+v", list)
		}
	}
	_ = dave
}

func TestRemoteRefusalsAreAlikeInParallel(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Закрытая", "", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	_ = alice.app.GroupInviteRevoke(ctx, g.ID, inv.Nonce)
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = bob.app.JoinGroup(ctx, inv.Link)
		}(i)
	}
	wg.Wait()
	w.drain()
	for i, err := range errs {
		if err != nil {
			t.Errorf("parallel join %d: %v", i, err)
		}
	}
	if len(memberRoles(t, alice, g.ID)) != 1 {
		t.Fatal("the revoked invitation let somebody in")
	}
}

func TestInviteWithApprovalWaitsForADecision(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "По заявкам", "", app.GroupPrivate)
	inv, err := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour, Approval: true, Label: "для друзей"})
	if err != nil {
		t.Fatal(err)
	}
	pv, err := bob.app.InspectGroupInvite(inv.Link)
	if err != nil || pv.Title != "По заявкам" || !pv.Approval || pv.Member {
		t.Fatalf("preview %+v, %v", pv, err)
	}
	for _, n := range []*gNode{bob, carol} {
		info, err := n.app.JoinGroup(ctx, inv.Link)
		if err != nil || !info.Pending {
			t.Fatalf("JoinGroup: %+v, %v", info, err)
		}
	}
	w.drain()
	reqs, err := alice.app.GroupJoinRequests(g.ID)
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests %+v, %v", reqs, err)
	}
	if got := len(memberRoles(t, alice, g.ID)); got != 1 {
		t.Fatalf("a request let somebody in: %d members", got)
	}
	if err := alice.app.GroupJoinDecide(ctx, g.ID, bob.nodeHex(), true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := alice.app.GroupJoinDecide(ctx, g.ID, carol.nodeHex(), false); err != nil {
		t.Fatalf("decline: %v", err)
	}
	w.drain()
	if roles := memberRoles(t, alice, g.ID); len(roles) != 2 || roles[bob.nodeHex()] != "member" {
		t.Fatalf("members after the decisions: %v", roles)
	}
	if len(memberRoles(t, bob, g.ID)) != 2 {
		t.Fatal("bob must see the group after being accepted")
	}
	if left, _ := alice.app.GroupJoinRequests(g.ID); len(left) != 0 {
		t.Fatalf("requests left: %+v", left)
	}
	if list, _ := bob.app.Groups(); len(list) != 1 || list[0].Pending {
		t.Fatalf("bob's list %+v", list)
	}
	if err := alice.app.GroupJoinDecide(ctx, g.ID, carol.nodeHex(), true); err == nil {
		t.Fatal("a declined request must be gone")
	}
}

func TestPlainMembersCannotManage(t *testing.T) {
	_, ns, id := team(t, 3)
	bob, carol := ns[1], ns[2]
	ctx := context.Background()
	if err := bob.app.GroupSetRole(ctx, id, carol.nodeHex(), "admin"); err == nil {
		t.Error("a member gave a role")
	}
	if err := bob.app.GroupRemove(ctx, id, carol.nodeHex(), false); err == nil {
		t.Error("a member removed another")
	}
	if _, err := bob.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "x", Name: "x"}); !errors.Is(err, app.ErrNoRight) {
		t.Errorf("a member made a channel: %v", err)
	}
	if _, err := bob.app.GroupRoleSave(ctx, id, app.GroupRole{ID: "boss", Rank: 40, Perms: 1}); !errors.Is(err, app.ErrNoRight) {
		t.Errorf("a member defined a role: %v", err)
	}
	if err := bob.app.GroupTimeout(ctx, id, carol.nodeHex(), 5); err == nil {
		t.Error("a member gave a timeout")
	}
	if err := bob.app.GroupTransfer(ctx, id, carol.nodeHex()); err == nil {
		t.Error("a member handed the group over")
	}
}

func TestRolesRankAndCustomRoles(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	if err := alice.app.GroupSetRole(ctx, id, bob.nodeHex(), "admin"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	w.drain()
	if got := memberRoles(t, carol, id)[bob.nodeHex()]; got != "admin" {
		t.Fatalf("carol sees bob as %q", got)
	}
	// an admin makes a custom role below their rank and gives it
	role, err := bob.app.GroupRoleSave(ctx, id, app.GroupRole{ID: "helper", Title: "Помощник", Color: 0x00ff00, Rank: 20, Perms: convlog.PermSend | convlog.PermDelete})
	if err != nil {
		t.Fatalf("RoleSave: %v", err)
	}
	if role.ID != "helper" || role.Builtin {
		t.Fatalf("role %+v", role)
	}
	if err := bob.app.GroupSetRole(ctx, id, carol.nodeHex(), "helper"); err != nil {
		t.Fatalf("give role: %v", err)
	}
	w.drain()
	roles, err := alice.app.GroupRoles(id)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range roles {
		if r.ID == "helper" && r.Title == "Помощник" && !r.Builtin && r.Rank == 20 {
			found = true
		}
		if r.ID == "owner" && !r.Builtin {
			t.Errorf("owner role not built in: %+v", r)
		}
	}
	if !found {
		t.Fatalf("roles %+v", roles)
	}
	if got := memberRoles(t, alice, id)[carol.nodeHex()]; got != "helper" {
		t.Fatalf("carol is %q", got)
	}
	// an admin cannot touch the owner, nor give a role above their own
	if err := bob.app.GroupSetRole(ctx, id, alice.nodeHex(), "member"); err == nil {
		t.Error("an admin demoted the owner")
	}
	if _, err := bob.app.GroupRoleSave(ctx, id, app.GroupRole{ID: "big", Rank: 60, Perms: 1}); err == nil {
		t.Error("an admin made a role above their rank")
	}
	// the role goes: carol is a member again
	if err := bob.app.GroupRoleRemove(ctx, id, "helper"); err != nil {
		t.Fatalf("RoleRemove: %v", err)
	}
	w.drain()
	if got := memberRoles(t, alice, id)[carol.nodeHex()]; got != "member" {
		t.Fatalf("after the role went, carol is %q", got)
	}
}

func TestChannelsAndTheirRights(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	ch, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "news", Name: "Новости", ReadOnly: true})
	if err != nil {
		t.Fatalf("ChannelSave: %v", err)
	}
	if ch.ID != "news" || !ch.ReadOnly {
		t.Fatalf("channel %+v", ch)
	}
	w.drain()
	chans, err := bob.app.GroupChannels(id)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, c := range chans {
		if c.ID == "news" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("bob does not see the channel: %+v", chans)
	}
	if _, err := bob.app.ChatSend(ctx, gref(id, "news"), app.SendReq{Text: "я тоже"}); err == nil {
		t.Fatal("a member wrote in a read-only channel")
	}
	if _, err := alice.app.ChatSend(ctx, gref(id, "news"), app.SendReq{Text: "новость дня"}); err != nil {
		t.Fatalf("the owner writes in it: %v", err)
	}
	w.drain()
	page, err := bob.app.ChatHistory(ctx, gref(id, "news"), "", 10)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("bob reads the channel: %+v, %v", page, err)
	}
	if r, err := bob.app.GroupRights(id, "news"); err != nil || r.Send || !r.Member {
		t.Fatalf("bob's rights in it: %+v, %v", r, err)
	}
	if r, err := bob.app.GroupRights(id, ""); err != nil || !r.Send {
		t.Fatalf("bob's rights in the main channel: %+v, %v", r, err)
	}
	// a voice room, a category, an order
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "talk", Name: "Разговоры", Voice: true}); err != nil {
		t.Fatalf("voice room: %v", err)
	}
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "work", Name: "Работа", Category: true}); err != nil {
		t.Fatalf("category: %v", err)
	}
	if err := alice.app.GroupChannelPlace(ctx, id, "news", "work"); err != nil {
		t.Fatalf("place: %v", err)
	}
	if err := alice.app.GroupChannelMove(ctx, id, "talk", 0); err != nil {
		t.Fatalf("move: %v", err)
	}
	w.drain()
	chans, _ = bob.app.GroupChannels(id)
	for _, c := range chans {
		if c.ID == "news" && c.Parent != "work" {
			t.Errorf("the channel is not in its category: %+v", c)
		}
	}
	if err := alice.app.GroupChannelRemove(ctx, id, "talk"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	w.drain()
	chans, _ = bob.app.GroupChannels(id)
	for _, c := range chans {
		if c.ID == "talk" {
			t.Errorf("a removed channel is listed: %+v", c)
		}
	}
}

func TestCategoriesSetupAndMainChannel(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "chat", Name: "Чат"}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "room", Name: "Комната", Voice: true}); err != nil {
		t.Fatal(err)
	}
	cats, err := alice.app.GroupCategoriesSetup(ctx, id, "Текстовые", "Голосовые")
	if err != nil || len(cats) != 2 {
		t.Fatalf("setup %v, %v", cats, err)
	}
	w.drain()
	chans, _ := bob.app.GroupChannels(id)
	parents := map[string]string{}
	for _, c := range chans {
		parents[c.ID] = c.Parent
	}
	if parents["chat"] != cats[0] || parents["room"] != cats[1] {
		t.Fatalf("channels not under their categories: %v, categories %v", parents, cats)
	}
	if err := alice.app.GroupMainChannelSave(ctx, id, convlog.ChannelSpec{Name: "Общий", Topic: "главное"}); err != nil {
		t.Fatalf("main save: %v", err)
	}
	w.drain()
	main, err := bob.app.GroupMainChannel(id)
	if err != nil || main.Name != "Общий" || main.Topic != "главное" {
		t.Fatalf("main channel %+v, %v", main, err)
	}
	if err := bob.app.GroupMainChannelSave(ctx, id, convlog.ChannelSpec{Name: "Мой"}); !errors.Is(err, app.ErrNoRight) {
		t.Fatalf("a member renamed the main channel: %v", err)
	}
}

func TestRemoveBanAndTimeout(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	// a timeout keeps bob from writing, until it is lifted
	if err := alice.app.GroupTimeout(ctx, id, bob.nodeHex(), 10); err != nil {
		t.Fatalf("timeout: %v", err)
	}
	w.drain()
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "можно?"}); err == nil {
		t.Fatal("a member in a timeout wrote")
	}
	ms, _ := carol.app.GroupMembers(id)
	var until int64
	for _, m := range ms {
		if m.Node == bob.nodeHex() {
			until = m.Timeout
		}
	}
	if until == 0 {
		t.Fatal("the member list does not show the timeout")
	}
	if err := alice.app.GroupTimeout(ctx, id, bob.nodeHex(), 0); err != nil {
		t.Fatalf("lift: %v", err)
	}
	w.drain()
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "теперь можно"}); err != nil {
		t.Fatalf("after the timeout: %v", err)
	}
	// carol is removed, and bob is banned and cannot come back by a fresh invitation
	if err := alice.app.GroupRemove(ctx, id, carol.nodeHex(), false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := alice.app.GroupRemove(ctx, id, bob.nodeHex(), true); err != nil {
		t.Fatalf("ban: %v", err)
	}
	w.drain()
	if roles := memberRoles(t, alice, id); len(roles) != 1 {
		t.Fatalf("members left: %v", roles)
	}
	for _, n := range []*gNode{bob, carol} {
		if _, err := n.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "я ещё здесь"}); err == nil {
			t.Error("a removed member wrote")
		}
	}
	inv, _ := alice.app.GroupInviteCreate(ctx, id, app.InviteSpec{TTL: time.Hour})
	_ = bob.app.ForgetGroup(id)
	_, _ = bob.app.JoinGroup(ctx, inv.Link)
	_, _ = carol.app.JoinGroup(ctx, inv.Link) // carol was only removed: she may come back
	w.drain()
	roles := memberRoles(t, alice, id)
	if _, in := roles[bob.nodeHex()]; in {
		t.Error("a banned member came back")
	}
	if _, in := roles[carol.nodeHex()]; !in {
		t.Error("a removed (not banned) member could not come back")
	}
}

func TestLeaveAndHandOver(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	if err := alice.app.LeaveGroup(ctx, id); !errors.Is(err, app.ErrOwnerStays) {
		t.Fatalf("the owner left: %v", err)
	}
	if err := carol.app.LeaveGroup(ctx, id); err != nil {
		t.Fatalf("leave: %v", err)
	}
	w.drain()
	if list, _ := carol.app.Groups(); len(list) != 0 {
		t.Fatalf("carol still lists the group: %+v", list)
	}
	if got := len(memberRoles(t, alice, id)); got != 2 {
		t.Fatalf("members after the leave: %d", got)
	}
	if err := alice.app.GroupTransfer(ctx, id, bob.nodeHex()); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	w.drain()
	if roles := memberRoles(t, bob, id); roles[bob.nodeHex()] != "owner" || roles[alice.nodeHex()] != "admin" {
		t.Fatalf("after the transfer: %v", roles)
	}
	if err := alice.app.LeaveGroup(ctx, id); err != nil {
		t.Fatalf("the former owner leaves: %v", err)
	}
	w.drain()
	if got := len(memberRoles(t, bob, id)); got != 1 {
		t.Fatalf("members: %d", got)
	}
}

func TestOwnerAloneForgetsTheGroup(t *testing.T) {
	_, ns := newGroupWorld(t, 1)
	a := ns[0]
	ctx := context.Background()
	g, _ := a.app.CreateGroup(ctx, "Пустая", "", app.GroupPrivate)
	if err := a.app.ForgetGroup(g.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if list, _ := a.app.Groups(); len(list) != 0 {
		t.Fatalf("list %+v", list)
	}
	if chats, _ := a.app.Conversations(); len(chats) != 0 {
		t.Fatalf("chats %+v", chats)
	}
}

func TestModerationLogNamesTheActions(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, carol := ns[0], ns[2]
	ctx := context.Background()
	_ = alice.app.GroupSetRole(ctx, id, ns[1].nodeHex(), "admin")
	_ = alice.app.GroupRemove(ctx, id, carol.nodeHex(), false)
	w.drain()
	log, err := ns[1].app.GroupModerationLog(id)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range log {
		have[e.Action] = true
	}
	for _, a := range []string{"roster", "join", "role", "remove"} {
		if !have[a] {
			t.Errorf("the log lacks %q: %+v", a, log)
		}
	}
}

func TestStrangersAndUnknownGroupsLookAlike(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, dave := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Секретная", "", app.GroupPrivate)
	w.drain()
	for _, id := range []string{g.ID, "fog_grp_unknown_00000000", "", "../x"} {
		if _, err := dave.app.ChatHistory(ctx, gref(id, ""), "", 10); !errors.Is(err, app.ErrNoChat) {
			t.Errorf("history of %q: %v", id, err)
		}
		if _, err := dave.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "x"}); !errors.Is(err, app.ErrNoChat) {
			t.Errorf("send to %q: %v", id, err)
		}
		if _, err := dave.app.GroupMembers(id); !errors.Is(err, app.ErrNoGroup) {
			t.Errorf("members of %q: %v", id, err)
		}
		if _, err := dave.app.GroupInfoOf(id); !errors.Is(err, app.ErrNoGroup) {
			t.Errorf("info of %q: %v", id, err)
		}
	}
}

func TestThreadsListAndTitles(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	root, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "тема для обсуждения"})
	if err != nil {
		t.Fatal(err)
	}
	w.drain()
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "согласен", Thread: root.ID}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	w.drain()
	ths, err := alice.app.ChatThreads(ctx, gref(id, ""))
	if err != nil || len(ths) != 1 || ths[0].ID != root.ID {
		t.Fatalf("threads %+v, %v", ths, err)
	}
	if err := alice.app.ChatThreadTitle(ctx, gref(id, ""), root.ID, "Обсуждение"); err != nil {
		t.Fatalf("title: %v", err)
	}
	w.drain()
	ths, _ = bob.app.ChatThreads(ctx, gref(id, ""))
	if len(ths) != 1 || ths[0].ThreadTitle != "Обсуждение" {
		t.Fatalf("bob's threads %+v", ths)
	}
}

func TestInviteContactsHandsEachTheLink(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	for _, p := range []*gNode{bob, carol} {
		alice.befriend(t, p)
	}
	g, _ := alice.app.CreateGroup(ctx, "Свои", "", app.GroupPrivate)
	n, err := alice.app.GroupInviteContacts(ctx, g.ID, []string{bob.nodeHex(), carol.nodeHex(), "00"}, "")
	if err != nil || n != 2 {
		t.Fatalf("invited %d, %v", n, err)
	}
	w.drain()
	for _, p := range []*gNode{bob, carol} {
		page, err := p.app.ChatHistory(ctx, app.ChatRef{Kind: "dm", ID: alice.nodeHex()}, "", 10)
		if err != nil || len(page.Messages) != 1 {
			t.Fatalf("the invitation did not arrive: %+v, %v", page, err)
		}
		pv, err := p.app.InspectGroupInvite(page.Messages[0].Text)
		if err != nil || pv.Title != "Свои" {
			t.Fatalf("the message is not an invitation: %q: %+v, %v", page.Messages[0].Text, pv, err)
		}
	}
}

func TestChannelKindGivesReadersAReadOnlyRole(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, err := alice.app.CreateGroup(ctx, "Вести", "", app.GroupChannel)
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if got := memberRoles(t, alice, g.ID)[bob.nodeHex()]; got != "subscriber" {
		t.Fatalf("a reader of a channel is %q", got)
	}
	if _, err := bob.app.ChatSend(ctx, gref(g.ID, ""), app.SendReq{Text: "комментарий"}); err == nil {
		t.Fatal("a reader wrote in a channel")
	}
}

func TestGroupCallsWithoutABookAreRefused(t *testing.T) {
	rg := newRig(t, app.ModePublic)
	if _, err := rg.a.CreateGroup(context.Background(), "x", "", app.GroupPrivate); !errors.Is(err, app.ErrGroupOff) {
		t.Fatalf("CreateGroup: %v", err)
	}
	if _, err := rg.a.Groups(); !errors.Is(err, app.ErrGroupOff) {
		t.Fatalf("Groups: %v", err)
	}
	if _, err := rg.a.JoinGroup(context.Background(), "x"); !errors.Is(err, app.ErrGroupOff) {
		t.Fatalf("JoinGroup: %v", err)
	}
}

func TestJoinMessageFromAnotherNodeChangesNothing(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, mallory := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Закрытая", "", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour, Approval: true})
	// mallory sends a request in bob's name: the sender is not the account named in it
	raw := `fogjoin2:{"v":1,"g":"` + g.ID + `","n":"` + inv.Nonce + `","p":"` + hex.EncodeToString(bob.pub[:]) + `"}`
	pm := &chatpb.PersonalMessageProto{Metadata: raw}
	if !alice.app.OnGroupMessage(ctx, mallory.node, pm) {
		t.Fatal("a join message must be taken, and ignored")
	}
	w.drain()
	if reqs, _ := alice.app.GroupJoinRequests(g.ID); len(reqs) != 0 {
		t.Fatalf("a forged request was kept: %+v", reqs)
	}
	// the same message from bob himself is kept
	alice.app.OnGroupMessage(ctx, bob.node, pm)
	if reqs, _ := alice.app.GroupJoinRequests(g.ID); len(reqs) != 1 {
		t.Fatalf("bob's request: %+v", reqs)
	}
	// text that is no join message is not taken
	if alice.app.OnGroupMessage(ctx, bob.node, &chatpb.PersonalMessageProto{Text: "привет"}) {
		t.Fatal("a chat message was taken as a join message")
	}
	// a message for a group that is not kept, and a huge one, are taken and ignored
	big := &chatpb.PersonalMessageProto{Metadata: "fogjoin2:" + string(make([]byte, 5000))}
	if !alice.app.OnGroupMessage(ctx, bob.node, big) {
		t.Fatal("a big join message must be taken")
	}
}

func TestGroupsSurviveARestart(t *testing.T) {
	w, ns, id := team(t, 2)
	alice := ns[0]
	book2, err := app.NewGroupBook(app.BookConfig{Pub: alice.pub, Sign: func(b []byte) ([]byte, error) { return ed25519.Sign(alice.priv, b), nil }, Store: alice.bookStore, Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := book2.Conv(convlog.GroupConv(id))
	if !ok || c.Group != id {
		t.Fatalf("a restarted book lost the group: %+v, %v", c, ok)
	}
	rg := newRig(t, app.ModePublic)
	a2, err := app.New(app.Config{Runtime: rg.rt, Links: rg.ln, Store: rg.kv, Clock: w.clk, Self: alice.node, Mode: app.ModePublic,
		Convs: alice.eng, Groups: book2})
	if err != nil {
		t.Fatal(err)
	}
	if ms, err := a2.GroupMembers(id); err != nil || len(ms) != 2 {
		t.Fatalf("members after the restart: %+v, %v", ms, err)
	}
}

type fakeVoice struct{ got [][2]string }

func (f *fakeVoice) Roster(group, channel string) []app.VoiceMember {
	f.got = append(f.got, [2]string{group, channel})
	return []app.VoiceMember{{Node: "n1", Muted: true}}
}

func TestVoiceRosterComesFromTheRuntimeOnly(t *testing.T) {
	w, ns, id := team(t, 2)
	alice := ns[0]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "talk", Name: "Разговоры", Voice: true}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if _, err := alice.app.GroupVoice(id, "talk"); !errors.Is(err, app.ErrNoVoice) {
		t.Fatalf("no roster wired: %v", err)
	}
	v := &fakeVoice{}
	rg := newRig(t, app.ModePublic)
	a2, err := app.New(app.Config{Runtime: rg.rt, Links: rg.ln, Store: rg.kv, Clock: w.clk, Self: alice.node, Mode: app.ModePublic,
		Convs: alice.eng, Groups: alice.book, Voice: v})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a2.GroupVoice(id, "talk")
	if err != nil || len(got) != 1 || got[0].Node != "n1" || len(v.got) != 1 || v.got[0] != [2]string{id, "talk"} {
		t.Fatalf("roster %+v, %v, asked %v", got, err, v.got)
	}
	// a text channel and an unknown one are no voice rooms
	for _, ch := range []string{"", "nope"} {
		if _, err := a2.GroupVoice(id, ch); !errors.Is(err, app.ErrNoGroup) {
			t.Errorf("room %q: %v", ch, err)
		}
	}
	if _, err := a2.GroupVoice("fog_grp_x", "talk"); !errors.Is(err, app.ErrNoGroup) {
		t.Errorf("unknown group: %v", err)
	}
	// the roster never enters the log
	if page, _ := alice.app.ChatHistory(ctx, gref(id, "talk"), "", 10); len(page.Messages) != 0 {
		t.Errorf("the room's log holds %d messages", len(page.Messages))
	}
}

func letters(w *gWorld, n *gNode) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.toNode[n.node]
}

func TestBannedFounderGetsNothingMore(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	if err := alice.app.GroupTransfer(ctx, id, bob.nodeHex()); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if err := bob.app.GroupRemove(ctx, id, alice.nodeHex(), true); err != nil {
		t.Fatalf("ban the founder: %v", err)
	}
	w.drain()
	before := letters(w, alice)
	for _, text := range []string{"раз", "два", "три"} {
		if _, err := carol.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	w.drain()
	if got := letters(w, alice); got != before {
		t.Fatalf("the banned founder got %d letters after the ban", got-before)
	}
	for _, n := range []*gNode{bob, carol} {
		for _, m := range n.book.Members(convlog.Group(id)) {
			if m == alice.pub {
				t.Fatal("the member list still names the banned founder")
			}
		}
	}
}

func TestRemovedIssuerIsNotAMemberAnyMore(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	i1, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, i1.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	i2, err := bob.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if err != nil {
		t.Fatalf("a member's invitation: %v", err)
	}
	if _, err := carol.app.JoinGroup(ctx, i2.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if len(memberRoles(t, carol, g.ID)) != 3 {
		t.Fatal("carol is not in")
	}
	if err := alice.app.GroupRemove(ctx, g.ID, bob.nodeHex(), true); err != nil {
		t.Fatal(err)
	}
	w.drain()
	for _, m := range carol.book.Members(convlog.Group(g.ID)) {
		if m == bob.pub {
			t.Fatal("the member list names the removed one who let carol in")
		}
	}
}

func TestRepeatedJoinMessagesSendTheHistoryOnce(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Закрытая", "", app.GroupPrivate)
	for i := 0; i < 20; i++ {
		if _, err := alice.app.ChatSend(ctx, gref(g.ID, ""), app.SendReq{Text: "сообщение"}); err != nil {
			t.Fatal(err)
		}
	}
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	pm := &chatpb.PersonalMessageProto{Metadata: `fogjoin2:{"v":1,"g":"` + g.ID + `","n":"` + inv.Nonce + `","p":"` + hex.EncodeToString(bob.pub[:]) + `"}`}
	base := letters(w, bob)
	alice.app.OnGroupMessage(ctx, bob.node, pm)
	first := letters(w, bob) - base
	if first == 0 {
		t.Fatal("the first request got no history")
	}
	for i := 0; i < 29; i++ {
		alice.app.OnGroupMessage(ctx, bob.node, pm)
	}
	if got := letters(w, bob) - base; got != first {
		t.Fatalf("30 requests brought %d letters, the first one %d", got, first)
	}
	w.clk.off.Store(int64(11 * time.Minute))
	alice.app.OnGroupMessage(ctx, bob.node, pm)
	if got := letters(w, bob) - base; got <= first {
		t.Fatal("after the pause a new request got nothing")
	}
}

func TestWaitingEntriesExpire(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Закрытая", "", app.GroupPrivate)
	ask, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: 20 * 24 * time.Hour, Approval: true})
	gone, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: 20 * 24 * time.Hour})
	_ = alice.app.GroupInviteRevoke(ctx, g.ID, gone.Nonce)
	w.drain()
	if _, err := bob.app.JoinGroup(ctx, ask.Link); err != nil {
		t.Fatal(err)
	}
	if _, err := carol.app.JoinGroup(ctx, gone.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if reqs, _ := alice.app.GroupJoinRequests(g.ID); len(reqs) != 1 {
		t.Fatalf("requests: %+v", reqs)
	}
	w.clk.off.Store(int64(8 * 24 * time.Hour))
	if reqs, _ := alice.app.GroupJoinRequests(g.ID); len(reqs) != 0 {
		t.Fatalf("a week-old request is still waiting: %+v", reqs)
	}
	if err := alice.app.GroupJoinDecide(ctx, g.ID, bob.nodeHex(), true); !errors.Is(err, app.ErrRequest) {
		t.Fatalf("deciding on an expired request: %v", err)
	}
	if list, _ := carol.app.Groups(); len(list) != 0 {
		t.Fatalf("a pending group that nobody answered is still listed: %+v", list)
	}
}

// One app does both: group events go to the members, and file transfers go to the subscribers of
// transfer events (the two used the name emit before they were merged).
func TestGroupEventsAndTransferEventsInOneApp(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	alice.befriend(t, bob)
	fw := newWorld()
	a2, err := app.New(app.Config{
		Runtime: alice.rig.rt, Links: alice.rig.ln, Store: alice.rig.kv, Clock: w.clk, Self: alice.node, Mode: app.ModePublic,
		Convs: alice.eng, Groups: alice.book, Chat: &gPort{w: w, self: alice.node},
		Files: &holder{}, FileNet: &fakeFiles{w: fw, self: alice.node}, FileDir: t.TempDir(),
		FileFree: func(string) (uint64, error) { return 1 << 40, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a2.Stop(context.Background())
		ts, _ := a2.ListFileTransfers("")
		for _, tr := range ts {
			_ = a2.CancelFile(tr.ID)
		}
	})
	var mu sync.Mutex
	var seen []app.Transfer
	stop := a2.OnFileTransfer(func(tr app.Transfer) { mu.Lock(); seen = append(seen, tr); mu.Unlock() })
	defer stop()

	// a group event reaches the other member
	if err := a2.GroupTimeout(ctx, id, bob.nodeHex(), 5); err != nil {
		t.Fatalf("group event: %v", err)
	}
	w.drain()
	ms, _ := bob.app.GroupMembers(id)
	var until int64
	for _, m := range ms {
		if m.Node == bob.nodeHex() {
			until = m.Timeout
		}
	}
	if until == 0 {
		t.Fatal("the group event did not reach bob")
	}
	// a transfer event reaches the subscriber
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, []byte("данные"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.SendFile(ctx, bob.nodeHex(), p); err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n == 0 {
		t.Fatal("no transfer event")
	}
}
