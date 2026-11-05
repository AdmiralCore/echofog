package noise3

import (
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
}

func newReplayCache(kv KV, now time.Time, rnd *mrand.Rand) *replayCache {
	r := &replayCache{set: &bloomSet{lastRotate: now}, kv: kv, rnd: rnd}
	r.nextFlush = now.Add(r.drawFlush())
	if kv != nil {
		if ok, err := kv.Has([]byte(bloomKey)); err == nil && ok {
			if data, err := kv.Get([]byte(bloomKey)); err == nil {
				if err := r.set.unmarshal(data); err == nil {
					r.set.rotate(now)
				}
			}
		}
	}
	return r
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

// maybeFlush persists the filter when the flush interval has passed. The listener's sweep timer
// calls it even when no packet arrives.
func (r *replayCache) maybeFlush(now time.Time) {
	r.mu.Lock()
	r.set.rotate(now)
	if now.Before(r.nextFlush) {
		r.mu.Unlock()
		return
	}
	r.nextFlush = now.Add(r.drawFlush())
	data := r.set.marshal()
	kv := r.kv
	r.mu.Unlock()
	if kv != nil {
		_ = kv.PutTTL([]byte(bloomKey), data, replayTTL)
	}
}

// flush persists the filter now (on shutdown).
func (r *replayCache) flush(now time.Time) {
	r.mu.Lock()
	r.set.rotate(now)
	data := r.set.marshal()
	kv := r.kv
	r.mu.Unlock()
	if kv != nil {
		_ = kv.PutTTL([]byte(bloomKey), data, replayTTL)
	}
}
