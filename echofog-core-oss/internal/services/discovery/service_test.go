package discovery

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/netip"
	"sync"
	"testing"
	"time"

	netpkg "github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type sent struct {
	to types.PeerID
	mt net_types.MessageType
}

type fakeNet struct {
	mu        sync.Mutex
	self      types.PeerID
	connected map[types.PeerID]bool
	sent      []sent
	saved     map[types.PeerID]string
}

func newFakeNet() *fakeNet {
	return &fakeNet{self: types.PeerID{0xee}, connected: map[types.PeerID]bool{}, saved: map[types.PeerID]string{}}
}

func (f *fakeNet) SelfID() types.PeerID { return f.self }
func (f *fakeNet) Connected(id types.PeerID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[id]
}
func (f *fakeNet) Send(id types.PeerID, mt net_types.MessageType, _ *internal_pb.MessageData) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{id, mt})
	return nil
}
func (f *fakeNet) Shareable(types.PeerID, int) []*internal_pb.PeerInfo { return nil }
func (f *fakeNet) Accept(addr string) bool {
	ap, err := netip.ParseAddrPort(netpkg.ParseAddrInfo(addr).Addr) // like the real policy, blind to the query
	return err == nil && ap.Addr().IsGlobalUnicast() && !ap.Addr().IsPrivate()
}
func (f *fakeNet) Banned(types.PeerID) bool { return false }
func (f *fakeNet) SaveDiscovered(pub types.PeerPublicKey, addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[types.PeerPubKeyToID(pub)] = addr
	return true
}

func id(i int) types.PeerID {
	var p types.PeerID
	p[0], p[1], p[2] = byte(i), byte(i>>8), 0x5a
	return p
}

func req(origin types.PeerID) *internal_pb.MessageData {
	return &internal_pb.MessageData{OriginId: origin[:], Payload: &internal_pb.MessageData_PeerReq{PeerReq: &internal_pb.PeerRequest{Count: 1000}}}
}

func TestRequestFromAStrangerIsNeverBroadcast(t *testing.T) {
	net := newFakeNet()
	d := newService(nil, nil, net, types.PeerPublicKey{})
	sender := id(1)
	net.connected[sender] = true

	d.Handle(req(id(2)), sender) // on behalf of somebody not connected
	d.Handle(req(id(3)), id(3))  // straight from a peer that is not connected
	if len(net.sent) != 0 {
		t.Fatalf("answered %d requests for peers not asking themselves", len(net.sent))
	}

	d.Handle(req(sender), sender)
	if len(net.sent) != 1 || net.sent[0].to != sender {
		t.Fatalf("the connected asker gets one direct answer, got %+v", net.sent)
	}
	d.Handle(req(sender), sender)
	if len(net.sent) != 1 {
		t.Fatal("a second request inside AnswerEvery is ignored")
	}
}

func TestAnswersShareOneNodeWideBudget(t *testing.T) {
	net := newFakeNet()
	d := newService(nil, nil, net, types.PeerPublicKey{})
	for i := 0; i < 3*AnswersPerMinute; i++ {
		net.connected[id(i)] = true
		d.Handle(req(id(i)), id(i))
	}
	if n := len(net.sent); n != AnswersPerMinute {
		t.Fatalf("answered %d fresh peer IDs, the budget is %d a minute", n, AnswersPerMinute)
	}
	if len(d.giver.lastAns) > maxTracked {
		t.Fatal("the per-peer table outgrew its bound")
	}
}

func peerInfo(t *testing.T, addr string) (*internal_pb.PeerInfo, types.PeerID) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	return &internal_pb.PeerInfo{PubKey: pub, Address: addr}, types.PeerPubKeyToID(pk)
}

func res(peers ...*internal_pb.PeerInfo) *internal_pb.MessageData {
	return &internal_pb.MessageData{Payload: &internal_pb.MessageData_PeerRes{PeerRes: &internal_pb.PeerResponse{Peers: peers}}}
}

