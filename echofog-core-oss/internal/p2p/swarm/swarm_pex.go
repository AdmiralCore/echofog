package swarm

import (
	"bytes"
	"errors"
	"math/rand/v2"

	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Peer exchange: right after two nodes connect, each asks the other for a few of its peers. It
// is how two seeds bootstrapped from different seed lists come to know each other, through any
// client that knows both, and how a node learns more than its bootstrap list gave it.
const (
	pexAsk           = 8  // peers asked for on connect
	pexMaxServe      = 16 // most peers given in one answer
	pexMaxDialsPerEx = 4  // learned peers dialed at once per exchange
	minRelayingPeers = 3  // relays or signalers the node wants among its peers
)

// exchangePeers asks a newly connected peer for some of its peers. A peer of an older version
// has no handler for it and closes the request; nothing is learned then.
func (s *Swarm) exchangePeers(p *Peer) {
	req := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  s.selfID[:],
		Payload:   &internal_pb.MessageData_PeerReq{PeerReq: &internal_pb.PeerRequest{Count: pexAsk}},
	}
	respType, resp, err := s.SendMessageWithRequest(p.ID(), net_types.TypeGetPeerRequest, req)
	if err != nil || respType != net_types.TypeGetPeerResponse {
		return
	}
	s.learnPeers(p.ID(), resp.GetPeerList())
}

// handlePeerExchange answers a peer exchange request with a few connected peers that can be
// dialed, relays and signalers first.
func (s *Swarm) handlePeerExchange(msg *internal_pb.MessageData, from types.PeerID) (net_types.MessageType, *internal_pb.MessageData, error) {
	req := msg.GetPeerReq()
	if req == nil {
		return 0, nil, errors.New("peer exchange: no request")
	}
	n := int(req.Count)
	if n <= 0 || n > pexMaxServe {
		n = pexMaxServe
	}
	return net_types.TypeGetPeerResponse, &internal_pb.MessageData{
		MessageId: msg.MessageId,
		OriginId:  s.selfID[:],
		Payload:   &internal_pb.MessageData_PeerList{PeerList: s.shareablePeers(from, n)},
	}, nil
}

// shareablePeers picks up to n connected peers to tell another node about: only peers with an
// address that passes the address policy, never the asker itself.
func (s *Swarm) shareablePeers(exclude types.PeerID, n int) *internal_pb.PeerList {
	policy := s.AddrPolicy()
	var pref, rest []*Peer
	for _, p := range s.GetAllPeers() {
		if p.ID() == exclude || !p.Alive() || !policy.Shareable(p.Addr()) {
			continue
		}
		if offersRelay(p.Caps()) {
			pref = append(pref, p)
		} else {
			rest = append(rest, p)
		}
	}
	rand.Shuffle(len(pref), func(i, j int) { pref[i], pref[j] = pref[j], pref[i] })
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })

	list := &internal_pb.PeerList{}
	for _, p := range append(pref, rest...) {
		if len(list.Peers) >= n {
			break
		}
		id, pub := p.ID(), p.PubKey()
		list.Peers = append(list.Peers, &internal_pb.PeerList_Peer{
			Id:            id[:],
			LastKnownAddr: p.Addr(),
			PubKey:        pub[:],
			Caps:          uint32(p.Caps()),
		})
	}
	return list
}

// learnPeers takes in what a peer told us: each listed peer with a valid key and an address
// that passes the policy is stored for later and handed to the OnPeerLearned callbacks. A few
// are dialed at once when this node needs peers or relays. Returns how many were taken in.
func (s *Swarm) learnPeers(from types.PeerID, list *internal_pb.PeerList) int {
	if list == nil {
		return 0
	}
	entries := list.Peers
	if len(entries) > pexMaxServe {
		entries = entries[:pexMaxServe]
	}
	policy := s.AddrPolicy()
	s.sourcesMu.RLock()
	learned := append([]func(types.PeerPublicKey, string, handshake.Capability){}, s.learned...)
	s.sourcesMu.RUnlock()

	var dial []DialCandidate
	taken := 0
	for _, e := range entries {
		pub, err := types.ToPeerPubKey(e.GetPubKey())
		if err != nil {
			continue
		}
		id := types.PeerPubKeyToID(pub)
		if (len(e.GetId()) > 0 && !bytes.Equal(e.GetId(), id[:])) || id == s.selfID || id == from {
			continue
		}
		addr := e.GetLastKnownAddr()
		if !policy.Shareable(addr) || s.CheckOnBan(id) {
			continue
		}
		taken++
		caps := handshake.Capability(e.GetCaps())
		if s.peerStore != nil {
			s.peerStore.SaveDiscovered(pub, addr)
		}
		for _, f := range learned {
			f(pub, addr, caps)
		}
		if len(dial) < pexMaxDialsPerEx && !s.ThisIsActivePeer(id) && s.wantsPeer(caps) {
			dial = append(dial, DialCandidate{ID: id, Addr: addr})
		}
	}
	if len(dial) > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.dialCandidates(dial)
		}()
	}
	return taken
}

// wantsPeer reports whether a learned peer is worth a connection now: the node is short of
// peers, or the peer relays and the node has few that do.
func (s *Swarm) wantsPeer(caps handshake.Capability) bool {
	if !s.AutoDial() {
		return false
	}
	return s.NeedsPeers() || (offersRelay(caps) && s.relayingPeers() < minRelayingPeers)
}
