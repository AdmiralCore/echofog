package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// testOlderSchema is a schema number below 200 that only the tests know a migration for.
const testOlderSchema = 199

func registerMigration(t *testing.T, from uint32, step migration) {
	t.Helper()

	migrationMu.Lock()
	previous, had := migrations[from]
	migrations[from] = step
	migrationMu.Unlock()

	t.Cleanup(func() {
		migrationMu.Lock()
		defer migrationMu.Unlock()
		if had {
			migrations[from] = previous
		} else {
			delete(migrations, from)
		}
	})
}

func rawEntries(t *testing.T, dir string) map[string]string {
	t.Helper()

	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	defer db.Close()

	out := map[string]string{}
	err = db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			out[string(item.KeyCopy(nil))] = string(value)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan raw badger: %v", err)
	}
	return out
}

// TestMigrationKeepsABackup migrates a store whose schema is lower forward: it writes a copy
// first, runs the step, and only then records schema 200 (versioning.md §3.7).
func TestMigrationKeepsABackup(t *testing.T) {
	dir := t.TempDir()
	if err := writeSchema(dir, testOlderSchema); err != nil {
		t.Fatalf("writeSchema: %v", err)
	}
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte("raw/old"), []byte("kept"))
	}); err != nil {
		t.Fatalf("seed raw data: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw badger: %v", err)
	}

	registerMigration(t, testOlderSchema, func(db *badger.DB, key *secret.Value) error {
		if key == nil || key.Size() != 32 {
			return errors.New("migration did not get the storage key")
		}
		return db.Update(func(txn *badger.Txn) error {
			return txn.Set([]byte("raw/migrated"), []byte("yes"))
		})
	})

	st, err := Open(Options{Dir: dir, Key: testKey(t, 30)})
	if err != nil {
		t.Fatalf("Open with a migration: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	version, err := readSchema(dir)
	if err != nil || version != Schema {
		t.Fatalf("schema after the migration = %d, %v, want %d", version, err, Schema)
	}

	backups, err := filepath.Glob(dir + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups after the migration = %v, %v, want one", backups, err)
	}
	if version, err := readSchema(backups[0]); err != nil || version != testOlderSchema {
		t.Fatalf("the backup has schema %d, %v, want %d: it must be the state before the migration", version, err, testOlderSchema)
	}

	raw := rawEntries(t, dir)
	if raw["raw/migrated"] != "yes" {
		t.Fatal("the migration step did not run")
	}
	if raw["raw/old"] != "kept" {
		t.Fatal("the migration lost the old records")
	}
}

// TestMigrationFailureKeepsTheSchema: a step that fails leaves the store at its old number so the
// next start can try again, and the backup is already there.
func TestMigrationFailureKeepsTheSchema(t *testing.T) {
	dir := t.TempDir()
	if err := writeSchema(dir, testOlderSchema); err != nil {
		t.Fatalf("writeSchema: %v", err)
	}
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	registerMigration(t, testOlderSchema, func(*badger.DB, *secret.Value) error {
		return errors.New("step failed")
	})

	if _, err := Open(Options{Dir: dir, Key: testKey(t, 31)}); err == nil {
		t.Fatal("Open succeeded with a failing migration")
	}
	version, err := readSchema(dir)
	if err != nil || version != testOlderSchema {
		t.Fatalf("schema after a failed migration = %d, %v, want %d", version, err, testOlderSchema)
	}
	if backups, _ := filepath.Glob(dir + ".backup-*"); len(backups) != 1 {
		t.Fatalf("backups after a failed migration = %v, want one", backups)
	}
}

// TestSchemaCorrupt pins that a truncated marker is refused rather than guessed.
func TestSchemaCorrupt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, schemaFile), []byte{0, 0}, 0o600); err != nil {
		t.Fatalf("write %s: %v", schemaFile, err)
	}
	if _, err := Open(Options{Dir: dir, Key: testKey(t, 32)}); !errors.Is(err, ErrSchemaCorrupt) {
		t.Fatalf("Open with a two-byte schema marker: %v, want ErrSchemaCorrupt", err)
	}
}
