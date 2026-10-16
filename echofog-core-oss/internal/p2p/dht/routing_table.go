package dht

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	MaxBucketSize = 20
	IDLengthBits  = 256
)

type Bucket struct {
	mu    sync.RWMutex
	Peers []*PeerNode

	lastLookup time.Time // when a lookup last went into this bucket's part of the ID space
}

type PeerNode struct {
	ID       types.PeerID
	PubKey   types.PeerPublicKey
	Addr     string
	LastSeen time.Time
}

type PeerPinger interface {
	PingPeer(id types.PeerID, lastAddr string) (time.Duration, error)
}

type RoutingTable struct {
	mu       sync.RWMutex
	myPubKey types.PeerPublicKey
	myID     types.PeerID

	buckets [IDLengthBits]*Bucket

	pinger PeerPinger

	policy atomic.Pointer[network.AddrPolicy] // addresses kept: public only unless set otherwise

	storage storage.Storage
}

// SetAddrPolicy sets which peer addresses the table keeps (and so gives out in answers).
func (r *RoutingTable) SetAddrPolicy(p network.AddrPolicy) { r.policy.Store(&p) }

// AddrPolicy is the policy set by SetAddrPolicy: public addresses only by default.
func (r *RoutingTable) AddrPolicy() network.AddrPolicy {
	if p := r.policy.Load(); p != nil {
		return *p
	}
	return network.AddrPolicy{}
}

func NewRoutingTable(myPubKey types.PeerPublicKey, storage storage.Storage, pinger PeerPinger) *RoutingTable {
	rt := &RoutingTable{
		myPubKey: myPubKey,
		myID:     types.PeerPubKeyToID(myPubKey),
		storage:  storage,
		pinger:   pinger,
	}

	for i := 0; i < IDLengthBits; i++ {
		rt.buckets[i] = &Bucket{
			Peers: make([]*PeerNode, 0, MaxBucketSize),
		}
	}

	return rt
}

func (r *RoutingTable) AddOrUpdatePeer(pubKey types.PeerPublicKey, addr string) {
	peerID := types.PeerPubKeyToID(pubKey)
	if peerID == r.myID || !r.AddrPolicy().Shareable(addr) {
		return // a private or loopback address would point whoever we give it to at their own LAN
	}

	bucketIdx := getBucketIndex(r.myID, peerID)
	bucket := r.buckets[bucketIdx]

	bucket.mu.Lock()

	for i, p := range bucket.Peers {
		if p.ID == peerID {
			// Copy-on-write: GetClosestPeers hands these pointers to readers outside the lock.
			updated := &PeerNode{ID: peerID, PubKey: pubKey, Addr: addr, LastSeen: time.Now()}
			bucket.Peers = append(bucket.Peers[:i], bucket.Peers[i+1:]...)
			bucket.Peers = append(bucket.Peers, updated)

			bucket.mu.Unlock()
			return
		}
	}

	newNode := &PeerNode{
		ID:       peerID,
		PubKey:   pubKey,
		Addr:     addr,
		LastSeen: time.Now(),
	}

	if len(bucket.Peers) < MaxBucketSize {
		bucket.Peers = append(bucket.Peers, newNode)
		bucket.mu.Unlock()
	} else {
		oldestPeer := bucket.Peers[0]
		bucket.mu.Unlock()

		go func(oldPeer *PeerNode, newPeer *PeerNode) {
			_, err := r.pinger.PingPeer(oldPeer.ID, oldPeer.Addr)
			if err != nil {
				bucket.mu.Lock()
				defer bucket.mu.Unlock()
				if len(bucket.Peers) > 0 && bucket.Peers[0].ID == oldPeer.ID {
					bucket.Peers = append(bucket.Peers[1:], newPeer)
				}
			}
		}(oldestPeer, newNode)
	}
}

