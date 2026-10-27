package network

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnvMemoryPersistsAcrossRestart records a per-network preference, lets the background writer
// flush it, then opens a fresh transport at the same path and checks the preference survived.
func TestEnvMemoryPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport-memory.json")
	const fp = "eth0:198.51.100.7"

	f1 := &FallbackTransport{done: make(chan struct{})}
	f1.SetEnvMemoryPath(path)
	// Simulate recordEnv(true): TCP won on this network.
	f1.envMu.Lock()
	f1.envMem = map[string]envRecord{fp: {tcp: true, at: time.Now()}}
	f1.envMu.Unlock()
	f1.signalEnvSave()

	// Wait for the debounced writer to produce the file.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport-memory.json was never written")
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(f1.done)

	// "Restart": a fresh transport loads the saved memory.
	f2 := &FallbackTransport{done: make(chan struct{})}
	defer close(f2.done)
	f2.SetEnvMemoryPath(path)
	f2.envMu.Lock()
	r, ok := f2.envMem[fp]
	f2.envMu.Unlock()
	if !ok || !r.tcp {
		t.Fatalf("preference did not survive restart: ok=%v rec=%+v", ok, r)
	}
}

// TestEnvMemoryPrunesStaleOnLoad drops entries older than envMemoryTTL so a long downtime does not
// resurrect a stale "prefer TCP" bias, while fresh entries are kept.
func TestEnvMemoryPrunesStaleOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport-memory.json")
	now := time.Now()
	saved := []envMemoryDTO{
		{Fingerprint: "stale", TCP: true, At: now.Add(-(envMemoryTTL + time.Minute))},
		{Fingerprint: "fresh", TCP: true, At: now.Add(-time.Minute)},
	}
	data, _ := json.Marshal(saved)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	f := &FallbackTransport{done: make(chan struct{})}
	defer close(f.done)
	f.SetEnvMemoryPath(path)
	f.envMu.Lock()
	_, haveStale := f.envMem["stale"]
	_, haveFresh := f.envMem["fresh"]
	f.envMu.Unlock()
	if haveStale {
		t.Error("stale entry should have been pruned on load")
	}
	if !haveFresh {
		t.Error("fresh entry should have been kept on load")
	}
}

// TestEnvMemoryEmptyPathStaysInProcess checks that without a path the memory never touches disk and
// recordEnv's save signal is a no-op (the previous behaviour).
func TestEnvMemoryEmptyPathStaysInProcess(t *testing.T) {
	f := &FallbackTransport{done: make(chan struct{})}
	defer close(f.done)
	f.SetEnvMemoryPath("") // no-op: persistence stays off
	f.signalEnvSave()      // must not panic or block with no store configured
	f.envMu.Lock()
	ch := f.envSaveCh
	f.envMu.Unlock()
	if ch != nil {
		t.Fatal("empty path must not enable the background writer")
	}
}
