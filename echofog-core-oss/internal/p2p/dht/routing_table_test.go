package dht

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

type mockPinger struct {
	mu             sync.Mutex // the routing table pings from its own goroutines
	pingResultFunc func(id types.PeerID, lastAddr string) (time.Duration, error)
	pingCount      atomic.Uint64
}

func (m *mockPinger) setResult(fn func(id types.PeerID, lastAddr string) (time.Duration, error)) {
	m.mu.Lock()
	m.pingResultFunc = fn
	m.mu.Unlock()
}

func (m *mockPinger) PingPeer(id types.PeerID, lastAddr string) (time.Duration, error) {
	m.pingCount.Add(1)
	m.mu.Lock()
	fn := m.pingResultFunc
	m.mu.Unlock()
	if fn != nil {
		return fn(id, lastAddr)
	}
	return 10 * time.Millisecond, nil
}

func generateRandomPeer() (types.PeerPublicKey, types.PeerID, string) {
	var pub types.PeerPublicKey
	rand.Read(pub[:])
	id := types.PeerPubKeyToID(pub)
	ipA, _ := rand.Int(rand.Reader, big.NewInt(254))
	ipB, _ := rand.Int(rand.Reader, big.NewInt(254))
	addr := fmt.Sprintf("203.%d.%d.1:9000", ipA.Int64()+1, ipB.Int64()+1)
	return pub, id, addr
}

func TestRoutingTable_XORDistanceAndKademliaMath(t *testing.T) {
	var zeroID types.PeerID
	var idA types.PeerID
	var idB types.PeerID

	idA[0] = 0b10000000 // Differ at bit 0
	idB[0] = 0b01000000 // Differ at bit 1

	// 1. Bit 0 differs -> bucket index 0
	idxA := getBucketIndex(zeroID, idA)
	if idxA != 0 {
		t.Fatalf("expected bucketIdx=0 for bit 0 difference, got %d", idxA)
	}

	// 2. Bit 1 differs -> bucket index 1
	idxB := getBucketIndex(zeroID, idB)
	if idxB != 1 {
		t.Fatalf("expected bucketIdx=1 for bit 1 difference, got %d", idxB)
	}

	// 3. Last bit differs (byte 31, bit 7) -> bucket index 255
	var idLast types.PeerID
	idLast[31] = 0b00000001
	idxLast := getBucketIndex(zeroID, idLast)
	if idxLast != 255 {
		t.Fatalf("expected bucketIdx=255 for last bit difference, got %d", idxLast)
	}

	// 4. Exact match (self) -> bucket index 255
	idxSelf := getBucketIndex(zeroID, zeroID)
	if idxSelf != 255 {
		t.Fatalf("expected bucketIdx=255 for identical IDs, got %d", idxSelf)
	}

	// 5. isCloser test: idB (distance ~2^254) is closer to zeroID than idA (distance ~2^255)
	if !isCloser(idB, idA, zeroID) {
		t.Fatalf("expected idB to be closer to zeroID than idA")
	}
	if isCloser(idA, idB, zeroID) {
		t.Fatalf("idA should not be closer than idB")
	}
}

