package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Before a run, the captures of all but the newest runs go; reports and logs stay everywhere.
func TestPrunePcapsKeepsTheNewestRuns(t *testing.T) {
	base := t.TempDir()
	runs := []string{"20261027-010000-censor", "20261028-010000-censor", "20261029-010000-censor", "20261029-020000-smoke"}
	for _, r := range runs {
		d := filepath.Join(base, r)
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"wan-gw_a.pcap", "sub/all.PCAPNG", "censor-report.txt", "x1.log"} {
			if err := os.WriteFile(filepath.Join(d, f), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	prunePcaps(base, 3) // with the coming run, three keep their captures: the two newest here

	exists := func(p string) bool { _, err := os.Stat(filepath.Join(base, p)); return err == nil }
	for i, r := range runs {
		kept := i >= len(runs)-2
		if got := exists(r + "/wan-gw_a.pcap"); got != kept {
			t.Errorf("%s: capture kept = %v, want %v", r, got, kept)
		}
		if got := exists(r + "/sub/all.PCAPNG"); got != kept {
			t.Errorf("%s: nested capture kept = %v, want %v", r, got, kept)
		}
		if !exists(r+"/censor-report.txt") || !exists(r+"/x1.log") {
			t.Errorf("%s: a report or log went", r)
		}
	}

	prunePcaps(base, 0) // 0: nothing goes
	if !exists(runs[3] + "/wan-gw_a.pcap") {
		t.Error("keep 0 removed a capture")
	}
}
