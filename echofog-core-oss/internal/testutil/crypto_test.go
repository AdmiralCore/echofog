package testutil

import (
	"crypto/rand"
	"testing"
)

func TestSetupTestCryptoEngine(t *testing.T) {
	engine, store := SetupTestCryptoEngine(t)
	defer store.CloseAndClear()

	if engine == nil {
		t.Fatal("expected engine to be non-nil")
	}

	data := []byte("hello world")
	key := make([]byte, 32)
	rand.Read(key)

	encrypted, err := engine.EncryptBlock(data, key)
	if err != nil {
		t.Fatalf("failed to encrypt block: %v", err)
	}

	decrypted, err := engine.DecryptBlock(encrypted, key)
	if err != nil {
		t.Fatalf("failed to decrypt block: %v", err)
	}

	if string(decrypted) != string(data) {
		t.Errorf("expected %s, got %s", string(data), string(decrypted))
	}
}
