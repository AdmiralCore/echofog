package chatcrypto

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// scanAll bounds a "whole prefix" scan of the adapter: one namespace never holds more prekeys than
// MaxJITOPKs plus a handful of rotation records.
const scanAll = 10000

// Records the v1 session manager writes forever: how long the v2 store keeps them as a
// safety net (AD19: never earlier than the logical end, which is the retention of the keys they
// guard, 35 days after the signed prekey is replaced, plus a day).
const (
	initSeenTTL = 50 * 24 * time.Hour
)

// kvStorage lets the v1 key manager and session manager (storage.Storage) work on a namespace of
// the v2 store (store.KV). It also blinds peer identifiers in keys (storage.Blinder), so that the
// database shows no peer (security.md 4.8).
type kvStorage struct {
	kv  store.KV
	ids *store.IDs
}

var (
	_ storage.Storage = (*kvStorage)(nil)
	_ storage.Blinder = (*kvStorage)(nil)
)

func (s *kvStorage) Set(key, value []byte) error {
	if strings.HasPrefix(string(key), "x3dh-init:") || strings.HasPrefix(string(key), "opkused:") {
		return s.kv.PutTTL(key, value, initSeenTTL)
	}
	return s.kv.Put(key, value)
}

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

// FindValuesSeek lists the records under prefix after start (exclusive); limit 0 means all of them.
func (s *kvStorage) FindValuesSeek(prefix, start []byte, limit int) ([]storage.Entry, error) {
	if limit <= 0 || limit > scanAll {
		limit = scanAll
	}
	var out []storage.Entry
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

// BlindID blinds an identifier of the v1 session manager (a hex string).
func (s *kvStorage) BlindID(id string) string {
	b, err := s.ids.Blind(store.DomainPeer, []byte(id))
	if err != nil {
		// the blinder is closed: the node is shutting down; never fall back to the raw id
		return "closed"
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

var errClosed = errors.New("chat/2: closed")
