package dht

import (
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/cache"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestDHT_HotCacheStressAndEviction(t *testing.T) {
	// Create an EchoFogDHT with our leak-free generic caches
	d := &EchoFogDHT{
		hotCache:    cache.New[types.PeerID, string](100, 50*time.Millisecond),
		peerQuotas:  cache.New[types.PeerID, int](200, 100*time.Millisecond),
		unreachable: cache.New[types.PeerID, struct{}](100, 50*time.Millisecond),
		recent:      cache.New[string, []byte](100, 50*time.Millisecond),
		misses:      cache.New[string, struct{}](100, 50*time.Millisecond),
	}

	const workers = 40
	const iterations = 500

	var wg sync.WaitGroup

	// 1. Concurrent writes and reads to hotCache
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				var pid types.PeerID
				pid[0] = byte(i % 150)
				pid[1] = byte(workerID)

				d.hotCache.Set(pid, "10.0.0.1:9000")

				addr, ok := d.hotCache.Get(pid)
				if ok && addr != "10.0.0.1:9000" {
					t.Errorf("hotCache corruption: got %s", addr)
				}

				if i%10 == 0 {
					d.OnPeerDisconnected(pid)
				}
			}
		}(w)
	}

	// 2. Concurrent quota tracking
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			var spammerID types.PeerID
			spammerID[0] = 0xEE
			spammerID[1] = byte(workerID)

			for i := 0; i < 1500; i++ {
				count := d.countRecordFromPeer(spammerID)
				if count <= 0 {
					t.Errorf("countRecordFromPeer returned invalid count %d", count)
				}
			}
		}(w)
	}

	// 3. Concurrent Prune and Reset calls
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				time.Sleep(10 * time.Millisecond)
				d.PruneExpired()
				if i%5 == 0 {
					d.ResetQuotas()
				}
			}
		}()
	}

	wg.Wait()

	// Verify hotCache strictly never exceeds capacity (100)
	if d.hotCache.Len() > 100 {
		t.Fatalf("hotCache exceeded capacity: %d > 100", d.hotCache.Len())
	}

	t.Logf("HotCache & Quotas stress passed cleanly: capacity preserved, no data races, current len: %d", d.hotCache.Len())
}

func TestDHT_RecordValidationAndSecurity(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ed25519 key: %v", err)
	}

	key := []byte("dht:user:alice:avatar")
	val := []byte("my_avatar_image_binary_data")
	now := uint64(time.Now().Unix())
	ttl := uint32(3600) // 1 hour

	signBytes := PackRecordForSigning(key, val, now, ttl)
	sig := ed25519.Sign(priv, signBytes)

	// 1. Valid record must pass
	validRec := &internal_pb.DhtRecord{
		Key:             key,
		Value:           val,
		PublisherPubKey: pub,
		Timestamp:       now,
		Ttl:             ttl,
		Signature:       sig,
	}

	if err := ValidateRecord(validRec); err != nil {
		t.Fatalf("expected valid record to pass, got: %v", err)
	}

	// 2. Tampered value must fail
	tamperedRec := &internal_pb.DhtRecord{
		Key:             key,
		Value:           []byte("tampered_fake_avatar"),
		PublisherPubKey: pub,
		Timestamp:       now,
		Ttl:             ttl,
		Signature:       sig,
	}
	if err := ValidateRecord(tamperedRec); err == nil {
		t.Fatalf("expected tampered record to fail signature check")
	}

	// 3. Expired record must fail
	expiredRec := &internal_pb.DhtRecord{
		Key:             key,
		Value:           val,
		PublisherPubKey: pub,
		Timestamp:       now - 7200, // 2 hours ago
		Ttl:             3600,       // valid for 1 hour -> expired 1 hour ago
		Signature:       sig,
	}
	expiredSignBytes := PackRecordForSigning(expiredRec.Key, expiredRec.Value, expiredRec.Timestamp, expiredRec.Ttl)
	expiredRec.Signature = ed25519.Sign(priv, expiredSignBytes)

	if err := ValidateRecord(expiredRec); err == nil {
		t.Fatalf("expected expired record to fail validation")
	}

	// 4. Record with future timestamp (> +60s) must fail
	futureRec := &internal_pb.DhtRecord{
		Key:             key,
		Value:           val,
		PublisherPubKey: pub,
		Timestamp:       now + 3600, // 1 hour in the future
		Ttl:             3600,
	}
	futureSignBytes := PackRecordForSigning(futureRec.Key, futureRec.Value, futureRec.Timestamp, futureRec.Ttl)
	futureRec.Signature = ed25519.Sign(priv, futureSignBytes)

	if err := ValidateRecord(futureRec); err == nil {
		t.Fatalf("expected future record to fail validation")
	}

	// 5. Oversized record (> 50KB) must fail
	hugeVal := make([]byte, MaxRecordSize+100)
	hugeRec := &internal_pb.DhtRecord{
		Key:             key,
		Value:           hugeVal,
		PublisherPubKey: pub,
		Timestamp:       now,
		Ttl:             ttl,
	}
	hugeSignBytes := PackRecordForSigning(hugeRec.Key, hugeRec.Value, hugeRec.Timestamp, hugeRec.Ttl)
	hugeRec.Signature = ed25519.Sign(priv, hugeSignBytes)

	if err := ValidateRecord(hugeRec); err == nil {
		t.Fatalf("expected oversized record to fail validation")
	}
}

func TestDHT_PendingResponseSynchronization(t *testing.T) {
	d := &EchoFogDHT{}

	const workers = 50
	var wg sync.WaitGroup
	var successCount atomic.Uint64

	// Concurrently test pending message response delivery
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			msgID := types.NewMessageID()
			msgIDStr := string(msgID[:])

			waitCh := make(chan *internal_pb.FindNodeResponse, 1)
			d.pending.Store(msgIDStr, waitCh)

			// Simulating response arriving from remote node
			go func() {
				res := &internal_pb.FindNodeResponse{
					ClosestPeers: []*internal_pb.PeerInfo{
						{Address: "192.168.1.1:9000"},
					},
				}
				resMsg := &internal_pb.MessageData{
					MessageId: msgID[:],
					Payload: &internal_pb.MessageData_FindNodeRes{
						FindNodeRes: res,
					},
				}
				d.HandleFindNodeResponse(resMsg)
			}()

			select {
			case res := <-waitCh:
				if len(res.ClosestPeers) == 1 && res.ClosestPeers[0].Address == "192.168.1.1:9000" {
					successCount.Add(1)
				}
			case <-time.After(1 * time.Second):
				t.Errorf("worker %d timed out waiting for pending response", workerID)
			}
		}(w)
	}

	wg.Wait()

	if successCount.Load() != workers {
		t.Fatalf("expected %d successful syncs, got %d", workers, successCount.Load())
	}
}