func TestRoutingTable_BucketCapacityAndPingEviction(t *testing.T) {
	var myPub types.PeerPublicKey
	rand.Read(myPub[:])
	pinger := &mockPinger{}

	rt := NewRoutingTable(myPub, nil, pinger)

	// Create 20 peers that all land in the EXACT SAME bucket
	// To guarantee landing in bucket 0: flip the first bit of myPub's ID
	myID := rt.myID
	firstBitFlipped := myID
	firstBitFlipped[0] ^= 0x80

	targetBucketIdx := getBucketIndex(myID, firstBitFlipped)

	peers := make([]types.PeerPublicKey, 0, MaxBucketSize+2)
	for i := 0; i < MaxBucketSize+2; i++ {
		pub, _, _ := generateRandomPeer()
		id := types.PeerPubKeyToID(pub)
		// Force id to have identical first bit as firstBitFlipped so it lands in targetBucketIdx
		for id[0]&0x80 != firstBitFlipped[0]&0x80 || getBucketIndex(myID, id) != targetBucketIdx {
			pub, _, _ = generateRandomPeer()
			id = types.PeerPubKeyToID(pub)
		}
		peers = append(peers, pub)
	}

	// Add 20 peers (fills bucket to maximum)
	for i := 0; i < MaxBucketSize; i++ {
		rt.AddOrUpdatePeer(peers[i], fmt.Sprintf("198.51.100.%d:9000", i+1))
	}

	bucket := rt.buckets[targetBucketIdx]
	bucket.mu.RLock()
	if len(bucket.Peers) != MaxBucketSize {
		t.Fatalf("expected bucket to be full (%d), got %d", MaxBucketSize, len(bucket.Peers))
	}
	oldestPeerID := bucket.Peers[0].ID
	bucket.mu.RUnlock()

	// Scenario A: 21st peer arrives, oldest peer responds to ping successfully -> oldest stays, new rejected
	pinger.setResult(func(id types.PeerID, lastAddr string) (time.Duration, error) {
		return 5 * time.Millisecond, nil
	})

	rt.AddOrUpdatePeer(peers[MaxBucketSize], "198.51.100.99:9000")
	time.Sleep(50 * time.Millisecond) // wait for async ping goroutine

	bucket.mu.RLock()
	if len(bucket.Peers) != MaxBucketSize {
		t.Fatalf("bucket size changed unexpectedly: %d", len(bucket.Peers))
	}
	if bucket.Peers[0].ID != oldestPeerID {
		t.Fatalf("responsive peer should not have been evicted")
	}
	bucket.mu.RUnlock()

	// Scenario B: 22nd peer arrives, oldest peer FAILS ping -> oldest evicted, new peer inserted!
	pinger.setResult(func(id types.PeerID, lastAddr string) (time.Duration, error) {
		return 0, errors.New("timeout: peer unreachable")
	})

	rt.AddOrUpdatePeer(peers[MaxBucketSize+1], "198.51.100.100:9000")
	time.Sleep(50 * time.Millisecond) // wait for eviction

	bucket.mu.RLock()
	if len(bucket.Peers) != MaxBucketSize {
		t.Fatalf("bucket size should remain %d after eviction, got %d", MaxBucketSize, len(bucket.Peers))
	}
	lastPeer := bucket.Peers[len(bucket.Peers)-1]
	newPeerID := types.PeerPubKeyToID(peers[MaxBucketSize+1])
	if lastPeer.ID != newPeerID {
		t.Fatalf("expected new peer %v to be added after eviction, got %v", newPeerID, lastPeer.ID)
	}
	bucket.mu.RUnlock()
}

func TestRoutingTable_GetClosestPeersOrder(t *testing.T) {
	var myPub types.PeerPublicKey
	rand.Read(myPub[:])
	rt := NewRoutingTable(myPub, nil, &mockPinger{})

	// Add 200 random peers across different buckets
	for i := 0; i < 200; i++ {
		pub, _, addr := generateRandomPeer()
		rt.AddOrUpdatePeer(pub, addr)
	}

	var target types.PeerID
	rand.Read(target[:])

	closest := rt.GetClosestPeers(target, 20)
	if len(closest) == 0 {
		t.Fatalf("expected closest peers to not be empty")
	}

	// Verify that candidates are strictly sorted by increasing XOR distance to target
	for i := 0; i < len(closest)-1; i++ {
		first := closest[i].ID
		second := closest[i+1].ID

		if isCloser(second, first, target) {
			t.Fatalf("peers at index %d and %d are not sorted by XOR distance to target", i, i+1)
		}
	}
}

func TestRoutingTable_MassiveConcurrencyStress(t *testing.T) {
	var myPub types.PeerPublicKey
	rand.Read(myPub[:])
	pinger := &mockPinger{}
	rt := NewRoutingTable(myPub, nil, pinger)

	const totalPeers = 5000
	const workers = 30

	peers := make([]struct {
		pub  types.PeerPublicKey
		id   types.PeerID
		addr string
	}, totalPeers)

	for i := 0; i < totalPeers; i++ {
		pub, id, addr := generateRandomPeer()
		peers[i].pub = pub
		peers[i].id = id
		peers[i].addr = addr
	}

	var wg sync.WaitGroup

	// 1. Concurrent AddOrUpdate workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			chunkSize := totalPeers / workers
			start := workerIdx * chunkSize
			end := start + chunkSize
			if workerIdx == workers-1 {
				end = totalPeers
			}

			for i := start; i < end; i++ {
				rt.AddOrUpdatePeer(peers[i].pub, peers[i].addr)
			}
		}(w)
	}

	// 2. Concurrent GetClosestPeers workers
	var queryCount atomic.Uint64
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				var target types.PeerID
				rand.Read(target[:])
				closest := rt.GetClosestPeers(target, 20)
				if len(closest) > 20 {
					t.Errorf("GetClosestPeers returned more than requested: %d", len(closest))
				}
				queryCount.Add(1)
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	// 3. Concurrent RemovePeer workers
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				idx, _ := rand.Int(rand.Reader, big.NewInt(totalPeers))
				rt.RemovePeer(peers[idx.Int64()].id)
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// Post-stress validation: Check that every bucket has <= MaxBucketSize
	for i := 0; i < IDLengthBits; i++ {
		b := rt.buckets[i]
		b.mu.RLock()
		if len(b.Peers) > MaxBucketSize {
			t.Fatalf("bucket %d exceeded MaxBucketSize: %d", i, len(b.Peers))
		}
		b.mu.RUnlock()
	}

	t.Logf("Stress test passed cleanly: 5,000 peers processed, %d closest queries under concurrency", queryCount.Load())
}
