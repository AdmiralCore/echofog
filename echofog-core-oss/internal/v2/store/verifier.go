package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"golang.org/x/crypto/hkdf"
)

const (
	// storeRecord is the first byte of store-level records. Namespace records start with 0x01
	// (kv.go), so the two key spaces cannot overlap.
	storeRecord byte = 0x00

	verifierName  = "storage-key-verifier"
	verifierInfo  = "echofog/store2/keycheck"
	verifierLabel = "\x00EchoFog v2 storage key verifier\x00"
)

// verifierKey is the store-level record that identifies the storage key. It is not a secret: it
// holds a keyed MAC, which a reader without the storage key cannot forge.
var verifierKey = append([]byte{storeRecord}, verifierName...)

// verifierValue computes the record stored under verifierKey. A later open recomputes it from its
// key and compares the two: a different key gives a different MAC. The derivation is separate from
// the value key, so a verifier cannot be used to decrypt data.
func verifierValue(key *secret.Value) ([]byte, error) {
	var out []byte
	err := key.Use(func(keyBytes []byte) error {
		subkey := make([]byte, sha256.Size)
		defer secret.Wipe(subkey)
		if _, err := io.ReadFull(hkdf.New(sha256.New, keyBytes, nil, []byte(verifierInfo)), subkey); err != nil {
			return fmt.Errorf("store: deriving the key-check key: %w", err)
		}
		mac := hmac.New(sha256.New, subkey)
		mac.Write([]byte(verifierLabel))
		out = append([]byte(verifierLabel), mac.Sum(nil)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkStorageKey verifies the store's key before the caller can read or write any record. A store
// written before the verifier existed has no record: its first open trusts the supplied key and
// writes the verifier, so every later open is checked. A record that does not compare, including a
// damaged one, fails closed.
func checkStorageKey(db *badger.DB, key *secret.Value) error {
	want, err := verifierValue(key)
	if err != nil {
		return err
	}

	var got []byte
	err = db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(verifierKey)
		if err != nil {
			return err
		}
		got, err = item.ValueCopy(nil)
		return err
	})
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		if err := db.Update(func(txn *badger.Txn) error {
			return txn.Set(verifierKey, want)
		}); err != nil {
			return fmt.Errorf("store: writing the storage key verifier: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("store: reading the storage key verifier: %w", err)
	}

	if !hmac.Equal(got, want) {
		return ErrWrongStorageKey
	}
	return nil
}
