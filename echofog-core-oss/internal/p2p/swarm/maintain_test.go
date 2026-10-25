package swarm

import (
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func newTestSwarm(t *testing.T) (*Swarm, *network.QuicTransport) {
	t.Helper()
	_, sw, tr, st := testSwarmInit(t, "127.0.0.1:0")
	t.Cleanup(func() {
		sw.Close()
		tr.Close()
		st.CloseAndClear()
	})
	return sw, tr
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func connect(t *testing.T, from *Swarm, to *Swarm, toTr *network.QuicTransport) {
	t.Helper()
	if err := from.netTransport.DialEarly(from.ctx, toTr.Addr().String()); err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitFor(t, "connection", func() bool { return from.ThisIsActivePeer(to.selfID) && to.ThisIsActivePeer(from.selfID) })
}

func randomPub() types.PeerPublicKey {
	var pub types.PeerPublicKey
	copy(pub[:], types.NewMessageID())
	copy(pub[16:], types.NewMessageID())
	return pub
}

func TestMaintainBackoffGrowsAndResets(t *testing.T) {
	var m maintainState
	now := time.Now()
	if !m.due(now) || m.backoff != maintainBackoffMin {
		t.Fatalf("the first round runs at once, then waits %v (got %v)", maintainBackoffMin, m.backoff)
	}
	if m.due(now.Add(time.Second)) {
		t.Fatal("no second round before the backoff")
	}
	want := maintainBackoffMin
	at := now
	for i := 0; i < 10; i++ {
		at = at.Add(want * 2) // past any jitter
		if !m.due(at) {
			t.Fatalf("round %d not due", i)
		}
		want = min(want*2, maintainBackoffMax)
		if m.backoff != want {
			t.Fatalf("round %d: backoff %v, want %v", i, m.backoff, want)
		}
		if wait := m.next.Sub(at); wait < want*8/10 || wait > want*12/10 {
			t.Fatalf("round %d: next round in %v, outside the jitter of %v", i, wait, want)
		}
	}
	m.reset()
	if !m.due(at.Add(time.Millisecond)) {
		t.Fatal("after reset a shortage is answered at once")
	}
}

func TestPingHandlerAnswersAndLivePeerCountsAsAlive(t *testing.T) {
	a, _ := newTestSwarm(t)
	b, trB := newTestSwarm(t)
	connect(t, a, b, trB)

	if _, err := a.PingPeer(b.selfID, ""); err != nil {
		t.Fatalf("ping of a connected peer: %v", err)
	}
	resp, msg, err := a.SendMessageWithRequest(b.selfID, net_types.TypePing, &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: 42}},
	})
	if err != nil || resp != net_types.TypePong || msg.GetPong().GetNonce() != 42 {
		t.Fatalf("ping RPC: %v %v %v", resp, msg, err)
	}

	// a node of an older version has no ping handler: its live connection still proves it is there
	b.rpcMu.Lock()
	delete(b.rpcHandlers, net_types.TypePing)
	b.rpcMu.Unlock()
	if _, err := a.PingPeer(b.selfID, ""); err != nil {
		t.Fatalf("a live connection must count as alive: %v", err)
	}

	b.Close()
	waitFor(t, "disconnect", func() bool { return !a.ThisIsActivePeer(b.selfID) })
	if _, err := a.PingPeer(b.selfID, ""); err == nil {
		t.Fatal("a gone peer without an address is not alive")
	}
}

func TestPeerExchangeIntroducesPeers(t *testing.T) {
	a, trA := newTestSwarm(t)
	b, trB := newTestSwarm(t)
	c, _ := newTestSwarm(t)

	var mu sync.Mutex
	learned := map[types.PeerID]string{}
	c.OnPeerLearned(func(pub types.PeerPublicKey, addr string, _ handshake.Capability) {
		mu.Lock()
		learned[types.PeerPubKeyToID(pub)] = addr
		mu.Unlock()
	})

	connect(t, a, b, trB) // a dials b: b knows a only as an inbound peer, at the port a announced
	if got := b.GetPeer(a.selfID).Addr(); got != trA.Addr().String() {
		t.Fatalf("b has a at %q, want its listen address %q", got, trA.Addr().String())
	}
	connect(t, c, b, trB)

	// c asked b for peers: b named a, and c (short of peers) connected to it
	waitFor(t, "c learns a through b", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return learned[a.selfID] == trA.Addr().String()
	})
	waitFor(t, "c connects to a", func() bool { return c.ThisIsActivePeer(a.selfID) })
	if e, err := c.peerStore.GetPeer(a.selfID); err != nil || e.LastKnownAddr != trA.Addr().String() {
		t.Fatalf("c stored a: %+v %v", e, err)
	}
}

func TestPeerExchangeSharesOnlyDialablePeers(t *testing.T) {
	a, _ := newTestSwarm(t)
	b, trB := newTestSwarm(t)
	connect(t, a, b, trB)

	b.SetAddrPolicy(network.AddrPolicy{}) // public only: a is on loopback
	if got := b.shareablePeers(types.PeerID{}, 16).Peers; len(got) != 0 {
		t.Fatalf("shared %d loopback peers under a public-only policy", len(got))
	}
	b.SetAddrPolicy(network.AddrPolicy{AllowLoopback: true})
	if got := b.shareablePeers(a.selfID, 16).Peers; len(got) != 0 {
		t.Fatal("the asker is never told about itself")
	}
	if got := b.shareablePeers(types.PeerID{}, 16).Peers; len(got) != 1 || len(got[0].PubKey) != 32 {
		t.Fatalf("got %v", got)
	}
}

