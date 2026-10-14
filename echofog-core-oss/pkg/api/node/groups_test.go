package node

import (
	"sort"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/internal/storage"
)

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemStore() *memStore { return &memStore{m: map[string][]byte{}} }

func (s *memStore) Set(k, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(k)] = append([]byte(nil), v...)
	return nil
}

func (s *memStore) Get(k []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[string(k)], nil
}

func (s *memStore) Delete(k []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(k))
	return nil
}

func (s *memStore) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	return s.FindValuesSeek(prefix, nil, limit)
}

func (s *memStore) FindValuesSeek(prefix, start []byte, limit int) ([]storage.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0)
	for k := range s.m {
		if strings.HasPrefix(k, string(prefix)) && k >= string(start) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]storage.Entry, 0)
	for _, k := range keys {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, storage.Entry{Key: []byte(k), Value: s.m[k]})
	}
	return out, nil
}

func (s *memStore) Exists(k []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[string(k)]
	return ok
}

func (s *memStore) Close() {}
