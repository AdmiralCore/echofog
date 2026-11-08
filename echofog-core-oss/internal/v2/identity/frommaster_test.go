package identity

import (
	"crypto/hmac"
	"crypto/sha512"
	"errors"
	"testing"

	"github.com/tyler-smith/go-bip39"
)

const masterPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// masterOf is what the v1 key manager holds: the SLIP-0010 master of the BIP-39 seed.
func masterOf(phrase, passphrase string) (key, chain []byte) {
	seed := bip39.NewSeed(phrase, passphrase)
	h := hmac.New(sha512.New, []byte("ed25519 seed"))
	h.Write(seed)
	i := h.Sum(nil)
	return i[:32], i[32:]
}

// The same phrase gives the same account whether it starts from the phrase or from the v1 master:
// key, node ID and storage key (AD7, T11.7).
func TestFromMasterEqualsFromMnemonic(t *testing.T) {
	for _, idx := range []uint32{0, 1, 7} {
		want, err := FromMnemonicIndex(masterPhrase, "", idx)
		if err != nil {
			t.Fatal(err)
		}
		k, c := masterOf(masterPhrase, "")
		got, err := FromMaster(k, c, idx)
		if err != nil {
			t.Fatalf("FromMaster(%d): %v", idx, err)
		}
		if got.NodeID() != want.NodeID() || string(got.Public()) != string(want.Public()) {
			t.Fatalf("account %d differs from the phrase's", idx)
		}
		var a, b []byte
		_ = got.StorageKey().Use(func(x []byte) error { a = append(a, x...); return nil })
		_ = want.StorageKey().Use(func(x []byte) error { b = append(b, x...); return nil })
		if string(a) != string(b) || len(a) != 32 {
			t.Fatalf("storage key %d differs", idx)
		}
		got.Destroy()
		want.Destroy()
	}
}

// A passphrase changes the seed and so the master: the account follows it.
func TestFromMasterFollowsThePassphrase(t *testing.T) {
	k, c := masterOf(masterPhrase, "pass")
	got, err := FromMaster(k, c, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Destroy()
	want, _ := FromMnemonic(masterPhrase, "pass")
	defer want.Destroy()
	if got.NodeID() != want.NodeID() {
		t.Fatal("passphrase account differs")
	}
}

func TestFromMasterRefusesBadLengthAndKeepsTheCallersSlices(t *testing.T) {
	k, c := masterOf(masterPhrase, "")
	for _, tc := range [][2][]byte{{k[:31], c}, {k, c[:5]}, {nil, nil}} {
		if _, err := FromMaster(tc[0], tc[1], 0); !errors.Is(err, ErrMaster) {
			t.Fatalf("lengths %d/%d: %v", len(tc[0]), len(tc[1]), err)
		}
	}
	k0 := append([]byte(nil), k...)
	c0 := append([]byte(nil), c...)
	a, err := FromMaster(k, c, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Destroy()
	if string(k) != string(k0) || string(c) != string(c0) {
		t.Fatal("FromMaster changed the caller's slices")
	}
}
