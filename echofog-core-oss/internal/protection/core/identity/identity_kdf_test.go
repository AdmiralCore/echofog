package identity

import (
	"testing"

	"github.com/awnumar/memguard"
	"golang.org/x/crypto/chacha20poly1305"
	"google.golang.org/protobuf/proto"
)

// An identity root sealed at the old password setting (Argon2id t=1) still opens, and the
// unlock seals it again at the current one; it opens just the same afterwards.
func TestIdentityRootSealedAtTheOldSettingIsUpgraded(t *testing.T) {
	memguard.CatchInterrupt()
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)
	mnemonic := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err := im.RestoreFromMnemonic(append([]byte(nil), mnemonic...), []byte("x")); err != nil {
		t.Fatal(err)
	}
	want, _ := im.GetOwnPeerID()
	pass := []byte("old-setting-pass")
	if err := im.SaveToStorage(append([]byte(nil), pass...)); err != nil {
		t.Fatal(err)
	}
	im.PanicButton()

	// turn the stored root into one sealed the old way
	root := readRoot(t, st)
	if root.Algorithm != kdfCurrent {
		t.Fatalf("a new root is sealed with %q, want %q", root.Algorithm, kdfCurrent)
	}
	plain := openRoot(t, root, pass)
	old := passwordKey(pass, root.Salt, kdfT1)
	aead, _ := chacha20poly1305.NewX(old)
	root.EncryptedData = aead.Seal(nil, root.Nonce, plain, nil)
	root.Algorithm = kdfT1
	raw, _ := proto.Marshal(root)
	if err := st.Set([]byte("system:identity:root"), raw); err != nil {
		t.Fatal(err)
	}

	im2 := newIdentityManager(st)
	if err := im2.LoadFromStorage(append([]byte(nil), pass...), false); err != nil {
		t.Fatalf("the old root did not open: %v", err)
	}
	if got, _ := im2.GetOwnPeerID(); got != want {
		t.Fatal("the old root opened to another identity")
	}
	if up := readRoot(t, st); up.Algorithm != kdfCurrent {
		t.Fatalf("after unlock the root is still sealed with %q", up.Algorithm)
	}

	im3 := newIdentityManager(st)
	if err := im3.LoadFromStorage(append([]byte(nil), pass...), false); err != nil {
		t.Fatalf("the re-sealed root did not open: %v", err)
	}
	if got, _ := im3.GetOwnPeerID(); got != want {
		t.Fatal("the re-sealed root opened to another identity")
	}
}

func readRoot(t *testing.T, st interface{ Get([]byte) ([]byte, error) }) *IdentityKeysInStorage {
	t.Helper()
	raw, err := st.Get([]byte("system:identity:root"))
	if err != nil {
		t.Fatal(err)
	}
	var r IdentityKeysInStorage
	if err := proto.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func openRoot(t *testing.T, r *IdentityKeysInStorage, pass []byte) []byte {
	t.Helper()
	aead, _ := chacha20poly1305.NewX(passwordKey(pass, r.Salt, r.Algorithm))
	plain, err := aead.Open(nil, r.Nonce, r.EncryptedData, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}
