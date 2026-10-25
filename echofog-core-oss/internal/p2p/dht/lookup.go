package dht

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	// K is how many closest peers a lookup converges on and an answer carries.
	K = MaxBucketSize

	// Replicas is how many peers a record is stored on.
	Replicas = 3

	// Concurrency is how many queries one lookup keeps in flight (libp2p runs 10). Peers that
	// answer cost next to nothing; a peer that is gone costs a dial timeout, and a table full of
	// them must clear in a few seconds, not a few minutes, so more than 10 wait at once.
	Concurrency = 16

	// nodeQueryTimeout is how long a connected peer gets to answer one query.
	nodeQueryTimeout = 2 * time.Second
	// lookupDialWait is how long a lookup waits to connect to a peer it was referred to.
	lookupDialWait = 2 * time.Second

	// maxLookupQueries bounds the peers one lookup asks, dials that fail included, so that peers
	// pointing at each other (or at addresses nobody answers) can never keep a lookup going.
	maxLookupQueries = 64

	// unreachableFor is how long a peer that did not answer a dial stays out of the table and of
	// lookups. Other nodes keep naming it meanwhile; without this memory every lookup would dial
	// it again and wait the same timeout.
	unreachableFor = 10 * time.Minute
)

var errNotConnected = errors.New("not connected")

type candState uint8

const (
	candNew      candState = iota
	candAsking             // a query is in flight
	candAnswered           // answered: a live peer
	candFailed             // did not answer, or could not be reached
)

type candidate struct {
	node  *PeerNode
	state candState
}

// lookup is one Kademlia lookup toward target: the peers known so far by distance, which of them
// were asked, and what came back. It is driven by run from one goroutine; the queries run in
// their own and report through a channel.
type lookup struct {
	d      *EchoFogDHT
	target types.PeerID
	key    []byte        // the record wanted; nil for a node lookup
	want   *types.PeerID // stop as soon as an answer names this peer

	cands   []*candidate // closest to target first
	index   map[types.PeerID]*candidate
	queries int  // peers asked so far, in flight included
	opening bool // the first batch is still to be picked

	res lookupResult
}

// lookupResult is what a lookup ends with.
type lookupResult struct {
	closest []*PeerNode // the live peers found, closest to target first, K at most
	record  *internal_pb.DhtRecord
	found   *PeerNode // the peer want named, when it was
	asked   int       // queries that came back, answered or failed
}

type queryResult struct {
	cand   *candidate
	nodes  []*PeerNode
	record *internal_pb.DhtRecord
	err    error
}

// newLookup starts from the K closest peers in the table and every peer connected now: in a
// small network the table is often full of peers that left, while the connected ones answer
// within a round trip.
func (d *EchoFogDHT) newLookup(target types.PeerID, key []byte, want *types.PeerID) *lookup {
	l := &lookup{d: d, target: target, key: key, want: want, index: map[types.PeerID]*candidate{}, opening: true}
	l.add(d.routingTable.GetClosestPeers(target, K))
	l.add(d.connectedPeers())
	return l
}

// add takes new candidates in: never this node, never a peer known to be unreachable, each once.
func (l *lookup) add(nodes []*PeerNode) {
	self := l.d.swarm.GetSelfID()
	added := false
	for _, n := range nodes {
		if n.ID == self || l.index[n.ID] != nil || l.d.isUnreachable(n.ID) {
			continue
		}
		c := &candidate{node: n}
		l.index[n.ID] = c
		l.cands = append(l.cands, c)
		added = true
	}
	if added {
		sort.SliceStable(l.cands, func(i, j int) bool { return isCloser(l.cands[i].node.ID, l.cands[j].node.ID, l.target) })
	}
}

// window is the K closest candidates that have not failed: the set a lookup converges on.
func (l *lookup) window() []*candidate {
	out := make([]*candidate, 0, K)
	for _, c := range l.cands {
		if len(out) == K {
			break
		}
		if c.state != candFailed {
			out = append(out, c)
		}
	}
	return out
}

func (l *lookup) inFlight() int {
	n := 0
	for _, c := range l.cands {
		if c.state == candAsking {
			n++
		}
	}
	return n
}

