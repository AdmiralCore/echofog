package store

import (
	"fmt"
	"log/slog"

	"github.com/dgraph-io/badger/v4"
	v1storage "github.com/echofog/echofog-core/internal/storage"
)

// repairTables finds the tables a crash left half-written and repairs them before badger opens
// them. Both v1 repairs take the table size as a parameter (platforms.md §1.2), so the same tools
// serve the mobile options: the table was written at mobileBaseTableSize.
func repairTables(dir string, opts badger.Options, log *slog.Logger) error {
	ids, backup, err := v1storage.RepairUnwrittenTables(dir)
	if err != nil {
		return repairError("unwritten tables", err, backup)
	}
	if len(ids) > 0 && log != nil {
		log.Warn("store: removed tables a killed process left unwritten", "tables", ids, "backup", backup)
	}

	ids, backup, err = v1storage.RepairBrokenTables(dir, v1storage.SalvageOptions{
		BaseTableSize: opts.BaseTableSize,
		BlockSize:     opts.BlockSize,
		Compression:   opts.Compression,
		BloomFalsePos: opts.BloomFalsePositive,
	})
	if err != nil {
		return repairError("broken tables", err, backup)
	}
	if len(ids) > 0 && log != nil {
		log.Warn("store: rebuilt tables whose index was lost", "tables", ids, "backup", backup)
	}
	return nil
}

func repairError(what string, err error, backup string) error {
	if backup != "" {
		return fmt.Errorf("store: repairing %s: %w; a copy is in %s", what, err, backup)
	}
	return fmt.Errorf("store: repairing %s: %w", what, err)
}