func TestLearnPeersFiltersAddresses(t *testing.T) {
	s, _ := newTestSwarm(t)
	s.SetAddrPolicy(network.AddrPolicy{})
	s.SetAutoDial(false) // only what is learned matters here, not dialing it

	entry := func(addr string) (*internal_pb.PeerList_Peer, types.PeerID) {
		pub := randomPub()
		id := types.PeerPubKeyToID(pub)
		return &internal_pb.PeerList_Peer{Id: id[:], PubKey: pub[:], LastKnownAddr: addr, Caps: uint32(handshake.CapRelay)}, id
	}
	list := &internal_pb.PeerList{}
	for _, bad := range []string{"10.1.2.3:4242", "192.168.0.7:4242", "100.64.0.1:4242", "127.0.0.1:4242", "", "[fe80::1]:4242"} {
		e, _ := entry(bad)
		list.Peers = append(list.Peers, e)
	}
	forged, _ := entry("203.0.113.8:443")
	forged.Id = make([]byte, 32) // an ID that is not the key's
	list.Peers = append(list.Peers, forged)
	good, goodID := entry("203.0.113.7:443")
	list.Peers = append(list.Peers, good)
	self := s.GetMyPubKey()
	list.Peers = append(list.Peers, &internal_pb.PeerList_Peer{PubKey: self[:], LastKnownAddr: "203.0.113.9:443"})

	var got []string
	s.OnPeerLearned(func(_ types.PeerPublicKey, addr string, _ handshake.Capability) { got = append(got, addr) })
	if n := s.learnPeers(types.PeerID{}, list); n != 1 || len(got) != 1 || got[0] != "203.0.113.7:443" {
		t.Fatalf("took %d %v: only the public, well-formed entry passes", n, got)
	}
	if _, err := s.peerStore.GetPeer(goodID); err != nil {
		t.Fatal("the learned peer is stored for later")
	}
}

func TestMaintainerRedialsKnownPeersAndCountsFailures(t *testing.T) {
	a, _ := newTestSwarm(t)
	b, trB := newTestSwarm(t)

	// a knows b and a dead peer from before
	_ = a.peerStore.SavePeer(b.GetMyPubKey(), trB.Addr().String(), 100)
	deadPub := randomPub()
	deadID := types.PeerPubKeyToID(deadPub)
	_ = a.peerStore.SavePeer(deadPub, "127.0.0.1:1", 100)

	if !a.NeedsPeers() {
		t.Fatal("a node without peers needs some")
	}
	a.maint.reset()
	if n := a.MaintainPeers(); n < 2 {
		t.Fatalf("dialed %d, want both known peers", n)
	}
	waitFor(t, "a reconnects to b", func() bool { return a.ThisIsActivePeer(b.selfID) })
	waitFor(t, "the dead peer's failure is noted", func() bool {
		e, err := a.peerStore.GetPeer(deadID)
		return err == nil && e.FailCount == 1
	})
	if n := a.MaintainPeers(); n != 0 {
		t.Fatal("the next round waits for the backoff")
	}
	if c := a.historyCandidates(10); len(c) != 0 {
		t.Fatalf("connected and recently failed peers are no candidates: %v", c)
	}

	a.SetAutoDial(false)
	a.maint.reset()
	if a.MaintainPeers() != 0 {
		t.Fatal("no_auto_dial turns the maintainer off")
	}
}

func TestBootstrapListIsCopied(t *testing.T) {
	s, _ := newTestSwarm(t)
	in := []string{"a", "b"}
	s.SetBootstrapNodes(in)
	in[0] = "changed"
	out := s.BootstrapNodes()
	out[1] = "changed"
	if got := s.BootstrapNodes(); got[0] != "a" || got[1] != "b" {
		t.Fatalf("the list is shared with callers: %v", got)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.SetBootstrapNodes([]string{"x"}) }()
		go func() { defer wg.Done(); _ = s.BootstrapNodes() }()
	}
	wg.Wait()
}

// Dialing a bootstrap node we are connected to must not open a second connection: it replaced
// the first, and the relay circuits on the first (calls, a VPN through a relay) died with it.
func TestBootstrapSkipsConnectedNodes(t *testing.T) {
	a, _ := newTestSwarm(t)
	b, bTr := newTestSwarm(t)
	connect(t, a, b, bTr)
	before := a.GetPeer(b.selfID)
	require := func(ok bool, msg string) {
		t.Helper()
		if !ok {
			t.Fatal(msg)
		}
	}
	require(before != nil, "not connected")

	a.Bootstrap([]string{network.JoinPeerAddr(bTr.Addr().String(), b.selfID)})
	time.Sleep(1500 * time.Millisecond)
	require(a.GetPeer(b.selfID) == before, "the connection to an already connected bootstrap node was replaced")
}
