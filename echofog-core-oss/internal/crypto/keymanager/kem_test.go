package keymanager

import (
	"bytes"
	"crypto/mlkem"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/awnumar/memguard"
)

func TestBundleCarriesSignedKEMPreKey(t *testing.T) {
	defer memguard.Purge()
	acc, st := testAccount(t)
	if acc.KEMSince() != 0 {
		t.Fatal("a fresh account has offered no KEM prekey yet")
	}
	b, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != BundleVersionPQ || len(b.KEMPreKey) != mlkem.EncapsulationKeySize768 {
		t.Fatalf("version %d, KEM prekey of %d bytes", b.Version, len(b.KEMPreKey))
	}
	if err := VerifyBundle(b); err != nil {
		t.Fatal(err)
	}
	if since := acc.KEMSince(); since == 0 || since > b.CreatedAt {
		t.Fatalf("KEMSince %d must be set and not after the bundle (%d)", since, b.CreatedAt)
	}

	seedEnc, err := acc.OpenKEMPreKey(KEMKeyID(b.KEMPreKey))
	if err != nil {
		t.Fatal(err)
	}
	seed := openBytes(t, seedEnc)
	pub, err := encapsulationKey(seed)
	if err != nil || !bytes.Equal(pub, b.KEMPreKey) {
		t.Fatal("the stored seed is not the one of the published KEM prekey")
	}
	for k, v := range st.data {
		if bytes.Contains(v, seed) {
			t.Fatalf("the KEM seed lies in the open under %q", k)
		}
	}
	if _, err := acc.OpenKEMPreKey([32]byte{1}); !errors.Is(err, ErrKEMKeyNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}

	// the same key until it is replaced
	again, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.KEMPreKey, b.KEMPreKey) {
		t.Fatal("the KEM prekey changed before its time")
	}
}

func TestTamperedKEMPreKeyIsRejected(t *testing.T) {
	defer memguard.Purge()
	acc, _ := testAccount(t)
	good, err := acc.CreateBundleWithOPKs(2)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() *PublicBundle {
		c := *good
		c.KEMPreKey = append([]byte(nil), good.KEMPreKey...)
		c.OneTimeKeys = append([][32]byte(nil), good.OneTimeKeys...)
		return &c
	}
	for name, c := range map[string]struct {
		tamper func(*PublicBundle)
		want   error
	}{
		"KEM key byte":      {func(b *PublicBundle) { b.KEMPreKey[100] ^= 1 }, ErrInvalidKEMPreKey},
		"KEM key cut":       {func(b *PublicBundle) { b.KEMPreKey = b.KEMPreKey[:1000] }, ErrInvalidKEMPreKey},
		"KEM signature":     {func(b *PublicBundle) { b.KEMSignature[3] ^= 1 }, ErrInvalidKEMPreKey},
		"version raised":    {func(b *PublicBundle) { b.Version = 3 }, ErrInvalidKEMPreKey},
		"version dropped":   {func(b *PublicBundle) { b.Version = 0 }, ErrInvalidKEMPreKey},
		"one-time key gone": {func(b *PublicBundle) { b.OneTimeKeys = b.OneTimeKeys[:1] }, ErrInvalidSignature},
		"key gone, version kept": {func(b *PublicBundle) {
			b.KEMPreKey, b.KEMSignature = nil, [64]byte{}
		}, ErrKEMPreKeyMissing},
		"key gone, signature kept": {func(b *PublicBundle) {
			b.KEMPreKey, b.Version = nil, 0
		}, ErrKEMPreKeyMissing},
		"another node's KEM key": {func(b *PublicBundle) {
			other, _ := testAccount(t)
			ob, err := other.CreateMyBundle(nil)
			if err != nil {
				t.Fatal(err)
			}
			b.KEMPreKey, b.KEMSignature = ob.KEMPreKey, ob.KEMSignature
		}, ErrInvalidKEMPreKey},
	} {
		b := clone()
		c.tamper(b)
		if err := VerifyBundle(b); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}

	// Stripped of all three fields the bundle is what v0.2.0 publishes: its classic signature
	// holds, so the bundle alone cannot show the stripping. That is caught where the bundle travels
	// (DHT records and envelopes are signed over all its bytes) and by the X3DH layer.
	b := clone()
	b.Version, b.KEMPreKey, b.KEMSignature = 0, nil, [64]byte{}
	if err := VerifyBundle(b); err != nil {
		t.Fatalf("a classic bundle must still verify: %v", err)
	}
}

