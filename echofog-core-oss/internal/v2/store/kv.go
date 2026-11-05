package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/dgraph-io/badger/v4"
)

// MaxScanLimit is the largest number of records one scan may return. A scan always has a limit:
// an unbounded walk over a profile's keys is a denial-of-service against the node itself.
const MaxScanLimit = 1000

// namespaceRecord is the first byte of every record key; it leaves room for store-level keys in
// front of the namespaces later.
const namespaceRecord byte = 0x01

// Entry is one record returned by a scan. The key is relative to the namespace.
type Entry struct {
	Key   []byte
	Value []byte
}

// KV is one namespace of the store: what node.Host.Store(ns) returns to a service
// (02-architecture §3.1). Implementations are safe for concurrent use.
type KV interface {
	// Get returns the value under key, or ErrNotFound.
	Get(key []byte) ([]byte, error)
	// Put stores value under key.
	Put(key, value []byte) error
	// PutTTL stores value under key until ttl passes.
	PutTTL(key, value []byte, ttl time.Duration) error
	// Delete removes key; deleting a missing key is not an error.
	Delete(key []byte) error
	// Has reports whether key has a record.
	Has(key []byte) (bool, error)
	// Scan returns up to limit records whose keys start with prefix, in key order. A nil prefix
	// means the whole namespace.
	Scan(prefix []byte, limit int) ([]Entry, error)
	// ScanFrom is Scan starting strictly after start: a paginator passes the last returned key and
	// never sees it again. A start that is not inside prefix (for example a cursor before the
	// prefix) starts at the first record of the prefix.
	ScanFrom(prefix, start []byte, limit int) ([]Entry, error)
}

// namespacePrefix encodes the namespace into the badger key. The length byte keeps names apart
// even when their concatenations would be equal.
func namespacePrefix(ns string) ([]byte, error) {
	if ns == "" || len(ns) > 255 {
		// The name is not echoed: a caller could have built it from user data.
		return nil, fmt.Errorf("%w: %d bytes", ErrBadNamespace, len(ns))
	}
	prefix := make([]byte, 2+len(ns))
	prefix[0] = namespaceRecord
	prefix[1] = byte(len(ns))
	copy(prefix[2:], ns)
	return prefix, nil
}

// namespace is a KV bound to one prefix.
type namespace struct {
	store  *Store
	prefix []byte
}

// fullKey returns the badger key of a namespace key. The result does not alias key or the prefix.
func (n *namespace) fullKey(key []byte) []byte {
	full := make([]byte, len(n.prefix)+len(key))
	copy(full, n.prefix)
	copy(full[len(n.prefix):], key)
	return full
}

func (n *namespace) Get(key []byte) ([]byte, error) {
	if !n.store.open() {
		return nil, ErrClosed
	}
	defer n.store.mu.RUnlock()

	full := n.fullKey(key)
	var value []byte
	err := n.store.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(full)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		raw, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		value, err = n.store.openValue(full, raw)
		return err
	})
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (n *namespace) Put(key, value []byte) error {
	return n.put(key, value, 0)
}

func (n *namespace) PutTTL(key, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: %s", ErrBadTTL, ttl)
	}
	return n.put(key, value, ttl)
}

func (n *namespace) put(key, value []byte, ttl time.Duration) error {
	if !n.store.open() {
		return ErrClosed
	}
	defer n.store.mu.RUnlock()

	full := n.fullKey(key)
	sealed, err := n.store.sealValue(full, value)
	if err != nil {
		return err
	}
	return n.store.db.Update(func(txn *badger.Txn) error {
		entry := badger.NewEntry(full, sealed)
		if ttl > 0 {
			entry = entry.WithTTL(ttl)
		}
		return txn.SetEntry(entry)
	})
}

func (n *namespace) Delete(key []byte) error {
	if !n.store.open() {
		return ErrClosed
	}
	defer n.store.mu.RUnlock()

	full := n.fullKey(key)
	return n.store.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(full)
	})
}

func (n *namespace) Has(key []byte) (bool, error) {
	if !n.store.open() {
		return false, ErrClosed
	}
	defer n.store.mu.RUnlock()

	full := n.fullKey(key)
	err := n.store.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(full)
		return err
	})
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

func (n *namespace) Scan(prefix []byte, limit int) ([]Entry, error) {
	return n.scan(prefix, nil, limit)
}

func (n *namespace) ScanFrom(prefix, start []byte, limit int) ([]Entry, error) {
	return n.scan(prefix, start, limit)
}

func (n *namespace) scan(prefix, start []byte, limit int) ([]Entry, error) {
	if limit <= 0 || limit > MaxScanLimit {
		return nil, fmt.Errorf("%w: %d", ErrBadLimit, limit)
	}
	if !n.store.open() {
		return nil, ErrClosed
	}
	defer n.store.mu.RUnlock()

	fullPrefix := n.fullKey(prefix)
	seek := fullPrefix
	if len(start) > 0 {
		// The cursor is exclusive. Appending 0x00 gives the smallest key strictly after start
		// (every longer key whose prefix is start begins with that byte or more). A cursor below
		// the prefix must not move the iterator past the prefix's records.
		if cursor := append(n.fullKey(start), 0x00); bytes.Compare(cursor, seek) > 0 {
			seek = cursor
		}
	}

	var entries []Entry
	err := n.store.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(seek); it.ValidForPrefix(fullPrefix); it.Next() {
			item := it.Item()
			full := item.KeyCopy(nil)
			raw, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			value, err := n.store.openValue(full, raw)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{Key: full[len(n.prefix):], Value: value})
			if len(entries) == limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// sealValue encrypts value under the full badger key, which is also its associated data.
func (s *Store) sealValue(key, value []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	out := make([]byte, nonceSize, nonceSize+len(value)+s.aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, fmt.Errorf("store: nonce: %w", err)
	}
	return s.aead.Seal(out, out[:nonceSize], value, key), nil
}

// openValue decrypts a stored value, checking that it belongs to key.
func (s *Store) openValue(key, raw []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize+s.aead.Overhead() {
		return nil, ErrValueDamaged
	}
	value, err := s.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], key)
	if err != nil {
		return nil, ErrValueDamaged
	}
	return value, nil
}

// badKV is the KV of an invalid namespace name: every operation reports why.
type badKV struct{ err error }

func (b badKV) Get([]byte) ([]byte, error)                    { return nil, b.err }
func (b badKV) Put([]byte, []byte) error                      { return b.err }
func (b badKV) PutTTL([]byte, []byte, time.Duration) error    { return b.err }
func (b badKV) Delete([]byte) error                           { return b.err }
func (b badKV) Has([]byte) (bool, error)                      { return false, b.err }
func (b badKV) Scan([]byte, int) ([]Entry, error)             { return nil, b.err }
func (b badKV) ScanFrom([]byte, []byte, int) ([]Entry, error) { return nil, b.err }
