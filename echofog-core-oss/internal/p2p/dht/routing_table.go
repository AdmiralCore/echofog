package dht

import (
	"sort"
	"sync"
	"time"

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

	storage storage.Storage
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
	if peerID == r.myID {
		return
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