func TestOnlyAskedForAnswersAreTakenIn(t *testing.T) {
	net := newFakeNet()
	d := newService(nil, nil, net, types.PeerPublicKey{})
	from := id(1)
	good, goodID := peerInfo(t, "203.0.113.5:4242")

	d.Handle(res(good), from)
	if len(net.saved) != 0 {
		t.Fatal("a peer list nobody asked for was stored")
	}

	if err := d.RequestPeers(from, 8); err != nil {
		t.Fatal(err)
	}
	if len(net.sent) != 1 || net.sent[0].mt != net_types.TypeGetPeerRequest {
		t.Fatalf("request not sent: %+v", net.sent)
	}
	lan, lanID := peerInfo(t, "192.168.1.5:4242")
	loop, _ := peerInfo(t, "127.0.0.1:4242")
	mcast, _ := peerInfo(t, "224.0.0.1:4242")
	unspec, _ := peerInfo(t, "0.0.0.0:4242")
	d.Handle(res(lan, loop, mcast, unspec, good), from)
	if net.saved[goodID] == "" || net.saved[lanID] != "" || len(net.saved) != 1 {
		t.Fatalf("want only the public address stored, got %v", net.saved)
	}

	extra, extraID := peerInfo(t, "203.0.113.6:4242")
	d.Handle(res(extra), from)
	if net.saved[extraID] != "" {
		t.Fatal("one request is answered once")
	}
}

func TestAnswerEntriesAreCapped(t *testing.T) {
	net := newFakeNet()
	d := newService(nil, nil, net, types.PeerPublicKey{})
	from := id(1)
	_ = d.RequestPeers(from, 0)
	var list []*internal_pb.PeerInfo
	for i := 0; i < 5*MaxPeersPerResponse; i++ {
		p, _ := peerInfo(t, "203.0.113.7:4242")
		list = append(list, p)
	}
	d.Handle(res(list...), from)
	if len(net.saved) != MaxPeersPerResponse {
		t.Fatalf("stored %d peers from one answer, want %d", len(net.saved), MaxPeersPerResponse)
	}
}

func TestOutstandingRequestsAreBounded(t *testing.T) {
	net := newFakeNet()
	d := newService(nil, nil, net, types.PeerPublicKey{})
	now := time.Now()
	for i := 0; i < 2*maxOutstanding; i++ {
		d.getter.expect(id(i), now)
	}
	if len(d.getter.pending) > maxOutstanding {
		t.Fatal("waiting requests outgrew their bound")
	}
	if !d.getter.expect(id(1<<15), now.Add(RequestTimeout+time.Second)) {
		t.Fatal("expired requests make room")
	}
}

// learnNet is a fakeNet that also learns what an address carries, as the swarm does.
type learnNet struct {
	*fakeNet
	sni map[types.PeerID]string
}

func (l *learnNet) LearnPeerAddr(id types.PeerID, addr string) string {
	info := netpkg.ParseAddrInfo(addr)
	if info.SNI != "" {
		l.sni[id] = info.SNI
	}
	return info.Addr
}

// A peer list entry with the SNI of a camouflage node is stored as the plain address and the SNI
// is kept: the address with "?sni=..." left on it could not be dialed later, and without the SNI
// the dial would be noise.
func TestPeerListKeepsTheSNIAndStoresAPlainAddress(t *testing.T) {
	for _, withLearner := range []bool{true, false} {
		base := newFakeNet()
		var net network = base
		l := &learnNet{fakeNet: base, sni: map[types.PeerID]string{}}
		if withLearner {
			net = l
		}
		d := newService(nil, nil, net, types.PeerPublicKey{})
		from := id(1)
		if err := d.RequestPeers(from, 8); err != nil {
			t.Fatal(err)
		}
		cam, camID := peerInfo(t, "203.0.113.5:4242?sni=cover.lab&srv=1")
		d.Handle(res(cam), from)
		if got := base.saved[camID]; got != "203.0.113.5:4242" {
			t.Fatalf("learner=%v: stored %q, want the plain address", withLearner, got)
		}
		if withLearner && l.sni[camID] != "cover.lab" {
			t.Fatalf("the SNI was not kept: %v", l.sni)
		}
	}
}
