package store

import (
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Schema is the storage schema number of protocol v2 (versioning.md §3.7).
const Schema uint32 = 200

// valueLogGCInterval is how often the value log is garbage-collected; every round is jittered by
// ±20 % (AD13).
const valueLogGCInterval = 5 * time.Minute

var (
	// ErrClosed is returned by every operation after Close.
	ErrClosed = errors.New("store: closed")
	// ErrNotFound is returned when there is no record under the key.
	ErrNotFound = errors.New("store: key not found")
	// ErrBadKey is returned when the storage key is not 32 bytes.
	ErrBadKey = errors.New("store: storage key must be 32 bytes")
	// ErrBadNamespace is returned for an empty or over-long namespace name.
	ErrBadNamespace = errors.New("store: bad namespace name")
	// ErrBadLimit is returned by a scan whose limit is outside 1..MaxScanLimit.
	ErrBadLimit = errors.New("store: scan limit is out of range")
	// ErrBadTTL is returned by PutTTL with a non-positive TTL.
	ErrBadTTL = errors.New("store: TTL must be positive")
	// ErrValueDamaged is returned when a stored value does not open: the storage key is not the
	// one the value was written with, or the value was changed on disk.
	ErrValueDamaged = errors.New("store: stored value does not open")
	// ErrSchemaNewer is returned for a store written by a newer build.
	ErrSchemaNewer = errors.New("store: schema is newer than this build")
	// ErrSchemaUnknown is returned when no migration path from the stored schema exists.
	ErrSchemaUnknown = errors.New("store: no migration from this schema")
	// ErrSchemaMissing is returned when a directory holds a database but no schema number.
	ErrSchemaMissing = errors.New("store: database without a schema number")
	// ErrSchemaCorrupt is returned when the schema number cannot be read.
	ErrSchemaCorrupt = errors.New("store: schema number cannot be read")
	// ErrNoDir is returned by Open without a directory.
	ErrNoDir = errors.New("store: no directory")
)

// Options configures a store.
type Options struct {
	// Dir is the store directory, usually Path(<data>).
	Dir string

	// Key is the account's 32-byte storage key (T4.1, identity.Account.StorageKey). It never
	// touches the disk; the caller keeps ownership of the Value.
	Key *secret.Value

	// MemCapMB is the platform flag of the same name (platforms.md §2). Zero or less selects the
	// desktop/server options; a positive value keeps badger inside a phone's memory budget
	// (platforms.md §1.2).
	MemCapMB int

	// Log receives one warning per repair or migration. Nil is silent.
	Log *slog.Logger
}

// Store is one profile's database. Its methods are safe for concurrent use.
type Store struct {
	dir  string
	db   *badger.DB
	aead cipher.AEAD

	mu     sync.RWMutex
	closed bool

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// Path returns the v2 store directory inside a profile's data directory (AD7). The data directory
// is the profile's directory, filepath.Dir(DatabasePath) in v1 terms.
func Path(dataDir string) string {
	return filepath.Join(dataDir, "v2")
}

// Open opens or creates the store in opts.Dir.
//
// A new directory starts at Schema. A store with a higher number is refused untouched. A lower
// number is migrated forward after a copy of the whole directory is written; a schema with no
// migration step is refused. Damaged tables left by a crash are repaired before badger opens them.
func Open(opts Options) (*Store, error) {
	if opts.Dir == "" {
		return nil, ErrNoDir
	}
	if opts.Key == nil {
		return nil, fmt.Errorf("%w: no key", ErrBadKey)
	}
	if opts.Key.Size() != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("%w: got %d", ErrBadKey, opts.Key.Size())
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", opts.Dir, err)
	}

	version, err := readSchema(opts.Dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := ensureNewDir(opts.Dir); err != nil {
			return nil, err
		}
		if err := writeSchema(opts.Dir, Schema); err != nil {
			return nil, err
		}
		version = Schema
	case err != nil:
		return nil, err
	}

	if version > Schema {
		return nil, fmt.Errorf("%w: store %d, this build writes %d", ErrSchemaNewer, version, Schema)
	}
	if version < Schema && !migrationPathExists(version) {
		return nil, fmt.Errorf("%w: %d", ErrSchemaUnknown, version)
	}

	badgerOpts := badgerOptions(opts)

	// A table left unwritten by a killed process makes badger panic on open in a goroutine that
	// cannot be recovered; repair first, with the size this store was written with.
	if err := repairTables(opts.Dir, badgerOpts, opts.Log); err != nil {
		return nil, err
	}
	if version < Schema {
		backup, err := backupDir(opts.Dir)
		if err != nil {
			return nil, err
		}
		if opts.Log != nil {
			opts.Log.Warn("store: migrating", "from", version, "to", Schema, "backup", backup)
		}
	}

	db, err := badger.Open(badgerOpts)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", opts.Dir, err)
	}

	if version < Schema {
		if err := migrate(db, version, opts.Key); err != nil {
			db.Close()
			return nil, err
		}
		// Record the new number only after every step succeeded, so a failed migration is retried
		// from the same place on the next start.
		if err := writeSchema(opts.Dir, Schema); err != nil {
			db.Close()
			return nil, err
		}
	}

	aead, err := newAEAD(opts.Key)
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{
		dir:  opts.Dir,
		db:   db,
		aead: aead,
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.gcLoop()
	return s, nil
}

// Store returns a namespace: the KV that node.Host.Store(ns) hands to a service. An invalid name
// yields a KV whose every operation reports ErrBadNamespace; it never panics and never silently
// shares records with another namespace.
func (s *Store) Store(ns string) KV {
	prefix, err := namespacePrefix(ns)
	if err != nil {
		return badKV{err: err}
	}
	return &namespace{store: s, prefix: prefix}
}

// Dir returns the directory the store lives in.
func (s *Store) Dir() string { return s.dir }

// Close stops the background work and closes the database. It is idempotent; later operations
// return ErrClosed.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.wg.Wait()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// open takes the read lock for one operation; it reports false once the store is closed.
func (s *Store) open() bool {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false
	}
	return true
}

// gcLoop collects the value log; without it deleted and overwritten values keep the files growing.
func (s *Store) gcLoop() {
	defer s.wg.Done()
	for {
		timer := time.NewTimer(jitter(valueLogGCInterval))
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		if !s.open() {
			return
		}
		for s.db.RunValueLogGC(0.5) == nil {
		}
		s.mu.RUnlock()
	}
}

// jitter returns d in [0.8d, 1.2d] (AD13).
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*mathrand.Float64()))
}

// newAEAD derives the value key from the storage key. The derivation separates the encryption of
// stored values from other uses of the same key (for example the at-rest wrap in T4.1).
func newAEAD(key *secret.Value) (cipher.AEAD, error) {
	var aead cipher.AEAD
	err := key.Use(func(keyBytes []byte) error {
		subkey := make([]byte, chacha20poly1305.KeySize)
		defer secret.Wipe(subkey)
		if _, err := io.ReadFull(hkdf.New(sha256.New, keyBytes, nil, []byte("echofog/store2/value")), subkey); err != nil {
			return fmt.Errorf("store: deriving the value key: %w", err)
		}
		a, err := chacha20poly1305.NewX(subkey)
		if err != nil {
			return fmt.Errorf("store: value cipher: %w", err)
		}
		aead = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return aead, nil
}