// After a week the KEM prekey is replaced; the old one still opens until no bundle can name it.
func TestKEMPreKeyRotationKeepsTheOldOne(t *testing.T) {
	defer memguard.Purge()
	acc, st := testAccount(t)
	oldPub, err := acc.KEMPreKey()
	if err != nil {
		t.Fatal(err)
	}
	since := acc.KEMSince()
	oldSeed := openBytes(t, mustOpenKEM(t, acc, oldPub))

	age := func(name []byte, by time.Duration) {
		t.Helper()
		plain := timedSeed(time.Now().Add(-by), oldSeed)
		sealed, err := acc.seal(name, plain)
		if err != nil {
			t.Fatal(err)
		}
		st.data[string(name)] = sealed
	}
	age([]byte(kemCurrentKey), KEMPreKeyTTL+time.Hour)

	newPub, err := acc.KEMPreKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(newPub, oldPub) {
		t.Fatal("the KEM prekey was not replaced")
	}
	if acc.KEMSince() != since {
		t.Fatal("KEMSince is the first KEM prekey, it does not move")
	}
	mustOpenKEM(t, acc, newPub)
	if !bytes.Equal(openBytes(t, mustOpenKEM(t, acc, oldPub)), oldSeed) {
		t.Fatal("the replaced KEM prekey must still open")
	}

	id := KEMKeyID(oldPub)
	oldName := append([]byte(kemOldPrefix), id[:]...)
	age(oldName, KEMPreKeyRetention+time.Hour)
	if _, err := acc.OpenKEMPreKey(id); !errors.Is(err, ErrKEMKeyNotFound) {
		t.Fatalf("a KEM prekey past its retention opened: %v", err)
	}
	acc.lastPurge = time.Time{}
	acc.purgeOld()
	if _, ok := st.data[string(oldName)]; ok {
		t.Fatal("an expired KEM prekey is still stored")
	}
}

func mustOpenKEM(t *testing.T, acc *AccountManager, pub []byte) *memguard.Enclave {
	t.Helper()
	e, err := acc.OpenKEMPreKey(KEMKeyID(pub))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestKEMSinceIsReadFromStorage(t *testing.T) {
	acc, st := testAccount(t)
	st.data[kemSinceKey] = binary.BigEndian.AppendUint64(nil, 12345)
	if acc.KEMSince() != 12345 {
		t.Fatal("KEMSince must come from storage")
	}
	if _, err := acc.KEMPreKey(); err != nil {
		t.Fatal(err)
	}
	if acc.KEMSince() != 12345 {
		t.Fatal("a known KEMSince is not overwritten")
	}
}

// A clock that ran ahead when the first KEM prekey was made must not leave kem:since in the future:
// it comes back to the present with the next bundle, and only ever moves back.
func TestKEMSinceFromAClockAheadIsCorrected(t *testing.T) {
	defer memguard.Purge()
	acc, st := testAccount(t)
	if _, err := acc.KEMPreKey(); err != nil {
		t.Fatal(err)
	}
	ahead := time.Now().AddDate(4, 0, 0).Unix()
	st.data[kemSinceKey] = binary.BigEndian.AppendUint64(nil, uint64(ahead))
	b, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	since := acc.KEMSince()
	if since == 0 || since > b.CreatedAt {
		t.Fatalf("kem:since %d after the bundle (%d) that carries a KEM prekey", since, b.CreatedAt)
	}
	if _, err := acc.KEMPreKey(); err != nil {
		t.Fatal(err)
	}
	if acc.KEMSince() != since {
		t.Fatal("a corrected kem:since must stay put")
	}
}
