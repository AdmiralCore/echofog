package testutil

import (
	"log/slog"
	"testing"

	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func SetupTestCryptoEngine(t *testing.T) (*crypto.CryptoEngine, *TestingBadgerStorage) {
	t.Helper()
	testStore := NewTestingBadgerStorage(t)
	engine := SetupTestCryptoEngineWithStorage(t, testStore.St)
	return engine, testStore
}

func SetupTestCryptoEngineWithStorage(t *testing.T, st storage.Storage) *crypto.CryptoEngine {
	t.Helper()

	rootI := make([]byte, 64)
	km := keymanager.NewKeyManager(rootI, st)

	accManager, err := km.GetAccount(0)
	if err != nil {
		t.Fatalf("failed to get account: %v", err)
	}

	_, pubKey, err := accManager.GetIdentityKey()
	if err != nil {
		t.Fatalf("failed to get identity key: %v", err)
	}
	var peerPubKey types.PeerPublicKey
	copy(peerPubKey[:], pubKey)
	me := types.PeerPubKeyToID(peerPubKey)

	password := []byte("test_password")
	initializer, err := crypto.NewX3DHInitializer(accManager, st, password)
	if err != nil {
		t.Fatalf("failed to create x3dh initializer: %v", err)
	}

	log := slog.Default()
	engine := crypto.NewCryptoEngine(
		me,
		accManager,
		initializer.GetEngine(),
		initializer.GetSessionManager(),
		&DummyBundleSource{},
		log,
	)

	return engine
}

type DummyBundleSource struct{}

func (d *DummyBundleSource) GetBundle(peerID types.PeerID) (*keymanager.PublicBundle, error) {
	return &keymanager.PublicBundle{}, nil
}
