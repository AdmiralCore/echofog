package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var runLockPath = flag.String("lock", filepath.Join("testbed", ".testbed.lock"),
	"the run lock the runner holds for the whole run (testbed.lock in the coordinator's locks/ is separate)")

// acquireRunLock takes testbed/.testbed.lock for the whole run. It is the runner's own guard on top
// of the coordinator's locks/testbed.lock: one run per machine, no exceptions.
func acquireRunLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if fi, statErr := os.Stat(path); statErr == nil {
			age := time.Since(fi.ModTime()).Round(time.Minute)
			if age > 3*time.Hour {
				return nil, fmt.Errorf("%s is held for %s (stale? do not delete it silently; ask the architect)", path, age)
			}
			return nil, fmt.Errorf("another run holds %s for %s", path, age)
		}
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "runner %d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	_ = f.Close()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(path)
	}, nil
}

// pruneOut deletes run directories under base older than keepAge. Captures and logs are evidence
// for a week; the small results/*.json files live outside base and are kept.
func pruneOut(base string, keepAge time.Duration, now time.Time) ([]string, error) {
	runs, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := now.Add(-keepAge)
	var removed []string
	for _, r := range runs {
		if !r.IsDir() {
			continue
		}
		at := r.Name()
		if len(at) >= 15 {
			if t, err := time.ParseInLocation("20060102-150405", at[:15], now.Location()); err == nil {
				at = t.Format(time.RFC3339)
				if t.Before(cutoff) {
					if err := os.RemoveAll(filepath.Join(base, r.Name())); err != nil {
						return removed, err
					}
					removed = append(removed, r.Name())
					continue
				}
			}
		}
		info, err := r.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(base, r.Name())); err != nil {
				return removed, err
			}
			removed = append(removed, r.Name())
		}
	}
	return removed, nil
}
