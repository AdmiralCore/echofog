package convsync

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatcrypto "github.com/echofog/echofog-core/internal/v2/chat/crypto"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// The log over real chat/2 services: sessions, sealed envelopes, ratchet, the outbox. Three devices
// only: memguard locks pages and a Windows process has little quota for it.

type clk struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clk) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clk) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type chatPeer struct {
	tw  *identity.Throwaway
	dl  *objects.DeviceList
	cr  *chatcrypto.Crypto
	svc *chat.Service
	eng *Engine
	pub [32]byte
}

type chatWorld struct {
	t     *testing.T
	clock *clk
	peers map[[32]byte]*chatPeer // by node
	byDev map[[32]byte]*chatPeer
	down  map[[32]byte]bool // devices that cannot be reached
}

type wDir struct{ w *chatWorld }

func (d wDir) Resolve(ctx context.Context, node [32]byte) (*dht.Resolved, error) {
	p := d.w.peers[node]
	if p == nil {
		return nil, errors.New("unknown")
	}
	raw, err := p.cr.Prekeys()
	if err != nil {
		return nil, err
	}
	pk, err := objects.VerifyPrekeys(raw, d.w.clock.Now())
	if err != nil {
		return nil, err
	}
	return &dht.Resolved{Devices: p.dl, Prekeys: []*objects.Prekeys{pk}}, nil
}

type wLink struct{ w *chatWorld }

func (l wLink) Deliver(ctx context.Context, device [32]byte, env []byte) (uint32, error) {
	p := l.w.byDev[device]
	if p == nil || l.w.down[device] {
		return 0, errors.New("unreachable")
	}
	return p.svc.Receive(ctx, env), nil
}

type wPubs struct{ w *chatWorld }

func (d wPubs) Pub(ctx context.Context, node [32]byte) ([32]byte, error) {
	if p := d.w.peers[node]; p != nil {
		return p.pub, nil
	}
	return [32]byte{}, errors.New("unknown")
}

