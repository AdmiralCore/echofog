package dht

import (
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// The routing table is stored as one record per entry, keyed by device ID and holding the exact
// signed card plus the local confirmation time. A restart reads the same list of cards the spec
// describes (dht.md §5, C12) and re-verifies every one of them.
const (
	entryPrefix byte = 'e' // 'e' ‖ device ID(32)
	entryFormat byte = 1
	entryHeader      = 1 + 8 // format ‖ last confirmed unixnano
)

func entryKey(id DeviceID) []byte {
	key := make([]byte, 1+len(id))
	key[0] = entryPrefix
	copy(key[1:], id[:])
	return key
}

func encodeEntry(e entryState) []byte {
	raw := e.card.Env.Raw
	out := make([]byte, entryHeader+len(raw))
	out[0] = entryFormat
	if !e.confirmed.IsZero() {
		binary.BigEndian.PutUint64(out[1:], uint64(e.confirmed.UnixNano()))
	}
	copy(out[entryHeader:], raw)
	return out
}

// decodeEntry returns the card bytes and the confirmation time stored with them.
func decodeEntry(value []byte) ([]byte, time.Time, bool) {
	if len(value) < entryHeader || value[0] != entryFormat {
		return nil, time.Time{}, false
	}
	ns := int64(binary.BigEndian.Uint64(value[1:entryHeader]))
	var at time.Time
	if ns != 0 {
		at = time.Unix(0, ns)
	}
	return value[entryHeader:], at, true
}

// persist writes one card and its confirmation time. The caller must hold writeMu (through
// beginWriteLocked); a storage failure is logged but never breaks the session that produced the
// card.
func (t *Table) persist(e entryState) {
	if t.kv == nil || e.card == nil || e.card.Env == nil {
		return
	}
	if err := t.kv.Put(entryKey(e.device), encodeEntry(e)); err != nil {
		t.log.Warn("dht: storing a routing entry", "device", e.device, "err", err)
	}
}

// deleteEntry removes one record. The caller must hold writeMu (through beginWriteLocked).
func (t *Table) deleteEntry(id DeviceID) {
	if t.kv == nil {
		return
	}
	if err := t.kv.Delete(entryKey(id)); err != nil {
		t.log.Warn("dht: deleting a routing entry", "device", id, "err", err)
	}
}

// Save writes every in-memory entry. The table also writes each accepted card immediately, so a
// crash does not lose the last five minutes; the periodic save of dht.md §5 refreshes the whole
// list and keeps the on-disk confirmation times current.
func (t *Table) Save() error {
	if t.kv == nil {
		return nil
	}
	t.mu.Lock()
	entries := make([]entryState, 0, t.total)
	for _, b := range t.buckets {
		for _, e := range b.entries {
			entries = append(entries, e.state())
		}
	}
	t.beginWriteLocked()
	defer t.endWrite()

	var firstErr error
	for _, e := range entries {
		if err := t.kv.Put(entryKey(e.device), encodeEntry(e)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// load replays the store through the normal verification rules. Cards are restored newest first
// so the in-memory cap keeps the most recently confirmed ones and the rest stay on disk.
func (t *Table) load() error {
	records, err := t.scanEntries()
	if err != nil {
		return err
	}

	type restored struct {
		id        DeviceID
		card      *objects.Card
		confirmed time.Time
	}
	now := t.now()
	var cards []restored
	for _, rec := range records {
		if len(rec.Key) != 1+32 || rec.Key[0] != entryPrefix {
			continue
		}
		var id DeviceID
		copy(id[:], rec.Key[1:])
		raw, at, ok := decodeEntry(rec.Value)
		if !ok {
			continue
		}
		card, err := objects.VerifyCard(raw, now)
		if err != nil {
			continue
		}
		if DeviceID(card.DeviceID) != id || id == t.self {
			continue
		}
		if card.Body.GetFlags()&objects.FlagUnlisted != 0 {
			continue
		}
		if at.IsZero() {
			at = now
		}
		cards = append(cards, restored{id: id, card: card, confirmed: at})
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if !cards[i].confirmed.Equal(cards[j].confirmed) {
			return cards[i].confirmed.After(cards[j].confirmed)
		}
		return cards[i].id.String() < cards[j].id.String()
	})

	for _, r := range cards {
		cand := newEntry(r.card, r.confirmed)
		if !t.fitsLocked(cand, nil) {
			continue
		}
		b := t.buckets[t.bucketOf(r.id)]
		if len(b.entries) < K && (t.memLimit == 0 || t.total < t.memLimit) {
			t.placeHotLocked(b, cand)
			continue
		}
		// The card stays on disk but its seq, endpoints and diversity still constrain new
		// entries: eviction (or the in-memory cap at start) must not bypass the rules.
		t.placeColdLocked(cand)
	}
	return nil
}

func (t *Table) scanEntries() ([]store.Entry, error) {
	var out []store.Entry
	var start []byte
	prefix := []byte{entryPrefix}
	for {
		entries, err := t.kv.ScanFrom(prefix, start, store.MaxScanLimit)
		if err != nil {
			return nil, fmt.Errorf("dht: reading the routing table: %w", err)
		}
		out = append(out, entries...)
		if len(entries) < store.MaxScanLimit {
			return out, nil
		}
		start = entries[len(entries)-1].Key
	}
}
