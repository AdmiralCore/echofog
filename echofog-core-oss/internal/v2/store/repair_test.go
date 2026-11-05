package store

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// tablesOf lists the table files of a store, oldest first (badger names them by number).
func tablesOf(t *testing.T, dir string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "*.sst"))
	if err != nil {
		t.Fatalf("glob tables: %v", err)
	}
	sort.Strings(matches)
	return matches
}

func backupsOf(t *testing.T, dir string) []string {
	t.Helper()

	matches, err := filepath.Glob(dir + ".backup-*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}

// TestRepairUnwrittenTable follows v1 storage/repair_test.go: a table a killed process left as
// zeros made badger panic on open. The store removes it, keeps a copy and opens with what is left.
func TestRepairUnwrittenTable(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"first", "second"} {
		st, err := Open(Options{Dir: dir, Key: testKey(21), MemCapMB: 40})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := st.Store("test").Put([]byte(v), []byte("payload "+v)); err != nil {
			t.Fatalf("Put(%s): %v", v, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	tables := tablesOf(t, dir)
	if len(tables) == 0 {
		t.Fatal("no table was written")
	}
	last := tables[len(tables)-1]
	info, err := os.Stat(last)
	if err != nil {
		t.Fatalf("stat %s: %v", last, err)
	}
	if err := os.WriteFile(last, make([]byte, info.Size()), 0o600); err != nil {
		t.Fatalf("zero %s: %v", last, err)
	}

	st, err := Open(Options{Dir: dir, Key: testKey(21), MemCapMB: 40})
	if err != nil {
		t.Fatalf("Open after the damage: %v", err)
	}
	defer st.Close()

	got, err := st.Store("test").Get([]byte("first"))
	if err != nil || string(got) != "payload first" {
		t.Fatalf("Get(first) = %q, %v: the older table must survive", got, err)
	}
	if backups := backupsOf(t, dir); len(backups) == 0 {
		t.Fatal("the repair kept no copy of the database")
	}
}

// TestRepairBrokenTable follows v1 storage/salvage_test.go: only the index and footer of a table
// never reached the disk. The store rebuilds the table from its blocks, so every record survives.
func TestRepairBrokenTable(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(Options{Dir: dir, Key: testKey(22), MemCapMB: 40})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	kv := st.Store("test")
	const n = 300
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key/%05d", i))
		value := []byte(strings.Repeat("v", 2048) + fmt.Sprintf("-%05d", i))
		if err := kv.Put(key, value); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tables := tablesOf(t, dir)
	if len(tables) == 0 {
		t.Fatal("no table was written")
	}
	// The table with the data is the largest one.
	var table string
	var size int64
	for _, name := range tables {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() > size {
			table, size = name, info.Size()
		}
	}

	data, err := os.ReadFile(table)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	cl := int(binary.BigEndian.Uint32(data[len(data)-4:]))
	if cl <= 0 || cl > len(data)-8 {
		t.Fatalf("%s: bad checksum length %d", table, cl)
	}
	il := int(binary.BigEndian.Uint32(data[len(data)-8-cl:]))
	blocksEnd := len(data) - 8 - cl - il
	if blocksEnd <= 0 || blocksEnd >= len(data) {
		t.Fatalf("%s: cannot find the index (%d bytes)", table, blocksEnd)
	}
	for i := blocksEnd; i < len(data); i++ {
		data[i] = 0
	}
	if err := os.WriteFile(table, data, 0o600); err != nil {
		t.Fatalf("damage %s: %v", table, err)
	}

	st, err = Open(Options{Dir: dir, Key: testKey(22), MemCapMB: 40})
	if err != nil {
		t.Fatalf("Open after the damage: %v", err)
	}
	defer st.Close()

	kv = st.Store("test")
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key/%05d", i))
		want := strings.Repeat("v", 2048) + fmt.Sprintf("-%05d", i)
		got, err := kv.Get(key)
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("Get(%d) = %q, want %q", i, got, want)
		}
	}
	if backups := backupsOf(t, dir); len(backups) == 0 {
		t.Fatal("the repair kept no copy of the database")
	}

	// A sound database is not repaired again.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st, err = Open(Options{Dir: dir, Key: testKey(22), MemCapMB: 40})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	if got := len(backupsOf(t, dir)); got != 1 {
		t.Fatalf("reopening a repaired store made %d backups, want 1", got)
	}
}

// TestRepairLeavesASoundStoreAlone keeps the repair path honest: opening and closing a store
// several times never rewrites a table.
func TestRepairLeavesASoundStoreAlone(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		st, err := Open(Options{Dir: dir, Key: testKey(23)})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := st.Store("test").Put([]byte(fmt.Sprintf("k%d", i)), []byte("v")); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if backups := backupsOf(t, dir); len(backups) != 0 {
		t.Fatalf("a sound store was copied: %v", backups)
	}
}
