package peerstore

import (
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func createTestStorage(t *testing.T) (storage.Storage, func()) {
	tmpDir, err := os.MkdirTemp("", "echofog-peerstore-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	opts := badger.DefaultOptions(tmpDir)
	opts.Logger = nil
	db, err := badger.Open(opts)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("failed to open badger: %v", err)
	}

	st := storage.NewBadgerStorage(db)
	cleanup := func() {
		st.Close()
		_ = os.RemoveAll(tmpDir)
	}
	return st, cleanup
}

func randomPubKey(t *testing.T) (types.PeerPublicKey, types.PeerID) {
	var pub types.PeerPublicKey
	_, err := rand.Read(pub[:])
	if err != nil {
		t.Fatalf("rand read: %v", err)
	}
	return pub, types.PeerPubKeyToID(pub)
}

func TestPeerStore_SaveAndRetrieve(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()

	ps := NewPeerStore(st, nil)
	defer ps.Close()

	pub, peerID := randomPubKey(t)
	addr := "127.0.0.1:4455"
	trust := uint32(95)

	if err := ps.SavePeer(pub, addr, trust); err != nil {
		t.Fatalf("SavePeer failed: %v", err)
	}

	entry, err := ps.GetPeer(peerID)
	if err != nil {
		t.Fatalf("GetPeer failed: %v", err)
	}

	if entry.LastKnownAddr != addr {
		t.Errorf("expected addr %s, got %s", addr, entry.LastKnownAddr)
	}
	if entry.TrustScore != trust {
		t.Errorf("expected trust %d, got %d", trust, entry.TrustScore)
	}

	history := ps.GetHistoryConnected(10)
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
}

func TestPeerStore_BanAndUnban(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()

	ps := NewPeerStore(st, nil)
	defer ps.Close()

	_, peerID := randomPubKey(t)

	if ps.IsBanned(peerID) {
		t.Fatal("peer should not be banned initially")
	}

	if err := ps.BanPeer(peerID); err != nil {
		t.Fatalf("BanPeer failed: %v", err)
	}

	if !ps.IsBanned(peerID) {
		t.Fatal("peer should be banned")
	}

	if err := ps.UnBanPeer(peerID); err != nil {
		t.Fatalf("UnBanPeer failed: %v", err)
	}

	if ps.IsBanned(peerID) {
		t.Fatal("peer should be unbanned")
	}
}

func TestPeerStore_AsyncGracefulShutdown(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()

	ps := NewPeerStore(st, nil)

	for i := 0; i < 50; i++ {
		pub, _ := randomPubKey(t)
		ps.SavePeerAsync(pub, "127.0.0.1:9000", 100)
	}

	time.Sleep(10 * time.Millisecond)
	// Must not hang or panic
	if err := ps.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
