package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
)

// loadBootstrapFile reads and parses a bootstrap list (T5.5). The build carries no addresses of
// its own: the list is the file the user names.
func loadBootstrapFile(path string, now time.Time) ([]bootstrap.Entry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap list: %w", err)
	}
	if info.Size() > bootstrap.MaxListBytes {
		return nil, fmt.Errorf("bootstrap list: larger than %d bytes", bootstrap.MaxListBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap list: %w", err)
	}
	entries, err := bootstrap.Parse(string(raw), now)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("bootstrap list: no entries")
	}
	return entries, nil
}

// runBootstrap dials the list once through the running node and logs the outcome. It never logs
// addresses or cards.
func runBootstrap(ctx context.Context, a *app, entries []bootstrap.Entry, log *slog.Logger) bootstrap.Report {
	rep := bootstrap.New(a.rt, entries, a.clock.Now).Run(ctx)
	log.Info("bootstrap finished", "entries", len(entries), "joined", rep.Joined,
		"candidates", len(rep.Candidates), "failed", rep.Failed)
	return rep
}