// next picks the peers to ask now, up to Concurrency in flight and the query budget. The opening
// batch takes the closest connected peers wherever they stand, since they answer at once; after
// that the K-closest window is worked through, its connected peers first, nearest first.
func (l *lookup) next() []*candidate {
	room := Concurrency - l.inFlight()
	if left := maxLookupQueries - l.queries; left < room {
		room = left
	}
	var picks []*candidate
	pick := func(c *candidate) {
		c.state = candAsking
		l.queries++
		picks = append(picks, c)
		room--
	}
	if l.opening {
		l.opening = false
		for _, c := range l.cands {
			if room <= 0 {
				break
			}
			if c.state == candNew && l.d.swarm.ThisIsActivePeer(c.node.ID) {
				pick(c)
			}
		}
	}
	window := l.window()
	for _, connectedOnly := range []bool{true, false} {
		for _, c := range window {
			if room <= 0 {
				break
			}
			if c.state != candNew || (connectedOnly && !l.d.swarm.ThisIsActivePeer(c.node.ID)) {
				continue
			}
			pick(c)
		}
	}
	return picks
}

// run drives the lookup until the record (or the wanted peer) is found, the K closest known
// peers have all answered or failed, the budget is spent or ctx ends.
func (l *lookup) run(ctx context.Context) lookupResult {
	l.d.routingTable.markLookup(l.target, time.Now())
	if l.want != nil {
		for _, c := range l.cands {
			if c.node.ID == *l.want {
				l.res.found = c.node
				return l.finish()
			}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan queryResult, maxLookupQueries) // queries never block after run returns
	inflight := 0
	for {
		for _, c := range l.next() {
			inflight++
			go func(c *candidate) { results <- l.ask(ctx, c) }(c)
		}
		if inflight == 0 {
			return l.finish()
		}
		select {
		case r := <-results:
			inflight--
			if l.take(r) {
				return l.finish()
			}
		case <-ctx.Done():
			return l.finish()
		}
	}
}

// take records one answer; true when the lookup is done.
func (l *lookup) take(r queryResult) bool {
	l.res.asked++
	if r.err != nil {
		r.cand.state = candFailed
		return false
	}
	r.cand.state = candAnswered
	if r.record != nil {
		l.res.record = r.record
		return true
	}
	l.add(r.nodes)
	if l.want != nil {
		for _, n := range r.nodes {
			if n.ID == *l.want {
				l.res.found = n
				return true
			}
		}
	}
	return false
}

func (l *lookup) finish() lookupResult {
	for _, c := range l.cands {
		if len(l.res.closest) == K {
			break
		}
		if c.state == candAnswered {
			l.res.closest = append(l.res.closest, c.node)
		}
	}
	return l.res
}

// ask queries one peer, connecting to it first when needed: for the closest peers it knows to
// target, or for the record and else those peers.
func (l *lookup) ask(ctx context.Context, c *candidate) queryResult {
	d, p := l.d, c.node
	out := queryResult{cand: c}
	if !d.swarm.ThisIsActivePeer(p.ID) {
		if p.Addr == "" || !d.swarm.AutoDial() {
			out.err = errNotConnected
			return out
		}
		if out.err = d.dialOnce(ctx, p.ID, p.Addr); out.err != nil {
			return out
		}
	}
	if ctx.Err() != nil {
		out.err = ctx.Err()
		return out
	}

	msgID := types.NewMessageID()
	req := &internal_pb.MessageData{MessageId: msgID[:]}
	var msgType net_types.MessageType
	nodeCh := make(chan *internal_pb.FindNodeResponse, 1)
	valueCh := make(chan *internal_pb.FindValueRes, 1)
	if l.key == nil {
		msgType = net_types.TypeFindNode
		req.Payload = &internal_pb.MessageData_FindNodeReq{FindNodeReq: &internal_pb.FindNodeRequest{TargetId: l.target[:]}}
		d.pending.Store(string(msgID[:]), nodeCh)
	} else {
		msgType = net_types.TypeFindValue
		req.Payload = &internal_pb.MessageData_FindValueReq{FindValueReq: &internal_pb.FindValueReq{TargetKey: l.key}}
		d.pending.Store(string(msgID[:]), valueCh)
	}
	defer d.pending.Delete(string(msgID[:]))

	if err := d.swarm.SendDataForPeer(p.ID, msgType, req); err != nil {
		out.err = err
		return out
	}
	qctx, cancel := context.WithTimeout(ctx, nodeQueryTimeout)
	defer cancel()
	select {
	case res := <-nodeCh:
		out.nodes = d.takeInfos(res.ClosestPeers)
	case res := <-valueCh:
		if res.Record != nil {
			// only signed records, and only for the key that was asked
			if err := CheckRecord(res.Record, l.key); err != nil {
				slog.Warn("DHT: peer sent an invalid record", "peer", p.ID, "err", err)
				out.err = err
				return out
			}
			out.record = res.Record
			return out
		}
		out.nodes = d.takeInfos(res.ClosestPeers)
	case <-qctx.Done():
		out.err = qctx.Err()
	}
	return out
}

// dialAttempt is one dial in progress, shared by every lookup that wants the same peer.
type dialAttempt struct {
	done chan struct{}
	err  error
}

// dialOnce connects to a peer, or joins the dial another lookup already has in progress to it:
// a directory refresh runs many lookups at once, and they are all referred to the same peers,
// a peer that left among them. One dial, one timeout, one verdict for all of them. The dial
// runs to its own timeout even when the lookup that started it is over (it found what it
// wanted, or its time ran out): a peer that does not answer is then remembered as unreachable
// instead of being dialed again by the next lookup. A lookup gives up waiting with its ctx.
func (d *EchoFogDHT) dialOnce(ctx context.Context, id types.PeerID, addr string) error {
	d.dialsMu.Lock()
	a, ok := d.dials[id]
	if !ok {
		a = &dialAttempt{done: make(chan struct{})}
		d.dials[id] = a
		go func() {
			dialCtx, cancel := context.WithTimeout(context.Background(), lookupDialWait)
			a.err = d.swarm.AddPotentialPeerWithCtx(dialCtx, id, addr)
			cancel()
			// a verdict on the peer only when this node could dial at all
			if a.err != nil && !errors.Is(a.err, swarm.ErrIsolated) && len(d.swarm.GetAllPeers()) > 0 {
				d.markUnreachable(id, addr)
			}
			d.dialsMu.Lock()
			delete(d.dials, id)
			d.dialsMu.Unlock()
			close(a.done)
		}()
	}
	d.dialsMu.Unlock()
	select {
	case <-a.done:
		return a.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// connectedPeers is every peer this node holds a connection to.
func (d *EchoFogDHT) connectedPeers() []*PeerNode {
	peers := d.swarm.GetAllPeers()
	out := make([]*PeerNode, 0, len(peers))
	for _, p := range peers {
		out = append(out, &PeerNode{ID: p.ID(), PubKey: p.PubKey(), Addr: p.Addr(), LastSeen: time.Now()})
	}
	return out
}

// connectedClosest is every connected peer, closest to target first.
func (d *EchoFogDHT) connectedClosest(target types.PeerID) []*PeerNode {
	return mergeClosest(nil, d.connectedPeers(), target, d.swarm.GetSelfID())
}

// markUnreachable notes that a peer did not answer at addr: it leaves the table, and lookups do
// not ask it (or route through it) for unreachableFor however often other nodes name it. The
// mark only keeps it out of the way of other lookups: whoever wants that very peer (a call, a
// chat, an exit) still gets its address, from the network or this mark, and dials it or punches
// through to it on their own terms (see FindPeer).
func (d *EchoFogDHT) markUnreachable(id types.PeerID, addr string) {
	d.unreachable.Set(id, addr)
	d.routingTable.RemovePeer(id)
	d.hotCache.Delete(id)
}

func (d *EchoFogDHT) isUnreachable(id types.PeerID) bool {
	_, ok := d.unreachable.Get(id)
	return ok
}

// unreachableAddr is the address a marked peer did not answer at, "" when it is not marked.
func (d *EchoFogDHT) unreachableAddr(id types.PeerID) string {
	addr, _ := d.unreachable.Get(id)
	return addr
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

// takeInfos turns peers named in an answer into routing table entries: valid keys only, never
// this node, only addresses the policy keeps. A peer marked unreachable is returned (a lookup for
// that very peer is done once somebody names it) but does not go back into the table; lookup.add
// keeps it from being asked.
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
		if !d.isUnreachable(id) {
			d.routingTable.AddOrUpdatePeer(pub, info.GetAddress())
		}
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
