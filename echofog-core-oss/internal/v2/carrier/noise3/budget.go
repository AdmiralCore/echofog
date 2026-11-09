package noise3

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
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

// budgetKey is the per-source budget key. An IPv6 subscriber moves freely inside its /64, so the
// key is the /64 prefix; IPv4 (and v4-mapped) addresses keep the full address.
func budgetKey(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

func newBudgets() *budgets {
	return &budgets{perIP: make(map[string]*bucket)}
}

// allow consumes one token from the source-IP bucket first and the listener bucket only when the
// IP token was taken. A noisy IP therefore cannot drain the listener-wide budget (wire.md §2.1).
func (b *budgets) allow(ip string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ipB, ok := b.perIP[ip]
	if !ok {
		if len(b.perIP) >= maxBudgetEntries {
			b.evict(now)
		}
		ipB = &bucket{}
		b.perIP[ip] = ipB
	}
	if !take(ipB, strangerPerIP, now) {
		return false
	}
	return take(&b.listener, strangerPerListener, now)
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

// The replay cache is a set of rotating Bloom generations. A generation lives bloomGens periods
// and is cleared when the rotation index wraps; the live set therefore covers 960 s at
// 2000 Init/s with a bounded false-positive rate. Per generation the filter has 2^22 bits and six
// hashes; the worst-case FPR of one generation at 2000/s is about 0.06%, and the chance that any
// of the eight live generations reports a false positive is below 0.5%.
const (
	bloomGens     = 8
	bloomPeriod   = 120 * time.Second
	bloomGenBits  = 1 << 22
	bloomGenWords = bloomGenBits / 64
	bloomHash     = 6
	bloomKey      = "replay-bloom"
	bloomMagic    = "N3B3"
)

type bloom struct {
	bits [bloomGenWords]uint64
	// used is true when any bit may be set; dirty is true when the persisted copy differs.
	used, dirty bool
	// ver increases on every mutation of the persisted bits. A flush snapshots it and clears
	// dirty only if the generation did not change while the KV write was in flight.
	ver uint64
}

func (b *bloom) indexes(sel []byte) [bloomHash]uint64 {
	sum := sha256Sum(sel)
	h1 := binary.LittleEndian.Uint64(sum[0:8]) % bloomGenBits
	h2 := binary.LittleEndian.Uint64(sum[8:16]) | 1
	var out [bloomHash]uint64
	for i := range out {
		out[i] = (h1 + uint64(i)*h2) % bloomGenBits
	}
	return out
}

// has reports whether all bits of sel are set in this generation.
func (b *bloom) has(sel []byte) bool {
	idx := b.indexes(sel)
	for _, i := range idx {
		if b.bits[i/64]&(1<<(i%64)) == 0 {
			return false
		}
	}
	return true
}

func (b *bloom) add(sel []byte) {
	for _, i := range b.indexes(sel) {
		b.bits[i/64] |= 1 << (i % 64)
	}
	b.used, b.dirty = true, true
	b.ver++
}

func (b *bloom) clear() {
	if b.used {
		b.dirty = true
	}
	b.bits = [bloomGenWords]uint64{}
	b.used = false
	b.ver++
}

func (b *bloom) marshal() []byte {
	out := make([]byte, bloomGenWords*8)
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(out[i*8:], w)
	}
	return out
}

func (b *bloom) unmarshal(data []byte) error {
	if len(data) != bloomGenWords*8 {
		return errors.New("noise3: replay bloom has the wrong size")
	}
	for i := range b.bits {
		b.bits[i] = binary.LittleEndian.Uint64(data[i*8:])
		if b.bits[i] != 0 {
			b.used = true
		}
	}
	b.dirty = false
	return nil
}

// bloomSet is the aging replay filter: generations rotate on the injected clock and the oldest
// generation is cleared, reclaiming its capacity.
type bloomSet struct {
	gens       [bloomGens]bloom
	current    int
	lastRotate time.Time
}

func (s *bloomSet) rotate(now time.Time) {
	if s.lastRotate.IsZero() {
		s.lastRotate = now
		return
	}
	for !now.Before(s.lastRotate.Add(bloomPeriod)) {
		if now.Sub(s.lastRotate) > time.Duration(bloomGens)*bloomPeriod {
			// More than a whole retention window passed (a suspended device or a stalled
			// process): every generation is stale, so start clean.
			for i := range s.gens {
				s.gens[i].clear()
			}
			s.current = 0
			s.lastRotate = now
			return
		}
		s.current = (s.current + 1) % bloomGens
		s.gens[s.current].clear()
		s.lastRotate = s.lastRotate.Add(bloomPeriod)
	}
}

// seen reports whether sel is present in any live generation and records it in the current one
// otherwise. Generation checks are independent: bits set in different generations must not be
// combined into a false match.
func (s *bloomSet) seen(sel []byte, now time.Time) bool {
	s.rotate(now)
	for i := range s.gens {
		if s.gens[i].has(sel) {
			return true
		}
	}
	s.gens[s.current].add(sel)
	return false
}

// metaMarshal encodes the rotation state; each generation is a separate KV value so that a
// flush rewrites only the generations that changed.
func (s *bloomSet) metaMarshal() []byte {
	out := make([]byte, 0, len(bloomMagic)+1+8)
	out = append(out, bloomMagic...)
	out = append(out, byte(s.current))
	var stamp [8]byte
	binary.LittleEndian.PutUint64(stamp[:], uint64(s.lastRotate.Unix()))
	return append(out, stamp[:]...)
}

func (s *bloomSet) metaUnmarshal(data []byte) error {
	if len(data) != len(bloomMagic)+1+8 || string(data[:4]) != bloomMagic {
		return errors.New("noise3: replay bloom has the wrong size or version")
	}
	current := int(data[4])
	if current >= bloomGens {
		return errors.New("noise3: replay bloom has a bad generation")
	}
	s.current = current
	s.lastRotate = time.Unix(int64(binary.LittleEndian.Uint64(data[5:13])), 0)
	return nil
}

func (s *bloomSet) anyDirty() bool {
	for i := range s.gens {
		if s.gens[i].dirty {
			return true
		}
	}
	return false
}

func genKey(i int) []byte { return []byte(bloomKey + "/g" + strconv.Itoa(i)) }
