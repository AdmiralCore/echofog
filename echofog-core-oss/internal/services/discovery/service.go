package discovery

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	netpkg "github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/gossip"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	guuid "github.com/google/uuid"
)

// Peer requests and answers go only between two connected peers: a request is answered straight
// to the peer that sent it, never broadcast, and an answer is taken in only from a peer this node
// asked. Peer IDs cost nothing, so on top of the per-peer limits the node keeps one budget for
// all peers together.
const (
	// MaxPeersPerResponse is the most peers given in one answer, and the most taken from one.
	MaxPeersPerResponse = 16
	// AnswerEvery is how often one peer gets an answer.
	AnswerEvery = 10 * time.Second
	// AnswersPerMinute is how many answers the node gives a minute, all peers together.
	AnswersPerMinute = 120
	// RequestTimeout is how long an answer to our request is waited for.
	RequestTimeout = 30 * time.Second

	maxTracked     = 4096 // peers remembered for the per-peer interval
	maxOutstanding = 256  // requests of ours waiting for an answer
)

// network is the part of the swarm discovery uses.
type network interface {
	SelfID() types.PeerID
	Connected(id types.PeerID) bool
	Send(id types.PeerID, mt net_types.MessageType, msg *internal_pb.MessageData) error
	// Shareable lists up to n connected peers whose address may be handed on, not exclude.
	Shareable(exclude types.PeerID, n int) []*internal_pb.PeerInfo
	// Accept reports whether an address another node told us about may be stored.
	Accept(addr string) bool
	Banned(id types.PeerID) bool
	SaveDiscovered(pub types.PeerPublicKey, addr string) bool
}

type DiscoveryService struct {
	storage  storage.Storage
	gsp      *gossip.Manager
	net      network
	getter   *peerGetter
	giver    *peerGiver
	myPubKey types.PeerPublicKey
}

func NewDiscoveryService(storage storage.Storage, gsp *gossip.Manager, swarm *swarm.Swarm, myPubKey types.PeerPublicKey) *DiscoveryService {
	return newService(storage, gsp, swarmNet{swarm}, myPubKey)
}

func newService(storage storage.Storage, gsp *gossip.Manager, net network, myPubKey types.PeerPublicKey) *DiscoveryService {
	ds := &DiscoveryService{
		storage:  storage,
		gsp:      gsp,
		net:      net,
		myPubKey: myPubKey,
	}
	ds.getter = newPeerGetter(ds)
	ds.giver = newPeerGiver(ds)
	return ds
}

func (d *DiscoveryService) Name() string {
	return "DiscoveryService"
}

func (d *DiscoveryService) Start(ctx context.Context) error {
	return nil
}

func (d *DiscoveryService) Stop() error {
	return nil
}

func (d *DiscoveryService) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	switch msg.Payload.(type) {
	case *internal_pb.MessageData_PeerRes:
		d.getter.Handle(msg, peerID)
	case *internal_pb.MessageData_PeerReq:
		d.giver.Handle(msg, peerID)
	}
}

func (d *DiscoveryService) GetSubscribedTypes() []interface{} {
	return []interface{}{
		(*internal_pb.MessageData_PeerReq)(nil),
		(*internal_pb.MessageData_PeerRes)(nil),
	}
}

// RequestPeers asks a connected peer for some of its peers; its answer is taken in when it comes.
func (d *DiscoveryService) RequestPeers(peer types.PeerID, count uint32) error {
	if count == 0 || count > MaxPeersPerResponse {
		count = MaxPeersPerResponse
	}
	if !d.getter.expect(peer, time.Now()) {
		return nil // already asked, or too many requests waiting
	}
	mesID := guuid.New()
	myID := d.net.SelfID()
	err := d.net.Send(peer, net_types.TypeGetPeerRequest, &internal_pb.MessageData{
		MessageId: mesID[:],
		OriginId:  myID[:],
		TargetId:  peer[:],
		Timestamp: uint64(time.Now().UnixNano()),
		HopLimit:  1,
		Payload:   &internal_pb.MessageData_PeerReq{PeerReq: &internal_pb.PeerRequest{Count: count}},
	})
	if err != nil {
		d.getter.forget(peer)
	}
	return err
}

type peerGetter struct {
	service *DiscoveryService

	mu      sync.Mutex
	pending map[types.PeerID]time.Time // peer we asked -> when the answer stops being awaited
}

func newPeerGetter(service *DiscoveryService) *peerGetter {
	return &peerGetter{service: service, pending: make(map[types.PeerID]time.Time)}
}

// expect records a request to peer; false when one is already waiting or too many are.
func (g *peerGetter) expect(peer types.PeerID, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until, ok := g.pending[peer]; ok && now.Before(until) {
		return false
	}
	if len(g.pending) >= maxOutstanding {
		for id, until := range g.pending {
			if !now.Before(until) {
				delete(g.pending, id)
			}
		}
		if len(g.pending) >= maxOutstanding {
			return false
		}
	}
	g.pending[peer] = now.Add(RequestTimeout)
	return true
}

func (g *peerGetter) forget(peer types.PeerID) {
	g.mu.Lock()
	delete(g.pending, peer)
	g.mu.Unlock()
}

// take reports whether an answer from peer was asked for, and uses the request up.
func (g *peerGetter) take(peer types.PeerID, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.pending[peer]
	delete(g.pending, peer)
	return ok && now.Before(until)
}

