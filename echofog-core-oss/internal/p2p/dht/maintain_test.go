package dht

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type testNode struct {
	dht *EchoFogDHT
	sw  *swarm.Swarm
	tr  *network.QuicTransport
	pub types.PeerPublicKey
	id  types.PeerID
}

func (n *testNode) addr() string { return n.tr.Addr().String() }

// newTestNode runs a swarm on loopback with a DHT routed to it, as the node does. Peer exchange
// and the maintainer do not dial: these tests build their topology by hand.
func newTestNode(t *testing.T) *testNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	im, st := testutil.SetupTestIdentity(t)
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	tr, err := network.NewQUICTransport("127.0.0.1:0", tlsCfg, network.GetQuicConfig(), im, config.CurrentProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.ListenEarly(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := im.GetPeerID()
	if err != nil {
		t.Fatal(err)
	}
	disp := dispatcher.NewDispatcher(ctx)
	go disp.Start()
	cfg := config.DefaultConfig("")
	cfg.Network.NoAutoDial = true
	sw := swarm.NewSwarm(ctx, tr, id, disp, im, tr.ConnChan(), st.St, cfg)
	d := NewEchoFogDHT(NewRoutingTable(sw.GetMyPubKey(), st.St, sw), sw)
	for _, typ := range d.GetSubscribedTypes() {
		disp.Registry(typ, d)
	}
	t.Cleanup(func() {
		_ = d.Stop()
		sw.Close()
		tr.Close()
		cancel()
		st.CloseAndClear()
	})
	return &testNode{dht: d, sw: sw, tr: tr, pub: sw.GetMyPubKey(), id: id}
}

func (n *testNode) dial(t *testing.T, to *testNode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.sw.AddPotentialPeerWithCtx(ctx, to.id, to.addr()); err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitUntil(t, "both sides registered", func() bool { return to.sw.ThisIsActivePeer(n.id) })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func randomPublicPeer() (types.PeerPublicKey, string) {
	var pub types.PeerPublicKey
	_, _ = rand.Read(pub[:])
	var b [2]byte
	_, _ = rand.Read(b[:])
	return pub, "203.0.113." + string('0'+rune(b[0]%9+1)) + ":" + "44" + string('0'+rune(b[1]%9+1))
}

func TestFindValueReferralsGoTowardTheKeysHash(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	for i := 0; i < 60; i++ {
		pub, addr := randomPublicPeer()
		b.dht.routingTable.AddOrUpdatePeer(pub, addr)
	}
	a.dial(t, b)

	key := []byte("bundle:" + b.id.String())
	msgID := types.NewMessageID()
	wait := make(chan *internal_pb.FindValueRes, 1)
	a.dht.pending.Store(string(msgID[:]), wait)
	if err := a.sw.SendDataForPeer(b.id, net_types.TypeFindValue, &internal_pb.MessageData{
		MessageId: msgID[:],
		Payload:   &internal_pb.MessageData_FindValueReq{FindValueReq: &internal_pb.FindValueReq{TargetKey: key}},
	}); err != nil {
		t.Fatal(err)
	}
	var res *internal_pb.FindValueRes
	select {
	case res = <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("no FindValue answer")
	}
	target := types.PeerID(sha256.Sum256(key))
	var want []*PeerNode
	for _, p := range b.dht.routingTable.GetClosestPeers(target, K+1) {
		if p.ID != a.id && len(want) < K {
			want = append(want, p)
		}
	}
	if len(res.ClosestPeers) != K {
		t.Fatalf("got %d referrals, want %d", len(res.ClosestPeers), K)
	}
	for i, info := range res.ClosestPeers {
		got := types.PeerPubKeyToID(types.PeerPublicKey(info.PubKey))
		if got == a.id {
			t.Fatal("the asker was referred to itself")
		}
		if got != want[i].ID {
			t.Fatalf("referral %d is not the %d-th closest to sha256(key): a non-ID key must not be read as the zero ID", i, i)
		}
	}
}

func TestFindPeerWalksThroughTheNetwork(t *testing.T) {
	a, b, c, d := newTestNode(t), newTestNode(t), newTestNode(t), newTestNode(t)
	// a knows b; b knows only of c; c knows only of d. Nobody is connected but a to b.
	a.dial(t, b)
	b.dht.routingTable.AddOrUpdatePeer(c.pub, c.addr())
	c.dht.routingTable.AddOrUpdatePeer(d.pub, d.addr())
	a.sw.SetAutoDial(true) // a's lookup has to connect to the peers it is referred to

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	addr, err := a.dht.FindPeer(ctx, d.id)
	if err != nil {
		t.Fatalf("FindPeer: %v", err)
	}
	if addr != d.addr() {
		t.Fatalf("found d at %q, want %q", addr, d.addr())
	}
	if !a.sw.ThisIsActivePeer(c.id) {
		t.Fatal("the lookup went through c")
	}
}

func TestSelfLookupOnJoinFillsTheTable(t *testing.T) {
	b, c, e, joiner := newTestNode(t), newTestNode(t), newTestNode(t), newTestNode(t)
	c.dial(t, b)
	e.dial(t, c)
	if err := joiner.dht.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	joiner.sw.SetAutoDial(true)

	joiner.dial(t, b) // the first connection: the joiner looks up its own ID
	waitUntil(t, "the joiner learns the network beyond its first peer", func() bool {
		known := map[types.PeerID]bool{}
		for _, id := range joiner.dht.KnownPeers() {
			known[id] = true
		}
		return known[b.id] && known[c.id] && known[e.id]
	})
}

func TestRoutingTableSurvivesARestart(t *testing.T) {
	st := testutil.NewTestingBadgerStorage(t)
	defer st.CloseAndClear()
	var self types.PeerPublicKey
	_, _ = rand.Read(self[:])
	rt := NewRoutingTable(self, st.St, &mockPinger{})
	for i := 0; i < 30; i++ {
		pub, addr := randomPublicPeer()
		rt.AddOrUpdatePeer(pub, addr)
	}
	want := rt.Len()
	if err := rt.Save(); err != nil {
		t.Fatal(err)
	}

	again := NewRoutingTable(self, st.St, &mockPinger{})
	if n := again.Load(); n != want || again.Len() != want {
		t.Fatalf("restored %d of %d peers", n, want)
	}
	for _, p := range rt.allPeers() {
		got := again.GetClosestPeers(p.ID, 1)
		if len(got) != 1 || got[0].ID != p.ID || got[0].Addr != p.Addr {
			t.Fatal("a restored peer lost its address")
		}
	}
}

func TestRoutingTableKeepsOnlyPolicyAddresses(t *testing.T) {
	var self types.PeerPublicKey
	_, _ = rand.Read(self[:])
	rt := NewRoutingTable(self, nil, &mockPinger{})
	for _, addr := range []string{"10.0.0.1:4242", "192.168.1.1:4242", "100.100.1.1:4242", "127.0.0.1:4242", ""} {
		pub, _ := randomPublicPeer()
		rt.AddOrUpdatePeer(pub, addr)
	}
	if rt.Len() != 0 {
		t.Fatalf("kept %d private or loopback addresses", rt.Len())
	}
	rt.SetAddrPolicy(network.AddrPolicy{AllowLoopback: true})
	pub, _ := randomPublicPeer()
	rt.AddOrUpdatePeer(pub, "127.0.0.1:4242")
	if rt.Len() != 1 {
		t.Fatal("a loopback test setup keeps loopback addresses")
	}
}

func TestRandomIDLandsInItsBucket(t *testing.T) {
	var self types.PeerID
	_, _ = rand.Read(self[:])
	for idx := 0; idx < IDLengthBits; idx++ {
		var r [32]byte
		_, _ = rand.Read(r[:])
		if got := getBucketIndex(self, randomIDInBucket(self, idx, r)); got != idx {
			t.Fatalf("an ID for bucket %d fell in bucket %d", idx, got)
		}
	}
}

func TestStaleBucketsAreRefreshedUpToTheDeepest(t *testing.T) {
	var self types.PeerPublicKey
	_, _ = rand.Read(self[:])
	rt := NewRoutingTable(self, nil, &mockPinger{})
	var r [32]byte
	far := randomIDInBucket(rt.myID, 0, r)
	near := randomIDInBucket(rt.myID, 5, r)
	rt.buckets[0].Peers = append(rt.buckets[0].Peers, &PeerNode{ID: far, Addr: "203.0.113.1:1"})
	rt.buckets[5].Peers = append(rt.buckets[5].Peers, &PeerNode{ID: near, Addr: "203.0.113.2:1"})

	now := time.Now()
	if got := rt.staleBuckets(now, 10*time.Minute); len(got) != 6 {
		t.Fatalf("stale %v, want buckets 0..5", got)
	}
	rt.markLookup(near, now)
	got := rt.staleBuckets(now.Add(time.Minute), 10*time.Minute)
	if len(got) != 5 || got[len(got)-1] != 4 {
		t.Fatalf("stale %v: a looked-up bucket is fresh", got)
	}
}

func TestMergeClosestKeepsKInOrder(t *testing.T) {
	var target, self types.PeerID
	_, _ = rand.Read(target[:])
	_, _ = rand.Read(self[:])
	var list, add []*PeerNode
	for i := 0; i < K; i++ {
		var a, b types.PeerID
		_, _ = rand.Read(a[:])
		_, _ = rand.Read(b[:])
		list = append(list, &PeerNode{ID: a})
		add = append(add, &PeerNode{ID: b})
	}
	add = append(add, list[0], &PeerNode{ID: self})
	out := mergeClosest(list, add, target, self)
	if len(out) != K {
		t.Fatalf("got %d, want %d", len(out), K)
	}
	seen := map[types.PeerID]bool{}
	for i, p := range out {
		if p.ID == self || seen[p.ID] {
			t.Fatal("self or a duplicate in the shortlist")
		}
		seen[p.ID] = true
		if i > 0 && isCloser(p.ID, out[i-1].ID, target) {
			t.Fatal("not ordered by distance")
		}
	}
}

func TestSetStoresOnReachablePeersWhenTheClosestAreDead(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	// a's table is full of peers behind NAT at addresses that no longer answer, most of them
	// closer to the key than b
	for i := 0; i < 60; i++ {
		pub, addr := randomPublicPeer()
		a.dht.routingTable.AddOrUpdatePeer(pub, addr)
	}
	a.dial(t, b)
	a.dht.routingTable.AddOrUpdatePeer(b.pub, b.addr())

	key := swarm.BundleKey(a.id)
	if err := a.dht.Set(key, []byte("bundle")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the record lands on the one peer that can be reached", func() bool {
		raw, err := b.sw.GetStorage().Get(RecordKey(key))
		return err == nil && len(raw) > 0
	})
}

// The same table full of dead peers must not hide a value a connected peer holds: a lookup starts
// from the peers it can reach.
func TestGetFindsTheValueWhenTheClosestAreDead(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	b.dial(t, a)
	key := swarm.BundleKey(b.id)
	if err := b.dht.Set(key, []byte("advert")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the record is on a", func() bool {
		raw, err := a.sw.GetStorage().Get(RecordKey(key))
		return err == nil && len(raw) > 0
	})

	c := newTestNode(t)
	for i := 0; i < 60; i++ {
		pub, addr := randomPublicPeer()
		c.dht.routingTable.AddOrUpdatePeer(pub, addr)
	}
	c.dial(t, a)
	c.dht.routingTable.AddOrUpdatePeer(a.pub, a.addr())

	got, err := c.dht.GetFresh(key)
	if err != nil || string(got) != "advert" {
		t.Fatalf("GetFresh = %q, %v", got, err)
	}
	got, err = c.dht.Get(key)
	if err != nil || string(got) != "advert" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}
