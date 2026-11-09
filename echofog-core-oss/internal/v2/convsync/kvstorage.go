package convsync

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// scanAll bounds a whole-prefix scan of the adapter.
const scanAll = 1_000_000

// kvStorage lets the v1 conversation log (storage.Storage) work on a namespace of the v2 store, and
// blinds the identifiers in its keys (storage.Blinder, security.md 4.8). The store encrypts every
// value, so the log's sealed prefixes need no second layer.
type kvStorage struct {
	kv  store.KV
	ids *store.IDs
}

var (
	_ storage.Storage = (*kvStorage)(nil)
	_ storage.Blinder = (*kvStorage)(nil)
)

func (s *kvStorage) Set(key, value []byte) error    { return s.kv.Put(key, value) }
func (s *kvStorage) Get(key []byte) ([]byte, error) { return s.kv.Get(key) }
func (s *kvStorage) Delete(key []byte) error        { return s.kv.Delete(key) }
func (s *kvStorage) Close()                         {}

func (s *kvStorage) Exists(key []byte) bool {
	ok, err := s.kv.Has(key)
	return err == nil && ok
}

func (s *kvStorage) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	return s.FindValuesSeek(prefix, nil, limit)
}

// FindValuesSeek lists the records under prefix from start on, start included as the v1 store does
// (the store's own ScanFrom is exclusive); limit 0 means all of them.
func (s *kvStorage) FindValuesSeek(prefix, start []byte, limit int) ([]storage.Entry, error) {
	if limit <= 0 || limit > scanAll {
		limit = scanAll
	}
	var out []storage.Entry
	if start != nil && bytes.HasPrefix(start, prefix) {
		if v, err := s.kv.Get(start); err == nil {
			out = append(out, storage.Entry{Key: append([]byte(nil), start...), Value: v})
		}
	}
	for len(out) < limit {
		n := limit - len(out)
		if n > store.MaxScanLimit {
			n = store.MaxScanLimit
		}
		var (
			page []store.Entry
			err  error
		)
		if start == nil {
			page, err = s.kv.Scan(prefix, n)
		} else {
			page, err = s.kv.ScanFrom(prefix, start, n)
		}
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			out = append(out, storage.Entry{Key: e.Key, Value: e.Value})
		}
		if len(page) < n {
			break
		}
		start = page[len(page)-1].Key
	}
	return out, nil
}

// BlindID blinds an identifier of the log (a conversation or an author).
func (s *kvStorage) BlindID(id string) string {
	b, err := s.ids.Blind(store.DomainPeer, []byte(id))
	if err != nil {
		return "closed" // the blinder is closed: never fall back to the raw id
	}
	return hex.EncodeToString(b[:])
}

func (s *kvStorage) BlindChat(chatID uint64) uint64 {
	b, err := s.ids.Blind(store.DomainChat, binary.BigEndian.AppendUint64(nil, chatID))
	if err != nil {
		return 0
	}
	return binary.BigEndian.Uint64(b[:8])
}
