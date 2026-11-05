package reality2

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// The token replay cache of wire.md §3.3 step 4: the 32-byte tokens of admitted hellos,
// kept 900 s, shared by every listener that uses the same masking key, persisted across
// restarts as a Bloom filter.
const (
	// ReplayTTL is how long an accepted token stays in the cache.
	ReplayTTL = 900 * time.Second

	// Bloom geometry: 2^19 bits (64 KiB) with 4 probes holds roughly 45 000 tokens at a 1 %
	// false-positive rate, well above 900 s of admissions for any single masking key.
	bloomBits   = 1 << 19
	bloomHashes = 4

	// replayFlushMin and replayFlushJitter make the persistence interval uniform in
	// [8 s, 12 s], with the ±20 % jitter of AD13.
	replayFlushMin    = 8 * time.Second
	replayFlushJitter = 4 * time.Second

	replayPrefix  = "v2/reality2/replay/"
	replayMagic   = "r2rp"
	replayVersion = 1
)

// ErrReplay is the KV read/write error of the replay cache; a broken cache never admits a
// token the node has seen (which is a stronger failure than refusing to start).
var ErrReplay = errors.New("reality2: replay cache")

// ReplayConfig configures Replay.
type ReplayConfig struct {
	// KV persists the filters. Nil keeps them in memory only.
	KV carrier.KV
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// OnError receives persistence errors; nil ignores them.
	OnError func(error)
}

// Replay is the shared token replay cache. It is safe for concurrent use.
type Replay struct {
	kv      carrier.KV
	now     func() time.Time
	onError func(error)

	mu   sync.Mutex
	sets map[[32]byte]*replaySet
}

// replaySet is two Bloom generations of one masking key: the current one and the previous
// one, so a token stays known for between one and two TTLs.
type replaySet struct {
	cur, prev [bloomBits / 8]byte
	rotated   time.Time
	loaded    bool
	dirty     bool
}

// NewReplay builds a replay cache. A nil KV makes it memory-only.
func NewReplay(cfg ReplayConfig) *Replay {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Replay{kv: cfg.KV, now: cfg.Now, onError: cfg.OnError, sets: make(map[[32]byte]*replaySet)}
}

// SeenOrAdd records a token and reports whether it was already known. A hit means the hello
// is a replay and must go to the cover.
func (r *Replay) SeenOrAdd(maskID [32]byte, token []byte) bool {
	sum := sha256.Sum256(token)
	h1 := binary.BigEndian.Uint64(sum[:8])
	h2 := binary.BigEndian.Uint64(sum[8:16]) | 1

	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.setLocked(maskID)
	if set == nil {
		return false
	}
	if r.now().Sub(set.rotated) >= ReplayTTL {
		set.prev = set.cur
		set.cur = [bloomBits / 8]byte{}
		set.rotated = r.now()
		set.dirty = true
	}
	if bloomContains(set.cur[:], h1, h2) || bloomContains(set.prev[:], h1, h2) {
		return true
	}
	bloomAdd(set.cur[:], h1, h2)
	set.dirty = true
	return false
}

// setLocked returns the set of maskID, loading it from the KV on first use.
func (r *Replay) setLocked(maskID [32]byte) *replaySet {
	if s := r.sets[maskID]; s != nil {
		if !s.loaded {
			r.loadLocked(maskID, s)
		}
		return s
	}
	if len(r.sets) >= 64 {
		// Bound: one process rarely holds more than 8 live masking keys, but a caller may
		// rotate many. Evicting the oldest set only reopens a replay window for a key no
		// longer in use.
		var oldest [32]byte
		var oldestAt time.Time
		first := true
		for k, s := range r.sets {
			if first || s.rotated.Before(oldestAt) {
				oldest, oldestAt, first = k, s.rotated, false
			}
		}
		delete(r.sets, oldest)
	}
	s := &replaySet{rotated: r.now()}
	r.sets[maskID] = s
	r.loadLocked(maskID, s)
	return s
}

func (r *Replay) loadLocked(maskID [32]byte, s *replaySet) {
	s.loaded = true
	if r.kv == nil {
		return
	}
	value, ok, err := r.kv.Get(context.Background(), replayKey(maskID))
	if err != nil {
		r.report(fmt.Errorf("%w: load: %v", ErrReplay, err))
		return
	}
	if !ok {
		return
	}
	if err := s.unmarshal(value); err != nil {
		r.report(err)
	}
}

// Flush persists every dirty set. It is called periodically by Run and on Close.
func (r *Replay) Flush() {
	if r.kv == nil {
		return
	}
	r.mu.Lock()
	type pending struct {
		id    [32]byte
		value []byte
	}
	var out []pending
	for id, s := range r.sets {
		if !s.dirty {
			continue
		}
		out = append(out, pending{id, s.marshal()})
		s.dirty = false
	}
	r.mu.Unlock()
	for _, p := range out {
		if err := r.kv.Put(context.Background(), replayKey(p.id), p.value); err != nil {
			r.report(fmt.Errorf("%w: save: %v", ErrReplay, err))
			r.mu.Lock()
			if s := r.sets[p.id]; s != nil {
				s.dirty = true
			}
			r.mu.Unlock()
		}
	}
}

// Run flushes the cache every uniform(8, 12) s until ctx is done. It then flushes once more.
func (r *Replay) Run(ctx context.Context) {
	if r.kv == nil {
		return
	}
	timer := time.NewTimer(replayFlushMin + time.Duration(mrand.Int64N(int64(replayFlushJitter))))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Flush()
			return
		case <-timer.C:
			r.Flush()
			timer.Reset(replayFlushMin + time.Duration(mrand.Int64N(int64(replayFlushJitter))))
		}
	}
}

func (r *Replay) report(err error) {
	if r.onError != nil {
		r.onError(err)
	}
}

func replayKey(maskID [32]byte) []byte {
	return []byte(replayPrefix + hex.EncodeToString(maskID[:16]))
}

func (s *replaySet) marshal() []byte {
	b := make([]byte, 0, 4+1+8+2*len(s.cur))
	b = append(b, replayMagic...)
	b = append(b, replayVersion)
	b = binary.BigEndian.AppendUint64(b, uint64(s.rotated.UnixNano()))
	b = append(b, s.cur[:]...)
	b = append(b, s.prev[:]...)
	return b
}

func (s *replaySet) unmarshal(b []byte) error {
	const header = 4 + 1 + 8
	if len(b) != header+2*bloomBits/8 || string(b[:4]) != replayMagic {
		return fmt.Errorf("%w: bad entry", ErrReplay)
	}
	if b[4] != replayVersion {
		return fmt.Errorf("%w: unknown version %d", ErrReplay, b[4])
	}
	s.rotated = time.Unix(0, int64(binary.BigEndian.Uint64(b[5:13])))
	copy(s.cur[:], b[header:header+bloomBits/8])
	copy(s.prev[:], b[header+bloomBits/8:])
	return nil
}

func bloomContains(bits []byte, h1, h2 uint64) bool {
	for i := uint64(0); i < bloomHashes; i++ {
		idx := (h1 + i*h2) % bloomBits
		if bits[idx>>3]&(1<<(idx&7)) == 0 {
			return false
		}
	}
	return true
}

func bloomAdd(bits []byte, h1, h2 uint64) {
	for i := uint64(0); i < bloomHashes; i++ {
		idx := (h1 + i*h2) % bloomBits
		bits[idx>>3] |= 1 << (idx & 7)
	}
}
