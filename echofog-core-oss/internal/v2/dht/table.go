// Package dht implements the DHT of protocol v2: the 256-bucket routing table of dht.md §5 now,
// and the dht/2 records and lookups in later tasks.
//
// The routing table is deliberately narrow about what it accepts. An entry is a device ID and
// that device's own signed card. It is added or updated only from an authenticated session with
// the device or from a strictly newer card; a third party replaying an old card can never change
// the address of an entry (dht.md §5, the v1 weakness C4–C6).
package dht

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"math/rand"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Parameters of dht.md §1 and §5.
const (
	// BucketCount is the number of buckets; the keyspace is 256 bits and a bucket is one
	// leading bit.
	BucketCount = 256
	// K is the bucket size and the result size (dht.md §1).
	K = 20
	// Alpha is the number of parallel requests per lookup step (dht.md §1). The routing table
	// exports it; the lookup of T5.2 uses it.
	Alpha = 3

	// DeadAfter is how long an entry may stay unconfirmed before the table pings it and drops it
	// when the check fails (dht.md §5).
	DeadAfter = 5 * time.Minute

	// RefreshMin/RefreshMax bound the bucket refresh interval (dht.md §5).
	RefreshMin = 10 * time.Minute
	RefreshMax = 15 * time.Minute
	// LowPowerRefreshMin/LowPowerRefreshMax are the same interval under the low_power flag
	// (platforms.md §2).
	LowPowerRefreshMin = 45 * time.Minute
	LowPowerRefreshMax = 75 * time.Minute

	// SaveBase is the save interval; AD13 adds ±20 % jitter.
	SaveBase = 5 * time.Minute
	// MemCapBuckets is the mem_cap_mb limit: 32 buckets' worth of cards stay in memory and the
	// rest stay in the store (platforms.md §2).
	MemCapBuckets = 32

	// maxPerUnitPerBucket is "at most 2 entries per diversity unit per bucket" (dht.md §5).
	maxPerUnitPerBucket = 2
	// PingTimeout bounds one liveness check.
	PingTimeout = 10 * time.Second
	// pruneEvery is how often the maintenance loop looks for entries unconfirmed for DeadAfter.
	pruneEvery = time.Minute
)

// ErrConfig is a local wiring error.
var ErrConfig = errors.New("dht: invalid configuration")

// DeviceID is a device position in the keyspace: SHA-256 of the device akey (dht.md §1).
type DeviceID [32]byte

func (d DeviceID) String() string { return fmt.Sprintf("%x", d[:]) }

// Pinger checks that a node is still alive, on an existing session or by a new one. The routing
// table calls it only for an entry it is about to replace or remove as dead.
type Pinger interface {
	Ping(ctx context.Context, card *objects.Card) error
}

// LookupFunc is the iterative lookup of T5.2. The table uses it for the start-up self-lookup and
// for bucket refreshes; a nil LookupFunc makes both a no-op.
type LookupFunc func(ctx context.Context, target [32]byte) error

// Config assembles one routing table.
type Config struct {
	// Self is the local device ID. Required.
	Self DeviceID
	// Store is the persistent namespace; nil keeps the table in memory only (tests, a node
	// that has not opened its profile). The table does not close it.
	Store store.KV
	// Now is the corrected clock (AD13). Nil is the system clock.
	Now func() time.Time
	// Rand is the jitter and random-target source. Nil seeds one from the clock.
	Rand *rand.Rand
	// LowPower uses the 45–75 minute refresh window and no other timer (platforms.md §2).
	LowPower bool
	// MemCapMB enables the in-memory cap of MemCapBuckets buckets' worth when positive
	// (platforms.md §2).
	MemCapMB int
	// MemoryLimit overrides the in-memory entry cap for tests; 0 derives it from MemCapMB.
	MemoryLimit int
	// Pinger checks entries before replacement and after DeadAfter. Nil disables both, so a
	// full bucket never replaces an entry.
	Pinger Pinger
	// Lookup runs the start-up self-lookup and bucket refreshes. Nil leaves both as a no-op.
	Lookup LookupFunc
	// Log receives diagnostics. Nil means slog.Default.
	Log *slog.Logger
}