func newChatWorld(t *testing.T, n int) (*chatWorld, []*chatPeer) {
	t.Helper()
	t.Cleanup(memguard.Purge)
	w := &chatWorld{t: t, clock: &clk{t: time.Unix(1_800_000_000, 0)}, peers: map[[32]byte]*chatPeer{}, byDev: map[[32]byte]*chatPeer{}, down: map[[32]byte]bool{}}
	group := convlog.Group("g1")
	var all []*chatPeer
	var pubs [][32]byte
	for i := 0; i < n; i++ {
		tw, err := identity.NewThrowaway()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tw.Destroy)
		rawDL, err := tw.DeviceList(1, w.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		dl, err := objects.VerifyDeviceList(rawDL, w.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		p := &chatPeer{tw: tw, dl: dl}
		copy(p.pub[:], tw.Account.Public().Key())
		pubs = append(pubs, p.pub)
		all = append(all, p)
		w.peers[tw.Account.NodeID()] = p
		w.byDev[tw.Device.ID()] = p
	}
	founder := types.PeerPubKeyToID(types.PeerPublicKey(pubs[0]))
	for _, p := range all {
		p := p
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		sk := secret.New(k)
		st, err := store.Open(store.Options{Dir: filepath.Join(t.TempDir(), "v2"), Key: sk})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := store.NewIDs(sk)
		if err != nil {
			t.Fatal(err)
		}
		cr, err := chatcrypto.New(chatcrypto.Config{Device: p.tw.Device, Store: st.Store, IDs: ids, Now: w.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cr.Close(); ids.Close(); _ = st.Close(); sk.Destroy() })
		p.cr = cr
		self := chat.Self{Node: p.tw.Account.NodeID(), Device: p.tw.Device.ID(), Account: p.tw.Account.Public(), DeviceKey: p.tw.Device.Public()}
		p.svc, err = chat.NewService(chat.ServiceConfig{
			Self: self, Crypto: cr, Dir: wDir{w}, Link: wLink{w}, Store: st.Store("v2/chat/outbox"),
			Now: w.clock.Now, NoJitter: true,
			Events: chat.Events{Message: func(in chat.Incoming) { p.eng.Receive(context.Background(), in.From, in.Message) }},
		})
		if err != nil {
			t.Fatal(err)
		}
		p.eng, err = New(Config{
			Pub: p.pub, Sign: p.tw.Account.Sign, Store: st.Store("v2/convlog"), IDs: ids, Rules: rules{founder: founder},
			Net: ChatNet{p.svc}, Dir: wPubs{w}, Now: w.clock.Now,
			Groups:       func(id convlog.ConvID) (convlog.Conv, bool) { return group, id == group.ID },
			Members:      func(convlog.Conv) [][32]byte { return pubs },
			SharedGroups: func([32]byte) []convlog.Conv { return []convlog.Conv{group} },
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return w, all
}

// settle runs the outboxes until nothing is waiting (the clock moves on between rounds).
func (w *chatWorld) settle(peers []*chatPeer) {
	w.t.Helper()
	for round := 0; round < 40; round++ {
		busy := false
		for _, p := range peers {
			if err := p.svc.Tick(context.Background()); err != nil {
				w.t.Fatal(err)
			}
			if p.svc.Stats().Outbox > 0 {
				busy = true
			}
		}
		if !busy {
			return
		}
		w.clock.Add(3 * time.Second)
	}
	w.t.Fatal("the outboxes never emptied")
}

func TestLogEventsTravelAsChatMessages(t *testing.T) {
	w, ps := newChatWorld(t, 3)
	a, b, c := ps[0], ps[1], ps[2]

	// a direct chat
	dm := a.eng.DM(b.pub)
	raw, id, err := a.eng.Log().AppendMessage(dm, "hello over chat/2", 0, "", convlog.EventID{}, convlog.EventID{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.eng.Publish(context.Background(), dm, raw); err != nil {
		t.Fatal(err)
	}
	w.settle(ps)
	if m, ok := b.eng.Log().Message(b.eng.DM(a.pub), id); !ok || m.Text != "hello over chat/2" {
		t.Fatalf("b has %+v %v", m, ok)
	}

	// a group of three: the founder's roster, then a line from c that a and b get
	group := convlog.Group("g1")
	body, _ := proto.Marshal(&convlog.MemberBody{Action: convlog.MemberRoster, Roster: []*convlog.RosterEntry{
		{Member: a.pub[:], Role: "owner"}, {Member: b.pub[:], Role: "member"}, {Member: c.pub[:], Role: "member"},
	}})
	roster, _, err := a.eng.Log().AppendKept(group, convlog.Draft{Kind: convlog.KindMember, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.eng.Publish(context.Background(), group, roster)
	w.settle(ps)
	line, _, err := c.eng.Log().AppendMessage(group, "from c", 0, "", convlog.EventID{}, convlog.EventID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.eng.Publish(context.Background(), group, line)
	w.settle(ps)
	for name, p := range map[string]*chatPeer{"a": a, "b": b} {
		msgs, _, _ := p.eng.Log().History(group, "", 10)
		if len(msgs) != 1 || msgs[0].Text != "from c" {
			t.Fatalf("%s sees %+v", name, msgs)
		}
	}
	// an unknown kind survives the transport and is shown as a stub
	unk, _, err := a.eng.Log().Append(group, convlog.Draft{Kind: 99, Body: []byte("later")})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.eng.Publish(context.Background(), group, unk)
	w.settle(ps)
	if s := c.eng.Stubs(group); len(s) != 1 || s[0].Text != StubText {
		t.Fatalf("c's stubs: %+v", s)
	}
}

// A mention reaches a member who is offline when it was written, over the same path as any message
// (the outbox, the mailbox), and is marked and counted when it arrives.
func TestMentionReachesAnOfflineMember(t *testing.T) {
	w, ps := newChatWorld(t, 3)
	a, b, c := ps[0], ps[1], ps[2]
	group := convlog.Group("g1")
	body, _ := proto.Marshal(&convlog.MemberBody{Action: convlog.MemberRoster, Roster: []*convlog.RosterEntry{
		{Member: a.pub[:], Role: "owner"}, {Member: b.pub[:], Role: "member"}, {Member: c.pub[:], Role: "member"},
	}})
	roster, _, err := a.eng.Log().AppendKept(group, convlog.Draft{Kind: convlog.KindMember, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.eng.Publish(context.Background(), group, roster)
	w.settle(ps)

	w.down[c.tw.Device.ID()] = true
	cid := types.PeerPubKeyToID(types.PeerPublicKey(c.pub))
	raw, _, err := a.eng.Log().AppendMessageMentioning(group, "", "c, look at this", convlog.Mentions{Who: []types.PeerID{cid}}, convlog.EventID{}, convlog.EventID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.eng.Publish(context.Background(), group, raw)
	for round := 0; round < 5; round++ {
		_ = a.svc.Tick(context.Background())
		w.clock.Add(3 * time.Second)
	}
	if m, _ := c.eng.Log().Meta(group); m.Unread != 0 {
		t.Fatal("it arrived while c was offline")
	}
	delete(w.down, c.tw.Device.ID())
	w.settle(ps)

	mc, _ := c.eng.Log().Meta(group)
	mb, _ := b.eng.Log().Meta(group)
	if mc.Unread != 1 || mc.Mentions != 1 || mb.Unread != 1 || mb.Mentions != 0 {
		t.Fatalf("c: unread %d mentions %d; b: unread %d mentions %d", mc.Unread, mc.Mentions, mb.Unread, mb.Mentions)
	}
	msgs, _, _ := c.eng.Log().History(group, "", 10)
	if len(msgs) != 1 || !msgs[0].Mentioned {
		t.Fatalf("c sees %+v", msgs)
	}
}
