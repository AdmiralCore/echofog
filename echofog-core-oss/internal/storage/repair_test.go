package storage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A database whose newest table never reached the disk opens again after the repair, with what
// was written before, and the original is kept.
func TestRepairUnwrittenTable(t *testing.T) {
	dir := os.Getenv("BROKEN_DB") // optionally a real damaged database (a copy)
	if dir == "" {
		dir = t.TempDir()
		// two sessions: each close writes its own table
		for _, k := range []string{"system:identity:root", "m:recent"} {
			db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
			require.NoError(t, err)
			require.NoError(t, db.Update(func(txn *badger.Txn) error { return txn.Set([]byte(k), []byte{1}) }))
			require.NoError(t, db.Close())
		}
		// the newest table is the one a killed process left as zeros
		ssts, _ := filepath.Glob(filepath.Join(dir, "*.sst"))
		require.NotEmpty(t, ssts)
		last := ssts[len(ssts)-1]
		st, _ := os.Stat(last)
		require.NoError(t, os.WriteFile(last, make([]byte, st.Size()), 0o600))
	}

	ids, backup, err := storage.RepairUnwrittenTables(dir)
	require.NoError(t, err)
	require.NotEmpty(t, ids)
	assert.DirExists(t, backup)
	t.Logf("removed %v, copy in %s", ids, backup)

	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.View(func(txn *badger.Txn) error {
		_, err := txn.Get([]byte("system:identity:root"))
		return err
	}), "the identity survives")

	again, _, err := storage.RepairUnwrittenTables(dir)
	require.NoError(t, err)
	assert.Empty(t, again)
}
