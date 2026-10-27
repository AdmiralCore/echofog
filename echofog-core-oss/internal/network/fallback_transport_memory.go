package network

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// transportMemoryDebounce coalesces a burst of record updates into one disk write.
const transportMemoryDebounce = 2 * time.Second

// envMemoryDTO is the on-disk form of one envMem entry (envRecord's fields are unexported).
type envMemoryDTO struct {
	Fingerprint string    `json:"fp"`
	TCP         bool      `json:"tcp"`
	At          time.Time `json:"at"`
}

// SetEnvMemoryPath makes the per-network transport memory survive restarts by storing it at path.
// It loads any saved memory now — at node init, never on the dial path — drops entries older than
// envMemoryTTL, and starts a background writer that coalesces updates off the dial path. Call it
// once, before dialing; an empty path leaves the memory in-process only (the previous behaviour).
func (f *FallbackTransport) SetEnvMemoryPath(path string) {
	if path == "" {
		return
	}
	f.envMu.Lock()
	f.envPath = path
	if f.envSaveCh == nil {
		f.envSaveCh = make(chan struct{}, 1)
	}
	f.envMu.Unlock()
	f.loadEnvMemory()
	go f.envSaveLoop()
}

// loadEnvMemory reads the saved memory, keeping only entries still within envMemoryTTL (so a long
// downtime does not resurrect a stale "prefer TCP" bias). Best-effort: any error leaves the memory
// empty, i.e. the safe QUIC-first default.
func (f *FallbackTransport) loadEnvMemory() {
	data, err := os.ReadFile(f.envPath)
	if err != nil {
		return
	}
	var saved []envMemoryDTO
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	now := time.Now()
	f.envMu.Lock()
	defer f.envMu.Unlock()
	if f.envMem == nil {
		f.envMem = make(map[string]envRecord)
	}
	for _, e := range saved {
		if len(f.envMem) >= 64 {
			break
		}
		if e.Fingerprint == "" || now.Sub(e.At) > envMemoryTTL {
			continue
		}
		f.envMem[e.Fingerprint] = envRecord{tcp: e.TCP, at: e.At}
	}
}

// signalEnvSave asks the background writer to persist the memory, without blocking the caller (so
// it is safe to call from recordEnv). A no-op when persistence is not enabled.
func (f *FallbackTransport) signalEnvSave() {
	f.envMu.Lock()
	ch := f.envSaveCh
	f.envMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default: // a save is already pending; it will capture this update too
	}
}

// envSaveLoop writes the memory to disk at most once per debounce window, entirely off the dial
// path, until the transport closes.
func (f *FallbackTransport) envSaveLoop() {
	for {
		select {
		case <-f.done:
			return
		case <-f.envSaveCh:
		}
		select {
		case <-f.done:
			f.writeEnvMemory() // flush what we have before shutting down
			return
		case <-time.After(transportMemoryDebounce):
		}
		f.writeEnvMemory()
	}
}

// writeEnvMemory snapshots the memory under the lock, then writes it atomically (temp file +
// rename) outside the lock so disk I/O never stalls a dial recording an update.
func (f *FallbackTransport) writeEnvMemory() {
	f.envMu.Lock()
	path := f.envPath
	now := time.Now()
	snapshot := make([]envMemoryDTO, 0, len(f.envMem))
	for fp, r := range f.envMem {
		if now.Sub(r.at) > envMemoryTTL {
			continue
		}
		snapshot = append(snapshot, envMemoryDTO{Fingerprint: fp, TCP: r.tcp, At: r.at})
	}
	f.envMu.Unlock()
	if path == "" {
		return
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
