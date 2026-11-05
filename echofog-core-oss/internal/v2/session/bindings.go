package session

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// The session layer keeps three things in the store (AD13): hs↔device bindings (handshake.md
// §7, S15), the card book (objects.md §3.2, §3.6) and the abuse counter. Each uses its own
// namespace so that a caller can pass store.Store(ns):
//
//	v2/session/bindings  KeyBindings
//	v2/session/cards     CardBook
//	v2/session/abuse     AbuseLog
const (
	BindingsNamespace = "v2/session/bindings"
	CardsNamespace    = "v2/session/cards"
	AbuseNamespace    = "v2/session/abuse"
)

// abuseRecord is the per-source abuse counter.
func abuseRecord(source [32]byte) []byte { return append([]byte{'a'}, source[:]...) }

// AbuseLog counts protocol abuse by the authenticated source that caused it. The count
// survives restarts; a node can use it for admission policy (T3.5). It is written only for
// authenticated misbehaviour, never for unknown frames (versioning.md §2 rule 2).
type AbuseLog struct {
	kv store.KV
	// mu serialises the read-modify-write of one counter; the KV itself is safe for
	// concurrent use.
	mu sync.Mutex
}

// NewAbuseLog opens the abuse counter over a store namespace. kv is required: abuse that is not
// remembered is not accounting.
func NewAbuseLog(kv store.KV) (*AbuseLog, error) {
	if kv == nil {
		return nil, fmt.Errorf("%w: no KV for abuse accounting", ErrConfig)
	}
	return &AbuseLog{kv: kv}, nil
}

// Report adds one to the counter of source. source is the authenticated device ID whose card
// or frame caused the finding.
func (a *AbuseLog) Report(source [32]byte) error {
	if a == nil || a.kv == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	key := abuseRecord(source)
	n := uint64(0)
	switch b, err := a.kv.Get(key); {
	case err == nil:
		if len(b) != 8 {
			return fmt.Errorf("session: damaged abuse counter: %d bytes", len(b))
		}
		n = binary.LittleEndian.Uint64(b)
	case errors.Is(err, store.ErrNotFound):
	default:
		return err
	}
	n++
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, n)
	return a.kv.Put(key, out)
}

// AbuseCount is one persisted counter for the operator view (T0.3 `efv2 status`).
type AbuseCount struct {
	Source [32]byte
	Count  uint64
}

// Snapshot returns every stored abuse counter, sorted by source device ID. A damaged record is
// skipped: a status view must stay available even when one counter needs repair, while Count
// keeps reporting the damage for the code that acts on it.
func (a *AbuseLog) Snapshot() ([]AbuseCount, error) {
	if a == nil || a.kv == nil {
		return nil, nil
	}
	entries, err := a.kv.Scan([]byte{'a'}, store.MaxScanLimit)
	if err != nil {
		return nil, err
	}
	out := make([]AbuseCount, 0, len(entries))
	for _, e := range entries {
		if len(e.Key) != 1+32 || len(e.Value) != 8 {
			continue
		}
		var c AbuseCount
		copy(c.Source[:], e.Key[1:])
		c.Count = binary.LittleEndian.Uint64(e.Value)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].Source[:], out[j].Source[:]) < 0
	})
	return out, nil
}

// Count returns the number of findings recorded against source.
func (a *AbuseLog) Count(source [32]byte) (uint64, error) {
	if a == nil || a.kv == nil {
		return 0, nil
	}
	b, err := a.kv.Get(abuseRecord(source))
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("session: damaged abuse counter: %d bytes", len(b))
	}
	return binary.LittleEndian.Uint64(b), nil
}

// bindingRecord is the store key of one hs key: a type byte and the 32-byte key.
func bindingRecord(hsKey []byte) []byte {
	out := make([]byte, 1+len(hsKey))
	out[0] = 'h'
	copy(out[1:], hsKey)
	return out
}

// KeyBindings remembers which device ID each hs key belongs to (handshake.md §7). It implements
// handshake.KeyBinder and lives in the store instead of process memory: a restart must not
// forget that an hs key was already seen, or the S15 check is lost.
type KeyBindings struct {
	kv    store.KV
	abuse *AbuseLog
	// mu makes the check-then-write atomic in this process; the store has no compare-and-swap.
	mu sync.Mutex
}

// NewKeyBindings opens the binding table over a store namespace. kv is required.
func NewKeyBindings(kv store.KV, abuse *AbuseLog) (*KeyBindings, error) {
	if kv == nil {
		return nil, fmt.Errorf("%w: no KV for hs bindings", ErrConfig)
	}
	return &KeyBindings{kv: kv, abuse: abuse}, nil
}

// Bind records hsKey → deviceID, or fails with handshake.ErrKeyBinding when the key is already
// bound to another device. A conflict is abuse against the device that supplied the card.
func (b *KeyBindings) Bind(hsKey []byte, deviceID [32]byte) error {
	if b == nil || b.kv == nil {
		return fmt.Errorf("%w: no binding table", ErrConfig)
	}
	if len(hsKey) != 32 {
		return handshake.ErrKeyBinding
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	key := bindingRecord(hsKey)
	cur, err := b.kv.Get(key)
	switch {
	case err == nil:
		if len(cur) != 32 {
			return fmt.Errorf("session: damaged hs binding: %d bytes: %w", len(cur), handshake.ErrKeyBinding)
		}
		if !bytes.Equal(cur, deviceID[:]) {
			if b.abuse != nil {
				_ = b.abuse.Report(deviceID)
			}
			return handshake.ErrKeyBinding
		}
		return nil
	case errors.Is(err, store.ErrNotFound):
	default:
		return err
	}
	return b.kv.Put(key, deviceID[:])
}

// Lookup returns the device ID bound to hsKey.
func (b *KeyBindings) Lookup(hsKey []byte) ([32]byte, bool, error) {
	if b == nil || b.kv == nil {
		return [32]byte{}, false, nil
	}
	if len(hsKey) != 32 {
		return [32]byte{}, false, nil
	}
	cur, err := b.kv.Get(bindingRecord(hsKey))
	if errors.Is(err, store.ErrNotFound) {
		return [32]byte{}, false, nil
	}
	if err != nil {
		return [32]byte{}, false, err
	}
	if len(cur) != 32 {
		return [32]byte{}, false, fmt.Errorf("session: damaged hs binding: %d bytes", len(cur))
	}
	var id [32]byte
	copy(id[:], cur)
	return id, true, nil
}
