package store

import "github.com/dgraph-io/badger/v4"

const (
	// desktopMemTableSize and desktopBaseTableSize match the v1 options in pkg/node/node.go.
	desktopMemTableSize   = 8 << 20
	desktopBaseTableSize  = 8 << 20
	desktopValueThreshold = 64 << 10

	// valueLogFileSize is 16 MiB on both profiles (platforms.md §1.2).
	valueLogFileSize = 16 << 20

	// mobileMemTableSize and mobileBaseTableSize are the phone values (platforms.md §1.2).
	mobileMemTableSize   = 4 << 20
	mobileBaseTableSize  = 4 << 20
	mobileBlockCacheSize = 8 << 20
	mobileIndexCacheSize = 4 << 20
	// mobileValueThreshold stays far below 15 % of a 4 MiB memtable: badger refuses to open when
	// a value may not fit in its write batch.
	mobileValueThreshold = 32 << 10

	// mobileMemtablesHigh is the mem_cap_mb at which a second memtable fits next to the caches
	// and the Go heap. iOS extensions get 40 MiB and keep one.
	mobileMemtablesHigh = 64

	// badgerMinCompactors is the smallest number of compaction workers badger opens with:
	// checkAndSetOptions returns "Cannot have 1 compactor. Need at least 2". platforms.md §1.2
	// asks for 1; 2 is the closest the engine allows.
	badgerMinCompactors = 2
)

// badgerOptions are the database options for the platform. A positive mem_cap_mb selects the
// mobile set of platforms.md §1.2; everything else keeps the v1 desktop values.
func badgerOptions(opts Options) badger.Options {
	o := badger.DefaultOptions(opts.Dir)
	o.Logger = nil
	o.ValueLogFileSize = valueLogFileSize
	o.MemTableSize = desktopMemTableSize
	o.BaseTableSize = desktopBaseTableSize
	o.ValueThreshold = desktopValueThreshold

	if opts.MemCapMB > 0 {
		o.MemTableSize = mobileMemTableSize
		o.BaseTableSize = mobileBaseTableSize
		o.ValueThreshold = mobileValueThreshold
		o.NumMemtables = mobileMemtables(opts.MemCapMB)
		o.BlockCacheSize = mobileBlockCacheSize
		o.IndexCacheSize = mobileIndexCacheSize
		o.NumCompactors = badgerMinCompactors
	}
	return o
}

// mobileMemtables is 1 or 2 as platforms.md §1.2 allows, chosen so that the memtables and caches
// leave the rest of mem_cap_mb to the Go heap.
func mobileMemtables(memCapMB int) int {
	if memCapMB >= mobileMemtablesHigh {
		return 2
	}
	return 1
}
