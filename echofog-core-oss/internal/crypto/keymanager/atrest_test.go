package keymanager

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"

	"github.com/awnumar/memguard"
)

func testAccount(t *testing.T) (*AccountManager, *mockStorage) {
	t.Helper()
	st := &mockStorage{data: map[string][]byte{}}
	root := make([]byte, 64)
	if _, err := rand.Read(root); err != nil {
		t.Fatal(err)
	}
	acc, err := NewKeyManager(root, st).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	return acc, st
}

func openBytes(t *testing.T, e *memguard.Enclave) []byte {
	t.Helper()
	b, err := e.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Destroy()
	return append([]byte(nil), b.Bytes()...)
}

func TestPrekeysAreSealedInStorage(t *testing.T) {
	acc, st := testAccount(t)
	spk, _, err := acc.GetSignedPreKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := acc.CreateBundleWithOPKs(3)
	if err != nil {
		t.Fatal(err)
	}
	secrets := [][]byte{openBytes(t, spk)}
	for _, pub := range b.OneTimeKeys {
		opk, err := acc.OpenOPK(pub)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, openBytes(t, opk))
	}
	for k, v := range st.data {
		for _, s := range secrets {
			if bytes.Contains(v, s) {
				t.Fatalf("a private key lies in the open under %q", k)
			}
		}
	}

	// a sealed value cannot be moved under another name
	name := append([]byte(opkPrefix), b.OneTimeKeys[0][:]...)
	other := append([]byte(opkPrefix), b.OneTimeKeys[1][:]...)
	st.data[string(other)] = st.data[string(name)]
	if _, err := acc.OpenOPK(b.OneTimeKeys[1]); err == nil {
		t.Fatal("a sealed prekey opened under another name")
	}
}

func TestLegacyPlainPrekeysStillOpen(t *testing.T) {
	acc, st := testAccount(t)
	pubs, _, err := GenerateOPKBatch(st) // the old, unsealed format
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acc.ConsumeOPK(pubs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := acc.OpenOPK(pubs[0]); err == nil {
		t.Fatal("a consumed prekey is still there")
	}
}

// After a week the signed prekey is replaced, and the old one is kept for bundles that name it.
func TestSignedPrekeyRotationKeepsTheOldOne(t *testing.T) {
	acc, st := testAccount(t)
	oldPriv, oldPub, err := acc.GetSignedPreKey()
	if err != nil {
		t.Fatal(err)
	}
	old := openBytes(t, oldPriv)

	// age it past the TTL
	plain := make([]byte, 40)
	binary.BigEndian.PutUint64(plain[:8], uint64(time.Now().Add(-SignedPreKeyTTL-time.Hour).Unix()))
	copy(plain[8:], old)
	sealed, err := acc.seal([]byte(spkCurrentKey), plain)
	if err != nil {
		t.Fatal(err)
	}
	st.data[spkCurrentKey] = sealed

	_, newPub, err := acc.GetSignedPreKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(newPub, oldPub) {
		t.Fatal("the signed prekey was not replaced")
	}
	all, err := acc.SignedPreKeys()
	if err != nil || len(all) != 2 {
		t.Fatalf("want current and old, got %d (%v)", len(all), err)
	}
	if !bytes.Equal(openBytes(t, all[1]), old) {
		t.Fatal("the old signed prekey was not kept")
	}

	// once no bundle can name it any more, it goes
	oldName := append([]byte(spkOldPrefix), oldPub...)
	binary.BigEndian.PutUint64(plain[:8], uint64(time.Now().Add(-SignedPreKeyRetention-time.Hour).Unix()))
	if st.data[string(oldName)], err = acc.seal(oldName, plain); err != nil {
		t.Fatal(err)
	}
	acc.lastPurge = time.Time{}
	acc.purgeOld()
	if all, _ := acc.SignedPreKeys(); len(all) != 1 {
		t.Fatalf("an expired signed prekey is still kept (%d)", len(all))
	}
}
