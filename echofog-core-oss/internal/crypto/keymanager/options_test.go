package keymanager

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/awnumar/memguard"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

func newRoot(t *testing.T) []byte {
	t.Helper()
	root := make([]byte, 64)
	if _, err := rand.Read(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// The key that seals secrets at rest is derived with a label; v2 renames it (services §4.0).
func TestStorageKeyInfoIsParameter(t *testing.T) {
	defer memguard.Purge()

	root := newRoot(t)
	v1Acc, err := NewKeyManager(root, &mockStorage{data: map[string][]byte{}}).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	v2Acc, err := NewKeyManager(append([]byte(nil), root...), &mockStorage{data: map[string][]byte{}}).
		GetAccountWithOptions(0, Options{StorageKeyInfo: "echofog/storage2/key"})
	if err != nil {
		t.Fatal(err)
	}

	v1Key, err := v1Acc.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	v2Key, err := v2Acc.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(openBytes(t, v1Key), openBytes(t, v2Key)) {
		t.Fatal("the storage key label is ignored")
	}

	// the default is still the v1 label, on the same account seed
	seedEnc, _, err := deriveSecureSLIP0010(v1Acc.AccountKey, v1Acc.AccountCC, HardenedIndexStandard+StorageKeyIndex)
	if err != nil {
		t.Fatal(err)
	}
	seed := openBytes(t, seedEnc)
	if got, want := openBytes(t, v1Key), labelKey(t, seed, "FOG_STORAGE_KEY_V1"); !bytes.Equal(got, want) {
		t.Fatal("the v1 storage key changed")
	}
	if got, want := openBytes(t, v2Key), labelKey(t, seed, "echofog/storage2/key"); !bytes.Equal(got, want) {
		t.Fatal("the v2 storage key does not use its label")
	}
}

func labelKey(t *testing.T, seed []byte, info string) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, seed, nil, []byte(info)), key); err != nil {
		t.Fatal(err)
	}
	defer memguard.WipeBytes(key)
	return append([]byte(nil), key...)
}

// staticIdentity is an identity key source outside the account derivation: v2 keeps the key at
// index 1' unused and stores a random X3DH identity DH key per device (objects §5).
type staticIdentity struct {
	enc *memguard.Enclave
	pub []byte
}

func (s staticIdentity) GetEncryptionKey() (*memguard.Enclave, []byte, error) {
	return s.enc, s.pub, nil
}

func TestIdentityKeySourceIsParameter(t *testing.T) {
	defer memguard.Purge()

	priv := bytes.Repeat([]byte{0x11}, 32)
	var xPriv, pub [32]byte
	copy(xPriv[:], priv)
	curve25519.ScalarBaseMult(&pub, &xPriv)
	src := staticIdentity{enc: memguard.NewEnclave(append([]byte(nil), priv...)), pub: pub[:]}

	acc, err := NewKeyManager(newRoot(t), &mockStorage{data: map[string][]byte{}}).
		GetAccountWithOptions(0, Options{IdentityKeySource: src})
	if err != nil {
		t.Fatal(err)
	}
	gotEnc, gotPub, err := acc.GetEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(openBytes(t, gotEnc), priv) {
		t.Fatal("the account ignored the identity key source")
	}
	if !bytes.Equal(gotPub, pub[:]) {
		t.Fatal("the public half does not match the identity key source")
	}

	// a bundle of the account carries the same key
	bundle, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bundle.IdentityKey[:], pub[:]) {
		t.Fatal("the bundle does not use the identity key source")
	}
}

// The default account still derives the X3DH identity DH key at index 1' of the account. The
// expected bytes are fixed: SLIP-0010 m/44'/999'/0'/1' from the root bytes 0x00..0x3f, worked out
// by hand with HMAC-SHA512 outside this code (the key is that of v0.2.0, a node that changed it
// would lose every contact), not derived by the code under test.
func TestDefaultIdentityKeyIsTheAccountDerivation(t *testing.T) {
	defer memguard.Purge()

	const (
		wantPriv = "25b6384a2b2e5944698a2c3e675e012941c6f78411e63e76d4cdb5c79fc1ed3a"
		wantPub  = "b3c3c8bbe0b97d3e2c88ef0b8b2a4ffbd5a2c7acb791f3d69005b54d0a86d907"
	)
	root := make([]byte, 64)
	for i := range root {
		root[i] = byte(i)
	}
	acc, err := NewKeyManager(root, &mockStorage{data: map[string][]byte{}}).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	gotEnc, gotPub, err := acc.GetEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(openBytes(t, gotEnc)); got != wantPriv {
		t.Fatalf("the default identity key changed: %s", got)
	}
	if got := hex.EncodeToString(gotPub); got != wantPub {
		t.Fatalf("the default identity public key changed: %s", got)
	}
}
