package store

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

const (
	schemaFile = "SCHEMA"
	schemaTemp = "SCHEMA.tmp"
)

// schemaMagic follows the number, so a file that happens to hold a small number is not taken for
// a store.
var schemaMagic = []byte("\x00EchoFog v2 store schema\x00")

// readSchema reads the schema number that starts a store directory.
func readSchema(dir string) (uint32, error) {
	raw, err := os.ReadFile(filepath.Join(dir, schemaFile))
	if err != nil {
		return 0, err
	}
	if len(raw) < 4+len(schemaMagic) || !bytes.Equal(raw[4:4+len(schemaMagic)], schemaMagic) {
		return 0, fmt.Errorf("%w: %s", ErrSchemaCorrupt, filepath.Join(dir, schemaFile))
	}
	return binary.BigEndian.Uint32(raw[:4]), nil
}

// writeSchema writes the schema number atomically: the file's first four bytes are the number.
func writeSchema(dir string, version uint32) error {
	buf := make([]byte, 4+len(schemaMagic))
	binary.BigEndian.PutUint32(buf, version)
	copy(buf[4:], schemaMagic)

	tmp := filepath.Join(dir, schemaTemp)
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("store: writing the schema number: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, schemaFile)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: writing the schema number: %w", err)
	}
	return nil
}

// ensureNewDir refuses to adopt a directory that already holds something: a database without a
// schema number is not ours to open.
func ensureNewDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.Name() == schemaTemp {
			// A creation interrupted between the temporary file and the rename.
			if err := os.Remove(filepath.Join(dir, schemaTemp)); err != nil {
				return fmt.Errorf("store: removing %s: %w", schemaTemp, err)
			}
			continue
		}
		return fmt.Errorf("%w: %s", ErrSchemaMissing, dir)
	}
	return nil
}

// migration is one step from schema version to version+1. It sees the database before the store
// is wrapped, and the account's storage key so it can open and rewrite encrypted values. A step
// must be idempotent: the new schema number is written only after it returns, so a crash in
// between runs the step again on the next start.
type migration func(db *badger.DB, key *secret.Value) error

var (
	migrationMu sync.Mutex
	// migrations is empty at schema 200: v2 is the first schema and nothing older is known. A
	// later schema adds its step here; the test injects one to exercise the mechanism.
	migrations = map[uint32]migration{}
)

func migrationFor(version uint32) (migration, bool) {
	migrationMu.Lock()
	defer migrationMu.Unlock()
	step, ok := migrations[version]
	return step, ok
}

// migrationPathExists reports whether every schema from version up to Schema has a step.
func migrationPathExists(version uint32) bool {
	for ; version < Schema; version++ {
		if _, ok := migrationFor(version); !ok {
			return false
		}
	}
	return true
}

func migrate(db *badger.DB, from uint32, key *secret.Value) error {
	for version := from; version < Schema; version++ {
		step, ok := migrationFor(version)
		if !ok {
			return fmt.Errorf("%w: %d", ErrSchemaUnknown, version)
		}
		if err := step(db, key); err != nil {
			return fmt.Errorf("store: migrating schema %d: %w", version, err)
		}
	}
	return nil
}

// backupDir copies the whole store directory next to itself before a migration changes it.
func backupDir(dir string) (string, error) {
	dst := dir + ".backup-" + time.Now().UTC().Format("20060102-150405.000000000")
	if err := copyDir(dir, dst); err != nil {
		return "", fmt.Errorf("store: keeping a copy before the migration: %w", err)
	}
	return dst, nil
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
