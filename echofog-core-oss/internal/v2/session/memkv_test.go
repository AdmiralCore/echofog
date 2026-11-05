package session_test

import (
	"bytes"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// memKV is an in-memory store.KV for tests that do not need encryption or a disk.
type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (k *memKV) Get(key []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[string(key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (k *memKV) Put(key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[string(key)] = append([]byte(nil), value...)
	return nil
}

func (k *memKV) PutTTL(key, value []byte, _ time.Duration) error { return k.Put(key, value) }

func (k *memKV) Delete(key []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, string(key))
	return nil
}

func (k *memKV) Has(key []byte) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.m[string(key)]
	return ok, nil
}

func (k *memKV) Scan(prefix []byte, limit int) ([]store.Entry, error) {
	return k.scan(prefix, nil, limit)
}

func (k *memKV) ScanFrom(prefix, start []byte, limit int) ([]store.Entry, error) {
	return k.scan(prefix, start, limit)
}

func (k *memKV) scan(prefix, start []byte, limit int) ([]store.Entry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := make([]string, 0, len(k.m))
	for key := range k.m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []store.Entry
	for _, key := range keys {
		b := []byte(key)
		if !bytes.HasPrefix(b, prefix) {
			continue
		}
		if start != nil && bytes.Compare(b, start) <= 0 {
			continue
		}
		out = append(out, store.Entry{Key: append([]byte(nil), b...), Value: append([]byte(nil), k.m[key]...)})
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}
