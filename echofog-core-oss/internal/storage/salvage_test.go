package storage

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

func salvageOpts(dir string) badger.Options {
	opts := badger.DefaultOptions(dir)
	opts.Logger = nil
	opts.ValueLogFileSize = 16 * 1024 * 1024
	opts.ValueThreshold = 64 * 1024
	opts.BaseTableSize = 8 * 1024 * 1024
	opts.MemTableSize = 8 * 1024 * 1024
	return opts
}

// A table whose index never reached the disk (its last pages read as zeros) made badger panic on
// open. After the repair the database opens and holds what the table's blocks held.
func TestRepairBrokenTablesRebuildsFromBlocks(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(salvageOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	const n = 2000
	if err := db.Update(func(txn *badger.Txn) error {
		for i := 0; i < n; i++ {
			if err := txn.Set([]byte(fmt.Sprintf("key/%05d", i)), []byte(strings.Repeat("v", 40)+fmt.Sprint(i))); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil { // the memtable goes to a table
		t.Fatal(err)
	}

	// the table's end, past its last block, reads as zeros: index, checksum and lengths
	var sst string
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".sst") {
			sst = filepath.Join(dir, f.Name())
		}
	}
	if sst == "" {
		t.Fatal("no table was written")
	}
	data, err := os.ReadFile(sst)
	if err != nil {
		t.Fatal(err)
	}
	cl := int(binary.BigEndian.Uint32(data[len(data)-4:]))
	il := int(binary.BigEndian.Uint32(data[len(data)-8-cl:]))
	blocksEnd := len(data) - 8 - cl - il
	for i := blocksEnd; i < len(data); i++ {
		data[i] = 0
	}
	if err := os.WriteFile(sst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if indexReadable(data) {
		t.Fatal("the test needs a table badger cannot read")
	}

	ids, backup, err := RepairBrokenTables(dir, SalvageOptions{BaseTableSize: 8 << 20, BlockSize: 4096, Compression: options.Snappy, BloomFalsePos: 0.01})
	if err != nil || len(ids) != 1 || backup == "" {
		t.Fatalf("repair: %v %q %v", ids, backup, err)
	}
	db, err = badger.Open(salvageOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := 0
	if err := db.View(func(txn *badger.Txn) error {
		for i := 0; i < n; i++ {
			item, err := txn.Get([]byte(fmt.Sprintf("key/%05d", i)))
			if err != nil {
				return fmt.Errorf("key %d: %w", i, err)
			}
			v, _ := item.ValueCopy(nil)
			if string(v) != strings.Repeat("v", 40)+fmt.Sprint(i) {
				return fmt.Errorf("key %d: %q", i, v)
			}
			got++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := RepairBrokenTables(dir, SalvageOptions{BaseTableSize: 8 << 20, BlockSize: 4096, Compression: options.Snappy, BloomFalsePos: 0.01}); len(again) != 0 {
		t.Fatalf("a sound database was repaired again: %v", again)
	}
}
