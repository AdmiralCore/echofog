package store

import "testing"

// TestMobileBadgerOptions pins platforms.md §1.2: under the mem_cap_mb flag badger must stay
// inside a phone's memory budget.
func TestMobileBadgerOptions(t *testing.T) {
	t.Parallel()

	opt := badgerOptions(Options{Dir: "x", MemCapMB: 128})

	if opt.MemTableSize != 4<<20 {
		t.Errorf("MemTableSize = %d, want 4 MiB", opt.MemTableSize)
	}
	if opt.NumMemtables < 1 || opt.NumMemtables > 2 {
		t.Errorf("NumMemtables = %d, want 1-2", opt.NumMemtables)
	}
	if opt.NumMemtables != 2 {
		t.Errorf("NumMemtables = %d with mem_cap_mb 128, want 2", opt.NumMemtables)
	}
	if opt.BlockCacheSize > 8<<20 {
		t.Errorf("BlockCacheSize = %d, want at most 8 MiB", opt.BlockCacheSize)
	}
	if opt.IndexCacheSize > 4<<20 {
		t.Errorf("IndexCacheSize = %d, want at most 4 MiB", opt.IndexCacheSize)
	}
	if opt.ValueLogFileSize != 16<<20 {
		t.Errorf("ValueLogFileSize = %d, want 16 MiB", opt.ValueLogFileSize)
	}
	// badger refuses exactly one compactor ("Cannot have 1 compactor. Need at least 2"), so the
	// smallest supported setting is the spec's one worker approximated: two.
	if opt.NumCompactors != 2 {
		t.Errorf("NumCompactors = %d, want 2 (badger's minimum)", opt.NumCompactors)
	}
	// badger rejects a ValueThreshold above 15 %% of a memtable; 4 MiB memtables need a small one.
	if opt.ValueThreshold > opt.MemTableSize*15/100 {
		t.Errorf("ValueThreshold = %d is above badger's batch limit for a 4 MiB memtable", opt.ValueThreshold)
	}

	small := badgerOptions(Options{Dir: "x", MemCapMB: 40})
	if small.NumMemtables != 1 {
		t.Errorf("NumMemtables = %d with mem_cap_mb 40, want 1", small.NumMemtables)
	}
	if small.MemTableSize != 4<<20 {
		t.Errorf("MemTableSize = %d with mem_cap_mb 40, want 4 MiB", small.MemTableSize)
	}
}

func TestDesktopBadgerOptions(t *testing.T) {
	t.Parallel()

	opt := badgerOptions(Options{Dir: "x"})
	if opt.MemTableSize != 8<<20 {
		t.Errorf("MemTableSize = %d, want 8 MiB (v1 pkg/node)", opt.MemTableSize)
	}
	if opt.BaseTableSize != 8<<20 {
		t.Errorf("BaseTableSize = %d, want 8 MiB", opt.BaseTableSize)
	}
	if opt.ValueLogFileSize != 16<<20 {
		t.Errorf("ValueLogFileSize = %d, want 16 MiB", opt.ValueLogFileSize)
	}
	if opt.ValueThreshold != 64<<10 {
		t.Errorf("ValueThreshold = %d, want 64 KiB", opt.ValueThreshold)
	}
}

// TestMobileOptionsActuallyOpen proves badger accepts the values: the compactor minimum and the
// ValueThreshold/memtable limit are checked by badger itself at Open.
func TestMobileOptionsActuallyOpen(t *testing.T) {
	t.Parallel()

	st := openStore(t, t.TempDir(), testKey(20), 40)
	if err := st.Store("test").Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
}
