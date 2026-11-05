package store

import (
	"bytes"
	"errors"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

// withRawDB opens the badger directory behind the store's back for a test that has to change or
// remove a store-level record.
func withRawDB(t *testing.T, dir string, fn func(txn *badger.Txn) error) {
	t.Helper()

	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	defer db.Close()
	if err := db.Update(fn); err != nil {
		t.Fatalf("change raw badger: %v", err)
	}
}

func rawExists(t *testing.T, dir string, key []byte) bool {
	t.Helper()

	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	defer db.Close()

	err = db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		return err
	})
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		return false
	case err != nil:
		t.Fatalf("read raw key: %v", err)
	default:
		return true
	}
}

// TestVerifierIsCreatedOnFirstOpen pins the record written by a fresh store: the key check is part
// of the store from then on, and the schema number stays 200.
func TestVerifierIsCreatedOnFirstOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	key := testKey(t, 31)
	st := openStore(t, dir, key, 0)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want, err := verifierValue(key)
	if err != nil {
		t.Fatalf("verifierValue: %v", err)
	}
	if got := readRaw(t, dir, verifierKey); !bytes.Equal(got, want) {
		t.Fatalf("the verifier = %x, want %x", got, want)
	}
	if version, err := readSchema(dir); err != nil || version != Schema {
		t.Fatalf("the verifier changed the schema: %d, %v", version, err)
	}
}

// TestOldStoreWithoutVerifierIsAdopted pins the backward-compatible format: a database written by
// T4.3 has no verifier, opens with its key, and is given one for the next open.
func TestOldStoreWithoutVerifierIsAdopted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	key := testKey(t, 32)
	st := openStore(t, dir, key, 0)
	if err := st.Store("chat").Put([]byte("k"), []byte("kept")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	withRawDB(t, dir, func(txn *badger.Txn) error {
		return txn.Delete(verifierKey)
	})
	if rawExists(t, dir, verifierKey) {
		t.Fatal("the verifier was not removed")
	}

	st = openStore(t, dir, key, 0)
	if got, err := st.Store("chat").Get([]byte("k")); err != nil || string(got) != "kept" {
		t.Fatalf("Get from an adopted store = %q, %v", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rawExists(t, dir, verifierKey) {
		t.Fatal("the adopted store did not get a verifier")
	}

	if _, err := Open(Options{Dir: dir, Key: testKey(t, 33)}); !errors.Is(err, ErrWrongStorageKey) {
		t.Fatalf("Open of the adopted store with another key: %v, want ErrWrongStorageKey", err)
	}
}

// TestDamagedVerifierIsRefused pins that a verifier that does not compare makes Open fail closed,
// rather than being treated as absent and overwritten.
func TestDamagedVerifierIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	key := testKey(t, 34)
	st := openStore(t, dir, key, 0)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	withRawDB(t, dir, func(txn *badger.Txn) error {
		return txn.Set(verifierKey, []byte("not a verifier"))
	})

	if _, err := Open(Options{Dir: dir, Key: key}); !errors.Is(err, ErrWrongStorageKey) {
		t.Fatalf("Open with a damaged verifier: %v, want ErrWrongStorageKey", err)
	}
	if got := readRaw(t, dir, verifierKey); string(got) != "not a verifier" {
		t.Fatalf("Open rewrote the damaged verifier: %q", got)
	}
}