func (g *peerGetter) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	msgPeer, ok := msg.Payload.(*internal_pb.MessageData_PeerRes)
	if !ok || msgPeer.PeerRes == nil {
		return
	}
	if !g.take(peerID, time.Now()) {
		slog.Debug("discovery: dropped a peer list nobody asked for", "from", peerID)
		return
	}

	net := g.service.net
	self := net.SelfID()
	entries := msgPeer.PeerRes.Peers
	if len(entries) > MaxPeersPerResponse {
		entries = entries[:MaxPeersPerResponse]
	}
	for _, p := range entries {
		pubKey, err := types.ToPeerPubKey(p.PubKey)
		if err != nil || !net.Accept(p.Address) {
			continue
		}
		id := types.PeerPubKeyToID(pubKey)
		if id == self || id == peerID || net.Banned(id) {
			continue
		}
		addr := p.Address
		if l, ok := net.(interface {
			LearnPeerAddr(types.PeerID, string) string
		}); ok {
			addr = l.LearnPeerAddr(id, addr) // SNI and server flag kept, the plain address stored
		} else {
			addr = netpkg.ParseAddrInfo(addr).Addr
		}
		net.SaveDiscovered(pubKey, addr)
	}
}

type peerGiver struct {
	service *DiscoveryService

	mu       sync.Mutex
	lastAns  map[types.PeerID]time.Time
	tokens   float64
	refilled time.Time
}

func newPeerGiver(service *DiscoveryService) *peerGiver {
	return &peerGiver{service: service, lastAns: make(map[types.PeerID]time.Time), tokens: AnswersPerMinute, refilled: time.Now()}
}

// allow applies the per-peer interval and the node-wide budget to one request.
func (g *peerGiver) allow(peer types.PeerID, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.lastAns[peer]; ok && now.Sub(last) < AnswerEvery {
		return false
	}
	g.tokens = min(g.tokens+now.Sub(g.refilled).Minutes()*AnswersPerMinute, AnswersPerMinute)
	g.refilled = now
	if g.tokens < 1 {
		return false
	}
	if _, ok := g.lastAns[peer]; !ok && len(g.lastAns) >= maxTracked {
		for id, t := range g.lastAns {
			if now.Sub(t) >= AnswerEvery {
				delete(g.lastAns, id)
			}
		}
		if len(g.lastAns) >= maxTracked {
			return false
		}
	}
	g.lastAns[peer] = now
	g.tokens--
	return true
}

// Handle answers a peer request, only to the connected peer that sent it on its own behalf.
func (g *peerGiver) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	msgPeer, ok := msg.Payload.(*internal_pb.MessageData_PeerReq)
	if !ok || msgPeer.PeerReq == nil {
		return
	}
	net := g.service.net
	if origin, err := types.ToPeerID(msg.OriginId); err != nil || origin != peerID || !net.Connected(peerID) {
		return // asked on somebody else's behalf: the answer would go out to the whole network
	}
	if !g.allow(peerID, time.Now()) {
		return
	}

	n := int(msgPeer.PeerReq.Count)
	if n <= 0 || n > MaxPeersPerResponse {
		n = MaxPeersPerResponse
	}
	mesID := guuid.New()
	myID := net.SelfID()
	resp := &internal_pb.MessageData{
		MessageId: mesID[:],
		OriginId:  myID[:],
		TargetId:  peerID[:],
		Timestamp: uint64(time.Now().UnixNano()),
		HopLimit:  1,
		Payload: &internal_pb.MessageData_PeerRes{
			PeerRes: &internal_pb.PeerResponse{Peers: net.Shareable(peerID, n)},
		},
	}
	if err := net.Send(peerID, net_types.TypeGetPeerResponse, resp); err != nil {
		slog.Debug("discovery: could not answer a peer request", "to", peerID, "err", err)
	}
}

// swarmNet is the network discovery runs on in a node.
type swarmNet struct{ s *swarm.Swarm }

func (n swarmNet) SelfID() types.PeerID           { return n.s.GetSelfID() }
func (n swarmNet) Connected(id types.PeerID) bool { return n.s.ThisIsActivePeer(id) }
func (n swarmNet) Accept(addr string) bool        { return n.s.AddrPolicy().Shareable(addr) }
func (n swarmNet) Banned(id types.PeerID) bool    { return n.s.CheckOnBan(id) }
func (n swarmNet) Send(id types.PeerID, mt net_types.MessageType, msg *internal_pb.MessageData) error {
	return n.s.SendDataForPeer(id, mt, msg)
}

func (n swarmNet) SaveDiscovered(pub types.PeerPublicKey, addr string) bool {
	if ps := n.s.GetPeerStore(); ps != nil {
		return ps.SaveDiscovered(pub, addr)
	}
	return false
}

func (n swarmNet) SetPeerSNI(id types.PeerID, sni string) {
	n.s.SetPeerSNI(id, sni)
}

func (n swarmNet) LearnPeerAddr(id types.PeerID, addr string) string {
	return n.s.LearnPeerAddr(id, addr)
}

func (n swarmNet) Shareable(exclude types.PeerID, max int) []*internal_pb.PeerInfo {
	policy := n.s.AddrPolicy()
	peers := n.s.GetAllPeers()
	rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })
	out := make([]*internal_pb.PeerInfo, 0, max)
	for _, p := range peers {
		if len(out) >= max {
			break
		}
		if p.ID() == exclude || !p.Alive() || !policy.Shareable(p.Addr()) {
			continue
		}
		pub := p.PubKey()
		addr := p.Addr()
		addr = n.s.AnnotatedAddr(p.ID(), addr)
		out = append(out, &internal_pb.PeerInfo{PubKey: pub[:], Address: addr})
	}
	return out
}