func (r *RoutingTable) RemovePeer(peerID types.PeerID) {
	bucketIdx := getBucketIndex(r.myID, peerID)
	bucket := r.buckets[bucketIdx]

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	for i, p := range bucket.Peers {
		if p.ID == peerID {
			bucket.Peers = append(bucket.Peers[:i], bucket.Peers[i+1:]...)
			return
		}
	}
}

func (r *RoutingTable) GetClosestPeers(target types.PeerID, count int) []*PeerNode {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var candidates []*PeerNode

	targetIdx := getBucketIndex(r.myID, target)

	collectFromBucket := func(idx int) {
		if idx < 0 || idx >= IDLengthBits {
			return
		}
		bucket := r.buckets[idx]
		bucket.mu.RLock()
		for _, p := range bucket.Peers {
			if p.ID != r.myID {
				candidates = append(candidates, p)
			}
		}
		bucket.mu.RUnlock()
	}

	collectFromBucket(targetIdx)

	left := targetIdx - 1
	right := targetIdx + 1

	for len(candidates) < count && (left >= 0 || right < IDLengthBits) {
		if left >= 0 {
			collectFromBucket(left)
			left--
		}
		if right < IDLengthBits {
			collectFromBucket(right)
			right++
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return isCloser(candidates[i].ID, candidates[j].ID, target)
	})

	if len(candidates) > count {
		candidates = candidates[:count]
	}

	return candidates
}

func isCloser(id1, id2, target types.PeerID) bool {
	for i := 0; i < IDLength; i++ {
		xor1 := id1[i] ^ target[i]
		xor2 := id2[i] ^ target[i]

		if xor1 != xor2 {
			return xor1 < xor2
		}
	}
	return false
}

// Len is the number of peers in the table.
func (r *RoutingTable) Len() int {
	n := 0
	for _, b := range r.buckets {
		b.mu.RLock()
		n += len(b.Peers)
		b.mu.RUnlock()
	}
	return n
}

// allPeers returns every peer in the table, most recently seen first.
func (r *RoutingTable) allPeers() []*PeerNode {
	var out []*PeerNode
	for _, b := range r.buckets {
		b.mu.RLock()
		out = append(out, b.Peers...)
		b.mu.RUnlock()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// markLookup notes that a lookup went toward target, which refreshes target's bucket.
func (r *RoutingTable) markLookup(target types.PeerID, at time.Time) {
	b := r.buckets[getBucketIndex(r.myID, target)]
	b.mu.Lock()
	b.lastLookup = at
	b.mu.Unlock()
}

// staleBuckets lists the buckets no lookup went into for maxAge, up to the deepest one holding
// a peer: past it every bucket is empty, as nobody shares that long a prefix with us, and a
// lookup of our own ID covers them.
func (r *RoutingTable) staleBuckets(now time.Time, maxAge time.Duration) []int {
	deepest := -1
	for i := IDLengthBits - 1; i >= 0; i-- {
		b := r.buckets[i]
		b.mu.RLock()
		n := len(b.Peers)
		b.mu.RUnlock()
		if n > 0 {
			deepest = i
			break
		}
	}
	var out []int
	for i := 0; i <= deepest; i++ {
		b := r.buckets[i]
		b.mu.RLock()
		stale := now.Sub(b.lastLookup) >= maxAge
		b.mu.RUnlock()
		if stale {
			out = append(out, i)
		}
	}
	return out
}

// randomIDInBucket is an ID that falls in bucket idx: it shares exactly idx leading bits with
// ours, the rest random.
func randomIDInBucket(self types.PeerID, idx int, random [32]byte) types.PeerID {
	id := random
	for bit := 0; bit < idx; bit++ {
		mask := byte(0x80) >> (bit % 8)
		id[bit/8] = id[bit/8]&^mask | self[bit/8]&mask
	}
	mask := byte(0x80) >> (idx % 8)
	id[idx/8] = id[idx/8]&^mask | (self[idx/8]&mask ^ mask)
	return id
}
