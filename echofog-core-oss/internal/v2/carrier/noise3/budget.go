package noise3

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// Token-bucket budgets of wire.md §2.1: stranger first packets are processed at most 50 per
// second per source IP and 2000 per second per listener. Beyond the budget a packet is dropped
// unprocessed, which is what any random UDP service would look like.
const (
	strangerPerIP       = 50
	strangerPerListener = 2000
	budgetIdle          = 60 * time.Second
	maxBudgetEntries    = 8192
)

type bucket struct {
	tokens float64
	last   time.Time
}

func take(b *bucket, rate float64, now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = rate
	} else {
		b.tokens += now.Sub(b.last).Seconds() * rate
		if b.tokens > rate {
			b.tokens = rate
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type budgets struct {
	mu       sync.Mutex
	listener bucket
	perIP    map[string]*bucket
}

func newBudgets() *budgets {
	return &budgets{perIP: make(map[string]*bucket)}
}

// allow consumes one token from both the listener and the source-IP bucket.
func (b *budgets) allow(ip string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !take(&b.listener, strangerPerListener, now) {
		return false
	}
	ipB, ok := b.perIP[ip]
	if !ok {
		if len(b.perIP) >= maxBudgetEntries {
			b.evict(now)
		}
		ipB = &bucket{}
		b.perIP[ip] = ipB
	}
	return take(ipB, strangerPerIP, now)
}

func (b *budgets) evict(now time.Time) {
	for k, v := range b.perIP {
		if now.Sub(v.last) > budgetIdle || v.last.IsZero() {
			delete(b.perIP, k)
		}
	}
	// Still too many: drop arbitrary entries. The map is keyed by source IP, so no honest
	// flow is tracked here; eviction only resets a rate counter.
	for k := range b.perIP {
		if len(b.perIP) < maxBudgetEntries*3/4 {
			break
		}
		delete(b.perIP, k)
	}
}

// Bloom parameters of the persisted replay cache. 2^24 bits with six hashes keep the false
// positive rate near 1% even when a peer fills the whole 900 s window at the 2000/s listener
// budget, and cost 2 MiB of memory.
const (
	bloomBits  = 1 << 24
	bloomWords = bloomBits / 64
	bloomHash  = 6
	bloomKey   = "replay-bloom"
)

type bloom struct {
	bits [bloomWords]uint64
}

func (b *bloom) indexes(sel []byte) [bloomHash]uint64 {
	sum := sha256Sum(sel)
	h1 := binary.LittleEndian.Uint64(sum[0:8]) % bloomBits
	h2 := binary.LittleEndian.Uint64(sum[8:16]) | 1
	var out [bloomHash]uint64
	for i := range out {
		out[i] = (h1 + uint64(i)*h2) % bloomBits
	}
	return out
}

// seen reports whether all bits of sel were already set and sets them otherwise.
func (b *bloom) seen(sel []byte) bool {
	idx := b.indexes(sel)
	all := true
	for _, i := range idx {
		if b.bits[i/64]&(1<<(i%64)) == 0 {
			all = false
			b.bits[i/64] |= 1 << (i % 64)
		}
	}
	return all
}

func (b *bloom) marshal() []byte {
	out := make([]byte, bloomWords*8)
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(out[i*8:], w)
	}
	return out
}

func (b *bloom) unmarshal(data []byte) error {
	if len(data) != bloomWords*8 {
		return errors.New("noise3: replay bloom has the wrong size")
	}
	for i := range b.bits {
		b.bits[i] = binary.LittleEndian.Uint64(data[i*8:])
	}
	return nil
}
