package noise3

import (
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"time"
)

// KV is the persistent key-value surface the replay cache needs. It is the same shape as
// store.KV narrowed to three methods, so node.Host.Store("noise3") satisfies it without this
// package importing the store.
type KV interface {
	Has(key []byte) (bool, error)
	Get(key []byte) ([]byte, error)
	PutTTL(key, value []byte, ttl time.Duration) error
}

// Replay cache of wire.md §2.5 step 5: selectors are kept for 900 s. The filter is aging: the
// bloomSet rotates generations on the injected clock, so capacity is reclaimed and the false
// positive rate stays bounded (see budget.go). It is flushed to KV at least every uniform(8,12) s,
// including from the listener sweep timer, so a restart does not reopen the replay window.
const (
	replayTTL      = bloomGens * bloomPeriod
	replayFlushMin = 8 * time.Second
	replayFlushMax = 12 * time.Second
)

type replayCache struct {
	mu        sync.Mutex
	set       *bloomSet
	kv        KV
	nextFlush time.Time
	rnd       *mrand.Rand
	// onError receives persistence errors; nil logs them. A failed write keeps the generation
	// dirty so the next flush retries it and a restart does not reopen the replay window.
	onError func(error)
}

func newReplayCache(kv KV, now time.Time, rnd *mrand.Rand) *replayCache {
	r := &replayCache{set: &bloomSet{lastRotate: now}, kv: kv, rnd: rnd}
	r.nextFlush = now.Add(r.drawFlush())
	if kv != nil {
		r.load(now)
	}
	return r
}

// load restores the filter; any missing or damaged part leaves an empty filter.
func (r *replayCache) load(now time.Time) {
	kv := r.kv
	if ok, err := kv.Has([]byte(bloomKey)); err != nil || !ok {
		return
	}
	meta, err := kv.Get([]byte(bloomKey))
	if err != nil {
		return
	}
	var loaded bloomSet
	if err := loaded.metaUnmarshal(meta); err != nil {
		return
	}
	for i := range loaded.gens {
		if ok, err := kv.Has(genKey(i)); err != nil || !ok {
			continue
		}
		data, err := kv.Get(genKey(i))
		if err != nil || loaded.gens[i].unmarshal(data) != nil {
			loaded.gens[i].clear()
		}
	}
	*r.set = loaded
	r.set.rotate(now)
}

func (r *replayCache) drawFlush() time.Duration {
	span := int64(replayFlushMax - replayFlushMin)
	if r.rnd != nil {
		return replayFlushMin + time.Duration(r.rnd.Int64N(span+1))
	}
	return replayFlushMin + time.Duration(mrand.Int64N(span+1))
}

// seen records sel and reports whether it was already present in a live generation.
func (r *replayCache) seen(sel []byte, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set.seen(sel, now)
}

// maybeFlush persists the filter when the flush interval has passed and something changed since
// the last write. The listener's sweep timer calls it even when no packet arrives.
func (r *replayCache) maybeFlush(now time.Time) {
	r.mu.Lock()
	r.set.rotate(now)
	if r.kv == nil || now.Before(r.nextFlush) {
		r.mu.Unlock()
		return
	}
	r.nextFlush = now.Add(r.drawFlush())
	r.persistLocked()
}

// flush persists the filter now (on shutdown).
func (r *replayCache) flush(now time.Time) {
	r.mu.Lock()
	r.set.rotate(now)
	if r.kv == nil {
		r.mu.Unlock()
		return
	}
	r.persistLocked()
}

// persistLocked writes only the changed generations, then the rotation state, and releases the
// lock before the KV calls. A nothing-changed filter writes nothing (the idle case). A generation
// is marked clean only after its PutTTL succeeded: on error it stays dirty, the next flush retries
// it, and a restart cannot silently reopen the replay window.
func (r *replayCache) persistLocked() {
	if !r.set.anyDirty() {
		r.mu.Unlock()
		return
	}
	type part struct {
		gen  int
		key  []byte
		data []byte
	}
	var parts []part
	for i := range r.set.gens {
		if g := &r.set.gens[i]; g.dirty {
			parts = append(parts, part{gen: i, key: genKey(i), data: g.marshal()})
		}
	}
	meta := r.set.metaMarshal()
	kv := r.kv
	r.mu.Unlock()
	for _, p := range parts {
		if err := kv.PutTTL(p.key, p.data, replayTTL); err != nil {
			r.report(err)
			continue
		}
		r.mu.Lock()
		r.set.gens[p.gen].dirty = false
		r.mu.Unlock()
	}
	if err := kv.PutTTL([]byte(bloomKey), meta, replayTTL); err != nil {
		r.report(err)
	}
}

// report sends a persistence error to the journal. It never carries keys or addresses.
func (r *replayCache) report(err error) {
	if r.onError != nil {
		r.onError(err)
		return
	}
	slog.Warn("noise3: replay cache persist failed", "err", err)
}
