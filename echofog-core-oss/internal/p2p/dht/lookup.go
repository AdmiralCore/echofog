package dht

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	// K is how many closest peers a lookup converges on and an answer carries.
	K = MaxBucketSize

	nodeQueryTimeout = 3 * time.Second
	lookupDialWait   = 2 * time.Second
)

// iterativeFindNode walks toward target the Kademlia way: it asks the Alpha closest peers it
// knows that it has not asked yet, merges what they answer, and repeats until the K closest
// it knows have all been asked (or the query budget is spent). With want set it stops as soon
// as some answer names that peer and returns it. The returned list is the closest peers found.
func (d *EchoFogDHT) iterativeFindNode(ctx context.Context, target types.PeerID, want *types.PeerID) ([]*PeerNode, *PeerNode) {
	d.routingTable.markLookup(target, time.Now())
	shortlist := d.routingTable.GetClosestPeers(target, K)
	if want != nil {
		for _, p := range shortlist {
			if p.ID == *want {
				return shortlist, p
			}
		}
	}
	lookup := newValueLookup(d.swarm.GetSelfID())

	type answer struct {
		asked *PeerNode
		nodes []*PeerNode
		err   error
	}
	for ctx.Err() == nil {
		var batch []*PeerNode
		for _, p := range shortlist {
			if len(batch) == Alpha {
				break
			}
			if lookup.claim(p.ID) {
				batch = append(batch, p)
			}
		}
		if len(batch) == 0 {
			break
		}
		answers := make(chan answer, len(batch))
		for _, p := range batch {
			go func(p *PeerNode) {
				nodes, err := d.askFindNode(ctx, p, target)
				answers <- answer{asked: p, nodes: nodes, err: err}
			}(p)
		}
		failed := map[types.PeerID]bool{}
		var found *PeerNode
		for range batch {
			a := <-answers
			if a.err != nil {
				failed[a.asked.ID] = true
				continue
			}
			for _, n := range a.nodes {
				if want != nil && n.ID == *want && found == nil {
					found = n
				}
			}
			shortlist = mergeClosest(shortlist, a.nodes, target, d.swarm.GetSelfID())
		}
		if len(failed) > 0 {
			kept := shortlist[:0]
			for _, p := range shortlist {
				if !failed[p.ID] {
					kept = append(kept, p)
				}
			}
			shortlist = kept
		}
		if found != nil {
			return shortlist, found
		}
	}
	return shortlist, nil
}

// mergeClosest adds the new nodes to the list, once each and never this node, keeping the K
// closest to target in order.
func mergeClosest(list, add []*PeerNode, target, self types.PeerID) []*PeerNode {
	have := make(map[types.PeerID]bool, len(list)+len(add))
	out := make([]*PeerNode, 0, len(list)+len(add))
	for _, p := range append(append([]*PeerNode{}, list...), add...) {
		if p.ID == self || have[p.ID] {
			continue
		}
		have[p.ID] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return isCloser(out[i].ID, out[j].ID, target) })
	if len(out) > K {
		out = out[:K]
	}
	return out
}

// askFindNode asks one peer for the peers it knows closest to target, connecting to it first
// when needed. The peers named are added to the routing table.
func (d *EchoFogDHT) askFindNode(ctx context.Context, p *PeerNode, target types.PeerID) ([]*PeerNode, error) {
	if !d.swarm.ThisIsActivePeer(p.ID) {
		if p.Addr == "" || !d.swarm.AutoDial() {
			return nil, errors.New("not connected")
		}
		dialCtx, cancel := context.WithTimeout(ctx, lookupDialWait)
		err := d.swarm.AddPotentialPeerWithCtx(dialCtx, p.ID, p.Addr)
		cancel()
		if err != nil {
			return nil, err
		}
	}

	msgID := types.NewMessageID()
	req := &internal_pb.MessageData{
		MessageId: msgID[:],
		Payload: &internal_pb.MessageData_FindNodeReq{
			FindNodeReq: &internal_pb.FindNodeRequest{TargetId: target[:]},
		},
	}
	waitCh := make(chan *internal_pb.FindNodeResponse, 1)
	d.pending.Store(string(msgID[:]), waitCh)
	defer d.pending.Delete(string(msgID[:]))

	if err := d.swarm.SendDataForPeer(p.ID, net_types.TypeFindNode, req); err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, nodeQueryTimeout)
	defer cancel()
	select {
	case res := <-waitCh:
		return d.takeInfos(res.ClosestPeers), nil
	case <-qctx.Done():
		return nil, qctx.Err()
	}
}

// takeInfos turns peers named in an answer into routing table entries: valid keys only, never
// this node, and only addresses the policy keeps.
func (d *EchoFogDHT) takeInfos(infos []*internal_pb.PeerInfo) []*PeerNode {
	policy := d.routingTable.AddrPolicy()
	self := d.swarm.GetSelfID()
	out := make([]*PeerNode, 0, len(infos))
	for _, info := range infos {
		pub, err := types.ToPeerPubKey(info.GetPubKey())
		if err != nil || !policy.Shareable(info.GetAddress()) {
			continue
		}
		id := types.PeerPubKeyToID(pub)
		if id == self {
			continue
		}
		d.routingTable.AddOrUpdatePeer(pub, info.GetAddress())
		out = append(out, &PeerNode{ID: id, PubKey: pub, Addr: info.GetAddress(), LastSeen: time.Now()})
	}
	return out
}

// closestInfos is what this node answers a lookup toward target with: the K closest peers it
// knows whose address the policy lets out, never the asker.
func (d *EchoFogDHT) closestInfos(target, asker types.PeerID) []*internal_pb.PeerInfo {
	policy := d.routingTable.AddrPolicy()
	closest := d.routingTable.GetClosestPeers(target, K+1)
	out := make([]*internal_pb.PeerInfo, 0, len(closest))
	for _, p := range closest {
		if len(out) == K {
			break
		}
		if p.ID == asker || !policy.Shareable(p.Addr) {
			continue
		}
		pub := p.PubKey
		out = append(out, &internal_pb.PeerInfo{PubKey: pub[:], Address: p.Addr})
	}
	return out
}
