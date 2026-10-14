package storage_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/internal/testutil"
)

func sealedOver(t *testing.T, raw storage.Storage) *storage.Sealed {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	s, err := storage.NewSealed(raw, memguard.NewEnclave(k))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealedValuesAreUnreadableUnderneath(t *testing.T) {
	db := testutil.NewTestingBadgerStorage(t)
	defer db.CloseAndClear()
	s := sealedOver(t, db.St)

	secret := []byte("meet me at the old bridge at nine")
	key := []byte("m:secret-message-key-000000000")
	if err := s.Set(key, secret); err != nil {
		t.Fatal(err)
	}
	raw, _ := db.St.Get(key)
	if bytes.Contains(raw, secret) {
		t.Fatal("the message lies in the open")
	}
	got, err := s.Get(key)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("%q %v", got, err)
	}

	// values outside the personal prefixes pass through unchanged
	_ = s.Set([]byte("bundle:x"), []byte("public"))
	if v, _ := db.St.Get([]byte("bundle:x")); string(v) != "public" {
		t.Fatal("a public record was sealed")
	}

	// a sealed value moved under another key does not open
	_ = db.St.Set([]byte("m:other"), raw)
	if _, err := s.Get([]byte("m:other")); err == nil {
		t.Fatal("a value opened under another key")
	}
	// scans skip what does not open, written by anyone else
	_ = db.St.Set([]byte("m:planted"), []byte("plain text from somebody"))
	all, err := s.FindValues([]byte("m:"), 0)
	if err != nil || len(all) != 1 || !bytes.Equal(all[0].Value, secret) {
		t.Fatalf("scan returned %d entries (%v)", len(all), err)
	}

	// another key cannot read it
	if _, err := sealedOver(t, db.St).Get(key); err == nil {
		t.Fatal("a foreign key opened the value")
	}
}

func TestBlindingIsStableAndHidesTheID(t *testing.T) {
	db := testutil.NewTestingBadgerStorage(t)
	defer db.CloseAndClear()
	s := sealedOver(t, db.St)
	id := "c793c5f38b47ec789470845d3676855ad08429d758e6927a2c5587838a508b8c"
	if s.BlindID(id) != s.BlindID(id) || s.BlindChat(42) != s.BlindChat(42) {
		t.Fatal("blinding is not stable")
	}
	if s.BlindID(id) == id || len(s.BlindID(id)) != 32 || s.BlindChat(42) == 42 {
		t.Fatal("the id is not blinded")
	}
	if storage.BlindID(db.St, id) != id {
		t.Fatal("a plain storage must not blind")
	}
	if sealedOver(t, db.St).BlindID(id) == s.BlindID(id) {
		t.Fatal("blinding does not depend on the key")
	}
}