// entryState is a lock-free snapshot of the fields the store needs. The card itself is immutable,
// so the snapshot can be written after the table lock is released.
type entryState struct {
	device    DeviceID
	confirmed time.Time
	card      *objects.Card
}

func (e *entry) state() entryState {
	return entryState{device: e.device, confirmed: e.confirmed, card: e.card}
}

// entry is one routing table record: the device, its signed card and local bookkeeping.
type entry struct {
	device    DeviceID
	card      *objects.Card
	seq       uint64
	confirmed time.Time
	units     []netip.Prefix // diversity units: IPv4 /24, IPv6 /32
	wide      []netip.Prefix // dht.md §5 table cap: IPv4 /16, IPv6 /24
	endpoints []string       // the card's addresses, one entry per address
}

// bucket is one of the 256 leading-bit groups.
type bucket struct {
	entries []*entry
	// nextRefresh is when a lookup into this bucket is due. It is drawn from uniform(10,15)
	// minutes, or uniform(45,75) under low_power.
	nextRefresh time.Time
	// checking guards the single liveness check a bucket may have in flight (dht.md §5).
	checking bool
}

// Table is the DHT routing table of dht.md §5. It is safe for concurrent use.
type Table struct {
	self     DeviceID
	kv       store.KV
	now      func() time.Time
	rnd      *rand.Rand
	lowPower bool
	memLimit int
	maxPer16 int
	pinger   Pinger
	lookup   LookupFunc
	log      *slog.Logger

	mu        sync.Mutex
	buckets   [BucketCount]*bucket
	index     map[DeviceID]*entry
	endpoints map[string]DeviceID
	total     int
	nextSave  time.Time
	nextPrune time.Time

	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// Open builds a table and restores the cards kept in cfg.Store. Every restored card is verified
// again; a damaged or expired record is skipped, never trusted.
func Open(cfg Config) (*Table, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	t := &Table{
		self:      cfg.Self,
		kv:        cfg.Store,
		now:       now,
		rnd:       rnd,
		lowPower:  cfg.LowPower,
		maxPer16:  BucketCount * K / 10, // 10 % of the table
		pinger:    cfg.Pinger,
		lookup:    cfg.Lookup,
		log:       log,
		index:     make(map[DeviceID]*entry),
		endpoints: make(map[string]DeviceID),
	}
	switch {
	case cfg.MemoryLimit > 0:
		t.memLimit = cfg.MemoryLimit
	case cfg.MemCapMB > 0:
		t.memLimit = MemCapBuckets * K
	}
	start := now()
	for i := range t.buckets {
		t.buckets[i] = &bucket{nextRefresh: start.Add(refreshAfter(t.lowPower, rnd))}
	}
	t.nextSave = start.Add(saveAfter(rnd))
	t.nextPrune = start.Add(pruneEvery)
	if t.kv != nil {
		if err := t.load(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// AddFromSession adds or updates the card of an authenticated session. This is the only source
// that may replace a card without a higher seq (dht.md §5): the handshake proved that the device
// is reachable at the address in its own card.
func (t *Table) AddFromSession(card *objects.Card) bool { return t.add(card, true) }

// AddFromCard adds a card learned from the DHT or PEX. A new device may be added; an existing
// entry changes only with a strictly higher seq, so a third party can never rewrite its address.
func (t *Table) AddFromCard(card *objects.Card) bool { return t.add(card, false) }

func (t *Table) add(card *objects.Card, session bool) bool {
	if card == nil || card.Env == nil {
		return false
	}
	id := DeviceID(card.DeviceID)
	if id == t.self {
		return false
	}
	now := t.now()

	t.mu.Lock()
	old := t.index[id]
	if card.Body.GetFlags()&objects.FlagUnlisted != 0 {
		// An unlisted node is never in the table, whatever the source.
		if old != nil {
			t.removeLocked(old)
		}
		t.mu.Unlock()
		if old != nil {
			t.deleteEntry(id)
		}
		return false
	}

	cand := newEntry(card, now)
	if old != nil {
		if !session && card.Env.Seq <= old.seq {
			t.mu.Unlock()
			return false
		}
		if !t.fitsLocked(cand, old) {
			t.mu.Unlock()
			return false
		}
		t.removeEndpointsLocked(old)
		old.card = cand.card
		old.seq = cand.seq
		old.confirmed = now
		old.units = cand.units
		old.wide = cand.wide
		old.endpoints = cand.endpoints
		t.addEndpointsLocked(old)
		t.sortBucketLocked(t.bucketOf(id))
		state := old.state()
		t.mu.Unlock()
		t.persist(state)
		return true
	}

	if !t.fitsLocked(cand, nil) {
		t.mu.Unlock()
		return false
	}
	b := t.buckets[t.bucketOf(id)]
	if len(b.entries) < K {
		t.insertLocked(b, cand)
		state := cand.state()
		t.mu.Unlock()
		t.persist(state)
		return true
	}

	// A full bucket replaces its least recently confirmed entry only after that entry fails a
	// ping, and only one such check may run per bucket (dht.md §5, V4).
	if t.pinger == nil || b.checking {
		t.mu.Unlock()
		return false
	}
	victim := b.entries[0] // buckets are ordered by confirmation time, oldest first
	victimCard := victim.card
	b.checking = true
	t.mu.Unlock()

	t.checkAndReplace(b, victim, victimCard, cand)
	return false
}

// checkAndReplace pings the victim and inserts the candidate only when the check fails.
func (t *Table) checkAndReplace(b *bucket, victim *entry, victimCard *objects.Card, cand *entry) {
	ctx, cancel := context.WithTimeout(context.Background(), PingTimeout)
	err := t.pinger.Ping(ctx, victimCard)
	cancel()

	t.mu.Lock()
	b.checking = false
	cur := t.index[victim.device]
	var removed *entry
	var added *entryState
	if cur == victim {
		if err != nil {
			removed = t.removeLocked(victim)
			if t.index[cand.device] == nil && t.fitsLocked(cand, nil) && len(b.entries) < K {
				t.insertLocked(b, cand)
				state := cand.state()
				added = &state
			}
		} else {
			victim.confirmed = t.now()
			t.sortBucketLocked(t.bucketOf(victim.device))
		}
	}
	t.mu.Unlock()

	if removed != nil {
		t.deleteEntry(removed.device)
	}
	if added != nil {
		t.persist(*added)
	}
}

// fitsLocked applies the diversity, address and table-wide caps to a candidate. ignore is the
// entry the candidate is about to replace.
func (t *Table) fitsLocked(cand *entry, ignore *entry) bool {
	for _, ep := range cand.endpoints {
		if owner, ok := t.endpoints[ep]; ok {
			if ignore == nil || owner != ignore.device {
				// At most one entry per address.
				return false
			}
		}
	}

	b := t.buckets[t.bucketOf(cand.device)]
	for _, unit := range cand.units {
		n := 0
		for _, e := range b.entries {
			if e == ignore {
				continue
			}
			if hasPrefix(e.units, unit) {
				n++
			}
		}
		if n >= maxPerUnitPerBucket {
			return false
		}
	}

	if t.maxPer16 > 0 {
		for _, wide := range cand.wide {
			n := 0
			for _, other := range t.buckets {
				for _, e := range other.entries {
					if e == ignore {
						continue
					}
					if hasPrefix(e.wide, wide) {
						n++
					}
				}
			}
			if n >= t.maxPer16 {
				// At most 10 % of the table per /16 (IPv6 /24).
				return false
			}
		}
	}
	return true
}

func hasPrefix(list []netip.Prefix, want netip.Prefix) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// insertLocked puts a fresh entry into a bucket and trims the in-memory table.
func (t *Table) insertLocked(b *bucket, e *entry) {
	b.entries = append(b.entries, e)
	t.index[e.device] = e
	t.addEndpointsLocked(e)
	t.total++
	b.nextRefresh = t.now().Add(refreshAfter(t.lowPower, t.rnd))
	t.sortBucketLocked(t.bucketOf(e.device))
	t.trimMemoryLocked()
}

// trimMemoryLocked keeps at most memLimit entries in memory. Entries dropped here stay in the
// store (platforms.md §2: "the rest on disk").
func (t *Table) trimMemoryLocked() {
	for t.memLimit > 0 && t.total > t.memLimit {
		victim := t.leastRecentLocked()
		if victim == nil {
			return
		}
		t.removeLocked(victim)
	}
}

func (t *Table) leastRecentLocked() *entry {
	var victim *entry
	for _, b := range t.buckets {
		for _, e := range b.entries {
			if victim == nil || entryLess(e, victim) {
				victim = e
			}
		}
	}
	return victim
}

func (t *Table) removeLocked(e *entry) *entry {
	if e == nil {
		return nil
	}
	b := t.buckets[t.bucketOf(e.device)]
	for i, cur := range b.entries {
		if cur == e {
			b.entries = append(b.entries[:i], b.entries[i+1:]...)
			break
		}
	}
	if t.index[e.device] == e {
		delete(t.index, e.device)
		t.removeEndpointsLocked(e)
		t.total--
	}
	return e
}

func (t *Table) addEndpointsLocked(e *entry) {
	for _, ep := range e.endpoints {
		t.endpoints[ep] = e.device
	}
}

func (t *Table) removeEndpointsLocked(e *entry) {
	for _, ep := range e.endpoints {
		if t.endpoints[ep] == e.device {
			delete(t.endpoints, ep)
		}
	}
}

// sortBucketLocked orders a bucket by confirmation time, then seq, then device ID: the least
// recently confirmed entry is first and is the one a full bucket pings.
func (t *Table) sortBucketLocked(idx int) {
	if idx < 0 || idx >= BucketCount {
		return
	}
	sort.SliceStable(t.buckets[idx].entries, func(i, j int) bool {
		return entryLess(t.buckets[idx].entries[i], t.buckets[idx].entries[j])
	})
}

func entryLess(a, b *entry) bool {
	if !a.confirmed.Equal(b.confirmed) {
		return a.confirmed.Before(b.confirmed)
	}
	if a.seq != b.seq {
		return a.seq < b.seq
	}
	return bytes.Compare(a.device[:], b.device[:]) < 0
}

func (t *Table) bucketOf(id DeviceID) int {
	idx := bucketIndex(t.self, id)
	if idx < 0 {
		return 0
	}
	return idx
}

// bucketIndex returns the number of leading bits self and other share, or -1 for the same ID.
func bucketIndex(self, other DeviceID) int {
	for i := 0; i < len(self); i++ {
		x := self[i] ^ other[i]
		if x != 0 {
			return i*8 + bits.LeadingZeros8(x)
		}
	}
	return -1
}

// Has reports whether a device is in the table.
func (t *Table) Has(id DeviceID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.index[id]
	return ok
}

// Card returns the card of a device, or nil.
func (t *Table) Card(id DeviceID) *objects.Card {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.index[id]; e != nil {
		return e.card
	}
	return nil
}

// Len returns the number of entries in memory.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}

// BucketLen returns the number of entries in one bucket.
func (t *Table) BucketLen(i int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i < 0 || i >= BucketCount {
		return 0
	}
	return len(t.buckets[i].entries)
}

// Closest returns up to n cards whose device IDs are closest to target by XOR distance. Order
// is strict distance; the diversity filtering of a lookup result belongs to T5.2.
func (t *Table) Closest(target [32]byte, n int) []*objects.Card {
	if n <= 0 {
		return nil
	}
	t.mu.Lock()
	all := make([]struct {
		device DeviceID
		card   *objects.Card
	}, 0, t.total)
	for _, b := range t.buckets {
		for _, e := range b.entries {
			all = append(all, struct {
				device DeviceID
				card   *objects.Card
			}{device: e.device, card: e.card})
		}
	}
	t.mu.Unlock()

	dist := target
	sort.SliceStable(all, func(i, j int) bool {
		return lessDistance(all[i].device, all[j].device, DeviceID(dist))
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]*objects.Card, 0, len(all))
	for _, e := range all {
		out = append(out, e.card)
	}
	return out
}

// lessDistance reports whether a is closer to target than b.
func lessDistance(a, b, target DeviceID) bool {
	for i := 0; i < 32; i++ {
		da := a[i] ^ target[i]
		db := b[i] ^ target[i]
		if da != db {
			return da < db
		}
	}
	return false
}

// SetPinger replaces the liveness checker (tests, a runtime that learns its session layer).
func (t *Table) SetPinger(p Pinger) {
	t.mu.Lock()
	t.pinger = p
	t.mu.Unlock()
}

// SetLookup replaces the lookup used for refreshes and the start-up self-lookup.
func (t *Table) SetLookup(f LookupFunc) {
	t.mu.Lock()
	t.lookup = f
	t.mu.Unlock()
}

// Start launches the maintenance loop and the self-lookup required at start-up (dht.md §5).
func (t *Table) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	if t.started || t.stopped {
		t.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.started = true
	self := t.self
	lookup := t.lookup
	t.mu.Unlock()

	if lookup != nil {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			if err := lookup(runCtx, [32]byte(self)); err != nil && runCtx.Err() == nil {
				t.log.Debug("dht: start-up self-lookup", "err", err)
			}
		}()
	}

	t.wg.Add(1)
	go t.maintain(runCtx)
}

// Stop stops the maintenance loop. It is idempotent and does not close the store.
func (t *Table) Stop() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.wg.Wait()
}

func (t *Table) maintain(ctx context.Context) {
	defer t.wg.Done()
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

func (t *Table) tick(ctx context.Context) {
	now := t.now()
	t.mu.Lock()
	save := !now.Before(t.nextSave)
	prune := !now.Before(t.nextPrune)
	if save {
		t.nextSave = now.Add(saveAfter(t.rnd))
	}
	if prune {
		t.nextPrune = now.Add(pruneEvery)
	}
	t.mu.Unlock()

	if save {
		if err := t.Save(); err != nil {
			t.log.Warn("dht: saving the routing table", "err", err)
		}
	}
	if prune {
		t.Prune(ctx)
	}
	t.Refresh(ctx)
}

// Prune pings entries that have not been confirmed for DeadAfter and drops the ones that do not
// answer (dht.md §5).
func (t *Table) Prune(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Ping the stale entries without holding the lock; the card pointer is immutable.
	stale := make([]struct {
		e    *entry
		card *objects.Card
	}, 0)
	t.mu.Lock()
	p := t.pinger
	now := t.now()
	if p != nil {
		for _, b := range t.buckets {
			for _, e := range b.entries {
				if e.confirmed.IsZero() || now.Sub(e.confirmed) >= DeadAfter {
					stale = append(stale, struct {
						e    *entry
						card *objects.Card
					}{e: e, card: e.card})
				}
			}
		}
	}
	t.mu.Unlock()
	if p == nil || len(stale) == 0 {
		return
	}

	var removed []*entry
	for _, s := range stale {
		if ctx.Err() != nil {
			break
		}
		pctx, cancel := context.WithTimeout(ctx, PingTimeout)
		err := p.Ping(pctx, s.card)
		cancel()

		t.mu.Lock()
		if t.index[s.e.device] == s.e {
			if err != nil {
				removed = append(removed, t.removeLocked(s.e))
			} else {
				s.e.confirmed = t.now()
				t.sortBucketLocked(t.bucketOf(s.e.device))
			}
		}
		t.mu.Unlock()
	}
	for _, e := range removed {
		t.deleteEntry(e.device)
	}
}

// Refresh runs one lookup of a random ID in every bucket that has not been touched for its
// refresh interval (dht.md §5). A nil LookupFunc still advances the timers.
func (t *Table) Refresh(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := t.now()

	t.mu.Lock()
	var targets [][32]byte
	for i, b := range t.buckets {
		if len(b.entries) == 0 {
			continue
		}
		if b.nextRefresh.IsZero() || now.Before(b.nextRefresh) {
			continue
		}
		b.nextRefresh = now.Add(refreshAfter(t.lowPower, t.rnd))
		targets = append(targets, randomIDInBucket(t.self, i, t.rnd))
	}
	lookup := t.lookup
	t.mu.Unlock()

	if lookup == nil {
		return
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		if err := lookup(ctx, target); err != nil && ctx.Err() == nil {
			t.log.Debug("dht: bucket refresh", "err", err)
		}
	}
}

// randomIDInBucket returns an ID that shares exactly idx leading bits with self.
func randomIDInBucket(self DeviceID, idx int, rnd *rand.Rand) [32]byte {
	var out [32]byte
	// math/rand.Rand.Read never returns an error.
	_, _ = rnd.Read(out[:])
	for bit := 0; bit < idx; bit++ {
		mask := byte(0x80) >> (bit % 8)
		if self[bit/8]&mask != 0 {
			out[bit/8] |= mask
		} else {
			out[bit/8] &^= mask
		}
	}
	// The bit at idx must differ from self's, so the ID falls in bucket idx.
	mask := byte(0x80) >> (idx % 8)
	out[idx/8] &^= mask
	if self[idx/8]&mask == 0 {
		out[idx/8] |= mask
	}
	return out
}

func refreshAfter(lowPower bool, rnd *rand.Rand) time.Duration {
	if lowPower {
		return uniform(LowPowerRefreshMin, LowPowerRefreshMax, rnd)
	}
	return uniform(RefreshMin, RefreshMax, rnd)
}

// saveAfter is the save interval with the ±20 % jitter of AD13.
func saveAfter(rnd *rand.Rand) time.Duration {
	return uniform(SaveBase*4/5, SaveBase*6/5, rnd)
}

func uniform(min, max time.Duration, rnd *rand.Rand) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rnd.Float64()*float64(max-min))
}

// newEntry extracts the diversity units, wide masks and endpoints of a card.
func newEntry(card *objects.Card, confirmed time.Time) *entry {
	e := &entry{
		device:    DeviceID(card.DeviceID),
		card:      card,
		seq:       card.Env.Seq,
		confirmed: confirmed,
	}
	units := make(map[netip.Prefix]struct{})
	wide := make(map[netip.Prefix]struct{})
	endpoints := make(map[string]struct{})

	for _, a := range card.Addrs() {
		host := a.GetHost()
		if host == "" {
			continue
		}
		port := a.GetPort()
		ip, ok := parseHost(host)
		if !ok {
			// A name is still an address for the one-entry-per-address rule, though it has no
			// diversity unit.
			endpoints[strings.ToLower(host)+":"+strconv.FormatUint(uint64(port), 10)] = struct{}{}
			continue
		}
		if port > 65535 {
			continue
		}
		endpoints[netip.AddrPortFrom(ip, uint16(port)).String()] = struct{}{}
		units[diversityUnit(ip)] = struct{}{}
		wide[wideMask(ip)] = struct{}{}
	}

	e.endpoints = sortedKeys(endpoints)
	for p := range units {
		e.units = append(e.units, p)
	}
	sort.Slice(e.units, func(i, j int) bool { return e.units[i].String() < e.units[j].String() })
	for p := range wide {
		e.wide = append(e.wide, p)
	}
	sort.Slice(e.wide, func(i, j int) bool { return e.wide[i].String() < e.wide[j].String() })
	return e
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parseHost(host string) (netip.Addr, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// diversityUnit is an IPv4 /24 or an IPv6 /32 (dht.md §1).
func diversityUnit(a netip.Addr) netip.Prefix {
	if a.Is4() {
		return netip.PrefixFrom(a, 24).Masked()
	}
	return netip.PrefixFrom(a, 32).Masked()
}

// wideMask is the IPv4 /16 or IPv6 /24 of dht.md §5.
func wideMask(a netip.Addr) netip.Prefix {
	if a.Is4() {
		return netip.PrefixFrom(a, 16).Masked()
	}
	return netip.PrefixFrom(a, 24).Masked()
}
