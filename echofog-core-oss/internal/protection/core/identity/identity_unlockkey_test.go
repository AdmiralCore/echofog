package identity

import (
	"bytes"
	"errors"
	"testing"

	"github.com/awnumar/memguard"
)

// The key a password becomes opens the identity without the password, until the root is sealed
// again with a new password; a wrong password gives no key.
func TestUnlockKeyOpensTheIdentityWithoutThePassword(t *testing.T) {
	memguard.CatchInterrupt()
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)
	if err := im.RestoreFromMnemonic([]byte(testMnemonic), []byte("")); err != nil {
		t.Fatal(err)
	}
	want, _ := im.GetOwnPeerID()
	if err := im.SaveToStorage([]byte("the-pass")); err != nil {
		t.Fatal(err)
	}

	if _, err := newIdentityManager(st).UnlockKey([]byte("typo")); !errors.Is(err, ErrInvalidPass) {
		t.Fatalf("a wrong password gave %v, want ErrInvalidPass", err)
	}
	key, err := newIdentityManager(st).UnlockKey([]byte("the-pass"))
	if err != nil {
		t.Fatal(err)
	}

	kept := append([]byte(nil), key...)
	byKey := newIdentityManager(st)
	if err := byKey.LoadWithKey(key); err != nil {
		t.Fatalf("the key did not open the identity: %v", err)
	}
	if got, _ := byKey.GetOwnPeerID(); got != want {
		t.Fatal("the key opened another identity")
	}
	if !bytes.Equal(key, kept) {
		t.Fatal("LoadWithKey must leave the caller's key as it is")
	}

	// a new password seals the root with a new salt: the old key no longer opens it
	if err := byKey.SaveToStorage([]byte("new-pass")); err != nil {
		t.Fatal(err)
	}
	if err := newIdentityManager(st).LoadWithKey(key); !errors.Is(err, ErrInvalidPass) {
		t.Fatalf("a key of the old password gave %v, want ErrInvalidPass", err)
	}
}
