package dht

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// memoryKeep is how long the store keeps a reader-memory entry: longer than any record lifetime
// (400 days for a release manifest, registries.md §9) plus a day, so the store TTL is only a
// guard against garbage and never ends the memory of a record that can still be replayed (AD19).
const memoryKeep = 401 * 24 * time.Hour

const memoryPrefix byte = 'm' // 'm' ‖ signer ID(32) ‖ label

// Memory is the reader memory of dht.md §4.2: the highest seq ever accepted per (signer, label).
// It is persistent (AD13); a reader never accepts a lower seq than it holds, so a rollback can only
// affect the first lookup of a record.
type Memory struct {
	mu sync.Mutex
	kv store.KV
	// local is used when no store is given (tests, a node that has not opened its profile).
	local map[string]uint64
}

// NewMemory opens the reader memory over a store namespace; nil keeps it in memory only.
func NewMemory(kv store.KV) *Memory {
	return &Memory{kv: kv, local: map[string]uint64{}}
}

func memoryKey(label string, signer [32]byte) []byte {
	k := make([]byte, 0, 1+32+len(label))
	k = append(k, memoryPrefix)
	k = append(k, signer[:]...)
	return append(k, label...)
}

// Seq returns the highest seq accepted for (label, signer).
func (m *Memory) Seq(label string, signer [32]byte) (uint64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked(memoryKey(label, signer))
}

func (m *Memory) loadLocked(key []byte) (uint64, bool, error) {
	if m.kv == nil {
		v, ok := m.local[string(key)]
		return v, ok, nil
	}
	b, err := m.kv.Get(key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("dht: reader memory: %w", err)
	case len(b) != 8:
		return 0, false, fmt.Errorf("dht: reader memory: record of %d bytes", len(b))
	}
	return binary.BigEndian.Uint64(b), true, nil
}

// Accept records that a record with seq was accepted, or returns ErrRollback when a higher seq was
// accepted before. The check and the write are one step. A storage failure is an error, not a
// pass: without the memory the guarantee would silently be gone.
func (m *Memory) Accept(label string, signer [32]byte, seq uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryKey(label, signer)
	cur, ok, err := m.loadLocked(key)
	if err != nil {
		return err
	}
	if ok && seq < cur {
		return ErrRollback
	}
	if ok && seq == cur {
		return nil
	}
	if m.kv == nil {
		m.local[string(key)] = seq
		return nil
	}
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], seq)
	if err := m.kv.PutTTL(key, v[:], memoryKeep); err != nil {
		return fmt.Errorf("dht: reader memory: %w", err)
	}
	return nil
}
