package dht

import (
	"bytes"
	"container/heap"
	"container/list"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Limits of the storing node (dht.md §3, §4.3, services.md §2).
const (
	// MaxRecords is the total number of stored records (dht.md §3).
	MaxRecords = 20000
	// SignerKnown and SignerUnknown are the per-signer quotas for known and unknown labels.
	SignerKnown   = 8
	SignerUnknown = 4
	// SessionRecords and ServerSessionRecords are the per-publishing-session quotas; the second
	// applies when the publisher's card has role = server (guards publish for their clients).
	SessionRecords       = 64
	ServerSessionRecords = 1024
	// UnitFactor multiplies the session quota for one diversity unit of the publisher.
	UnitFactor = 3
	// StorePerSecond and StorePerHour limit STORE per session.
	StorePerSecond = 2
	StorePerHour   = 300
	// RequestRate and RequestBurst limit all requests per session (services.md §2).
	RequestRate  = 20
	RequestBurst = 60
	// CacheRecords is the popular-record cache size; CacheRecordsLowMem applies under mem_cap_mb.
	CacheRecords       = 1000
	CacheRecordsLowMem = 200
	// ReachWindow is how long a working session keeps a card eligible for NODES (dht.md §4.3).
	ReachWindow = 24 * time.Hour
)

// Peer is the authenticated publisher of a dht/2 stream, in the types this package needs. The
// node adapts node.Peer to it.
type Peer struct {
	Device [32]byte
	SID    [16]byte
	// Unit is the publisher's diversity unit (IPv4 /24, IPv6 /32) when the session travels on a
	// direct path. A zero value means unknown: the device then counts as its own unit.
	Unit netip.Prefix
	// Server reports that the publisher's card has role = server.
	Server bool
}

// Result is the outcome of one STORE: a STORED status, or an ERROR code when Err is non-zero.
type Result struct {
	Status uint32
	Err    uint32
}

// ServerConfig wires one storing server.
type ServerConfig struct {
	// Self is the local device ID, the centre of "closest" for eviction.
	Self DeviceID
	// Table is the routing table used to decide whether this node is among the 20 closest to a
	// key. Nil means every key is accepted (tests, a node with no table).
	Table *Table
	// Responsible overrides the Table check (tests).
	Responsible func(key [32]byte) bool
	// Reached returns the cards of nodes this node reached at an address of the card in a working
	// session within ReachWindow (dht.md §4.3). Nil means none: nothing is passed on.
	Reached func(now time.Time) []*objects.Card
	// Now is the corrected clock (AD13). Nil is the system clock.
	Now func() time.Time
	// MemCapMB, when positive, limits the popular-record cache to CacheRecordsLowMem records
	// (platforms.md §2).
	MemCapMB int
	// MaxRecords overrides the total record quota (tests); 0 means MaxRecords.
	MaxRecords int
	// Log receives diagnostics. Nil is silent.
	Log *slog.Logger
}

// ErrServerConfig is a local wiring error.
var ErrServerConfig = errors.New("dht: invalid server configuration")

// record is one stored copy of a signed object under one DHT key. All of its accounting lives in
// the Server and changes only in removeLocked and addLocked (AD19).
type record struct {
	key    [32]byte
	env    *objects.Envelope
	expiry uint64
	dist   [32]byte // key XOR self
	signer [32]byte
	known  bool
	sid    [16]byte
	unit   string
	gone   bool
	expIdx int
	farIdx int
}

type signerCount struct{ known, unknown int }

type cacheEntry struct {
	key    [32]byte
	env    *objects.Envelope
	expiry uint64
}

type storeRate struct {
	tokens float64
	last   time.Time
	hour   []time.Time // accepted STORE times inside the last hour, oldest first
}

type reqRate struct {
	tokens float64
	last   time.Time
}

// Server is the storing side of dht/2: records, quotas, the farthest-first eviction, the popular
// cache and the FIND_NODE / FIND_VALUE answers.
type Server struct {
	cfg ServerConfig
	max int

	mu       sync.Mutex
	records  map[[32]byte]*record
	expq     expHeap
	farq     farHeap
	signers  map[[32]byte]*signerCount
	sessions map[[16]byte]int
	units    map[string]int

	cacheMax int
	cache    map[[32]byte]*list.Element
	lru      *list.List

	rateMu sync.Mutex
	stores map[[16]byte]*storeRate
	reqs   map[[16]byte]*reqRate
}

// NewServer returns a storing server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{
		cfg:      cfg,
		max:      cfg.MaxRecords,
		records:  map[[32]byte]*record{},
		signers:  map[[32]byte]*signerCount{},
		sessions: map[[16]byte]int{},
		units:    map[string]int{},
		cache:    map[[32]byte]*list.Element{},
		lru:      list.New(),
		stores:   map[[16]byte]*storeRate{},
		reqs:     map[[16]byte]*reqRate{},
		cacheMax: CacheRecords,
	}
	if s.max <= 0 {
		s.max = MaxRecords
	}
	if cfg.MemCapMB > 0 {
		s.cacheMax = CacheRecordsLowMem
	}
	return s, nil
}

// Len returns the number of stored records (cached copies are not counted).
func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// CacheLen returns the number of cached copies.
func (s *Server) CacheLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cache)
}

// --- logical expiry index and accounting (AD19) ---

type expHeap []*record

func (h expHeap) Len() int           { return len(h) }
func (h expHeap) Less(i, j int) bool { return h[i].expiry < h[j].expiry }
func (h expHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].expIdx = i; h[j].expIdx = j }
func (h *expHeap) Push(x any)        { r := x.(*record); r.expIdx = len(*h); *h = append(*h, r) }
func (h *expHeap) Pop() any {
	old := *h
	r := old[len(old)-1]
	*h = old[:len(old)-1]
	return r
}

// farHeap is a max-heap by distance from self: the top is the record to evict.
type farHeap []*record

func (h farHeap) Len() int           { return len(h) }
func (h farHeap) Less(i, j int) bool { return bytes.Compare(h[i].dist[:], h[j].dist[:]) > 0 }
func (h farHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].farIdx = i; h[j].farIdx = j }
func (h *farHeap) Push(x any)        { r := x.(*record); r.farIdx = len(*h); *h = append(*h, r) }
func (h *farHeap) Pop() any {
	old := *h
	r := old[len(old)-1]
	*h = old[:len(old)-1]
	return r
}

// addLocked inserts r and charges every counter. It is the only place counters grow.
func (s *Server) addLocked(r *record) {
	s.records[r.key] = r
	heap.Push(&s.expq, r)
	heap.Push(&s.farq, r)
	sc := s.signers[r.signer]
	if sc == nil {
		sc = &signerCount{}
		s.signers[r.signer] = sc
	}
	if r.known {
		sc.known++
	} else {
		sc.unknown++
	}
	s.sessions[r.sid]++
	s.units[r.unit]++
}

// removeLocked deletes r from the store, both heaps and every counter, exactly once: a record
// that expires and is evicted or replaced in the same pass is already gone the second time.
func (s *Server) removeLocked(r *record) {
	if r.gone {
		return
	}
	r.gone = true
	if s.records[r.key] == r {
		delete(s.records, r.key)
	}
	heap.Remove(&s.expq, r.expIdx)
	heap.Remove(&s.farq, r.farIdx)
	if sc := s.signers[r.signer]; sc != nil {
		if r.known {
			sc.known--
		} else {
			sc.unknown--
		}
		if sc.known <= 0 && sc.unknown <= 0 {
			delete(s.signers, r.signer)
		}
	}
	if n := s.sessions[r.sid] - 1; n > 0 {
		s.sessions[r.sid] = n
	} else {
		delete(s.sessions, r.sid)
	}
	if n := s.units[r.unit] - 1; n > 0 {
		s.units[r.unit] = n
	} else {
		delete(s.units, r.unit)
	}
}

func (s *Server) expireLocked(now time.Time) {
	t := unixSec(now)
	for len(s.expq) > 0 && s.expq[0].expiry <= t {
		s.removeLocked(s.expq[0])
	}
}

// Expire drops every record whose logical expiry has passed.
func (s *Server) Expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
}

func unixSec(t time.Time) uint64 {
	if u := t.Unix(); u > 0 {
		return uint64(u)
	}
	return 0
}

// --- rate limits ---

// Allow applies the request limit of services.md §2: 20 requests per second with a burst of 60.
func (s *Server) Allow(sid [16]byte, now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	b := s.reqs[sid]
	if b == nil {
		if len(s.reqs) > 1024 {
			for k, v := range s.reqs {
				if now.Sub(v.last) > time.Minute {
					delete(s.reqs, k)
				}
			}
		}
		b = &reqRate{tokens: RequestBurst, last: now}
		s.reqs[sid] = b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens += d.Seconds() * RequestRate
		if b.tokens > RequestBurst {
			b.tokens = RequestBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allowStore applies dht.md §3: at most 2 STORE per second and 300 per hour per session.
func (s *Server) allowStore(sid [16]byte, now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	b := s.stores[sid]
	if b == nil {
		if len(s.stores) > 1024 {
			for k, v := range s.stores {
				if now.Sub(v.last) > time.Hour {
					delete(s.stores, k)
				}
			}
		}
		b = &storeRate{tokens: StorePerSecond, last: now}
		s.stores[sid] = b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens += d.Seconds() * StorePerSecond
		if b.tokens > StorePerSecond {
			b.tokens = StorePerSecond
		}
		b.last = now
	}
	cut := 0
	for cut < len(b.hour) && now.Sub(b.hour[cut]) >= time.Hour {
		cut++
	}
	b.hour = b.hour[cut:]
	if b.tokens < 1 || len(b.hour) >= StorePerHour {
		return false
	}
	b.tokens--
	b.hour = append(b.hour, now)
	return true
}

// --- STORE ---

// Store handles one STORE with the server clock.
func (s *Server) Store(p Peer, record []byte) Result { return s.StoreAt(p, record, s.cfg.Now()) }

// checked is a record that passed the verification of dht.md §3 step 1.
type checked struct {
	env    *objects.Envelope
	expiry uint64
	keys   [][32]byte
}

// check verifies a record and derives the keys it may be stored under. It takes no lock.
func (s *Server) check(rec []byte, now time.Time) (*checked, Result) {
	env, err := objects.Parse(rec)
	if err != nil {
		return nil, Result{Err: CodeBadRecord}
	}
	if env.Label == objects.LabelName || !strings.HasPrefix(env.Label, "echofog/") {
		return nil, Result{Status: StatusRefused}
	}
	if env.Label == objects.LabelCard {
		card, err := objects.VerifyCard(rec, now)
		if err != nil {
			return nil, Result{Status: StatusInvalid}
		}
		if !card.Publishable() {
			return nil, Result{Status: StatusRefused}
		}
		env = card.Env
	} else {
		env, err = objects.Verify(rec, now)
		if err != nil {
			return nil, Result{Status: StatusInvalid}
		}
	}
	exp := env.StoreExpiry(now)
	if exp <= unixSec(now) {
		return nil, Result{Status: StatusInvalid}
	}
	return &checked{env: env, expiry: exp, keys: recordKeys(env, now)}, Result{}
}

// recordKeys are the keys of the current and the adjacent days (dht.md §2, §3 step 2). STORE has
// no key field, so the server derives them from the record.
func recordKeys(env *objects.Envelope, now time.Time) [][32]byte {
	d := objects.DHTDay(now)
	days := []uint32{d, d + 1}
	if d > 0 {
		days = append(days, d-1)
	}
	out := make([][32]byte, 0, len(days))
	for _, day := range days {
		if env.Label == objects.LabelRelease {
			out = append(out, objects.ReleaseDHTKey(day))
		} else {
			out = append(out, objects.DHTKey(env.Label, env.SignerID(), day))
		}
	}
	return out
}

// responsible reports whether this node is among the 20 closest nodes it knows to key.
func (s *Server) responsible(key [32]byte) bool {
	if s.cfg.Responsible != nil {
		return s.cfg.Responsible(key)
	}
	if s.cfg.Table == nil {
		return true
	}
	closest := s.cfg.Table.Closest(key, K)
	if len(closest) < K {
		return true
	}
	return lessDistance(s.cfg.Self, DeviceID(closest[K-1].DeviceID), DeviceID(key))
}

func (s *Server) unitKey(p Peer) string {
	if p.Unit.IsValid() {
		return p.Unit.String()
	}
	return "dev:" + hex.EncodeToString(p.Device[:])
}

func sessionLimit(p Peer) int {
	if p.Server {
		return ServerSessionRecords
	}
	return SessionRecords
}

// StoreAt handles one STORE at an explicit time.
func (s *Server) StoreAt(p Peer, rec []byte, now time.Time) Result {
	if !s.allowStore(p.SID, now) {
		return Result{Status: StatusQuota}
	}
	c, bad := s.check(rec, now)
	if c == nil {
		return bad
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)

	var keys [][32]byte
	for _, k := range c.keys {
		if s.responsible(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return Result{Err: CodeWrongKey}
	}
	var first Result
	stored := false
	for _, k := range keys {
		r := s.insertLocked(p, c, k)
		if r.Status == StatusStored {
			stored = true
		} else if first == (Result{}) {
			first = r
		}
	}
	if stored {
		return Result{Status: StatusStored}
	}
	return first
}

func (s *Server) insertLocked(p Peer, c *checked, key [32]byte) Result {
	env := c.env
	old := s.records[key]
	if old != nil {
		switch {
		case env.Seq < old.env.Seq:
			return Result{Status: StatusStale}
		case bytes.Equal(env.Raw, old.env.Raw):
			return Result{Status: StatusStored}
		case objects.Prefer(old.env, env) == old.env && env.Seq == old.env.Seq:
			return Result{Status: StatusStale}
		}
	}
	signer := env.SignerID()
	known := env.Known()
	unit := s.unitKey(p)

	// Quotas, counted without the record this one replaces.
	sc := s.signers[signer]
	known0, unknown0 := 0, 0
	if sc != nil {
		known0, unknown0 = sc.known, sc.unknown
	}
	if old != nil && old.signer == signer {
		if old.known {
			known0--
		} else {
			unknown0--
		}
	}
	if known && known0 >= SignerKnown || !known && unknown0 >= SignerUnknown {
		return Result{Status: StatusQuota}
	}
	sessionN, unitN, total := s.sessions[p.SID], s.units[unit], len(s.records)
	if old != nil {
		if old.sid == p.SID {
			sessionN--
		}
		if old.unit == unit {
			unitN--
		}
		total--
	}
	if sessionN >= sessionLimit(p) || unitN >= UnitFactor*sessionLimit(p) {
		return Result{Status: StatusQuota}
	}
	r := &record{
		key: key, env: env, expiry: c.expiry, signer: signer, known: known, sid: p.SID, unit: unit,
	}
	for i := range key {
		r.dist[i] = key[i] ^ s.cfg.Self[i]
	}
	var evict *record
	if total >= s.max {
		far := s.farq[0]
		if far == old {
			// The replaced record is the farthest: removing it already makes room.
			far = nil
		}
		if far != nil {
			if bytes.Compare(r.dist[:], far.dist[:]) >= 0 {
				return Result{Status: StatusQuota}
			}
			evict = far
		}
	}
	if old != nil {
		s.removeLocked(old)
	}
	if evict != nil {
		s.removeLocked(evict)
	}
	s.addLocked(r)
	s.cacheRemoveLocked(key)
	return Result{Status: StatusStored}
}

// --- answers ---

// FindNode answers FIND_NODE: the closest cards among those this node reached at an address of the
// card within the last 24 hours, never unlisted or non-public ones (dht.md §4.3).
func (s *Server) FindNode(target [32]byte) [][]byte { return s.FindNodeAt(target, s.cfg.Now()) }

// FindNodeAt is FindNode at an explicit time.
func (s *Server) FindNodeAt(target [32]byte, now time.Time) [][]byte {
	if s.cfg.Reached == nil {
		return nil
	}
	t := unixSec(now)
	var live []*objects.Card
	for _, c := range s.cfg.Reached(now) {
		if c == nil || !c.Publishable() || c.Env.NotAfter <= t {
			continue
		}
		live = append(live, c)
	}
	sort.SliceStable(live, func(i, j int) bool {
		return lessDistance(DeviceID(live[i].DeviceID), DeviceID(live[j].DeviceID), DeviceID(target))
	})
	if len(live) > MaxCardsPerAnswer {
		live = live[:MaxCardsPerAnswer]
	}
	raws := make([][]byte, len(live))
	for i, c := range live {
		raws[i] = c.Env.Raw
	}
	return trimCards(raws, MaxNodesPayload)
}

// nodesSize is an upper bound of the encoded size of a NODES/VALUE body carrying cards: the request
// id (tag + 10-byte varint) plus one length-delimited field per card.
func nodesSize(cards [][]byte) int {
	n := 11
	for _, c := range cards {
		n += 1 + varintLen(len(c)) + len(c)
	}
	return n
}

func varintLen(v int) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// trimCards drops the farthest cards (the end of the list) until the frame fits max (dht.md §4.3).
func trimCards(cards [][]byte, max int) [][]byte {
	for len(cards) > 0 && nodesSize(cards) > max {
		cards = cards[:len(cards)-1]
	}
	return cards
}

// FindValue answers FIND_VALUE: the stored record, else a cached copy, else the closer cards.
func (s *Server) FindValue(key [32]byte) ([]byte, [][]byte) {
	return s.FindValueAt(key, s.cfg.Now())
}

// FindValueAt is FindValue at an explicit time.
func (s *Server) FindValueAt(key [32]byte, now time.Time) ([]byte, [][]byte) {
	s.mu.Lock()
	s.expireLocked(now)
	if r := s.records[key]; r != nil {
		raw := r.env.Raw
		s.mu.Unlock()
		return raw, nil
	}
	if el := s.cache[key]; el != nil {
		e := el.Value.(*cacheEntry)
		if e.expiry > unixSec(now) {
			s.lru.MoveToFront(el)
			raw := e.env.Raw
			s.mu.Unlock()
			return raw, nil
		}
		s.cacheRemoveLocked(key)
	}
	s.mu.Unlock()
	return nil, s.FindNodeAt(key, now)
}

// --- popular-record cache ---

// Cache keeps a verified copy of a record that lookups keep asking for. Cached copies are served by
// FIND_VALUE but are not replicas: they do not count against any quota and never shadow a stored
// record (dht.md §3). It returns false for a record that does not pass the STORE checks.
func (s *Server) Cache(rec []byte) bool {
	now := s.cfg.Now()
	c, _ := s.check(rec, now)
	if c == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cachedAny := false
	for _, k := range c.keys {
		if _, stored := s.records[k]; stored {
			continue
		}
		if el := s.cache[k]; el != nil {
			e := el.Value.(*cacheEntry)
			if c.env.Seq < e.env.Seq || (c.env.Seq == e.env.Seq && objects.Prefer(e.env, c.env) == e.env) {
				continue
			}
			s.lru.Remove(el)
			delete(s.cache, k)
		}
		s.cache[k] = s.lru.PushFront(&cacheEntry{key: k, env: c.env, expiry: c.expiry})
		cachedAny = true
		for len(s.cache) > s.cacheMax {
			s.cacheRemoveLocked(s.lru.Back().Value.(*cacheEntry).key)
		}
	}
	return cachedAny
}

func (s *Server) cacheRemoveLocked(key [32]byte) {
	if el := s.cache[key]; el != nil {
		s.lru.Remove(el)
		delete(s.cache, key)
	}
}

// checkInvariantsErr verifies that every counter equals what the records say. Tests call it.
func (s *Server) checkInvariantsErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	signers := map[[32]byte]signerCount{}
	sessions := map[[16]byte]int{}
	units := map[string]int{}
	for k, r := range s.records {
		if r.gone || r.key != k {
			return fmt.Errorf("record %x: gone=%v", k[:4], r.gone)
		}
		c := signers[r.signer]
		if r.known {
			c.known++
		} else {
			c.unknown++
		}
		signers[r.signer] = c
		sessions[r.sid]++
		units[r.unit]++
	}
	if len(s.expq) != len(s.records) || len(s.farq) != len(s.records) {
		return fmt.Errorf("heaps %d/%d for %d records", len(s.expq), len(s.farq), len(s.records))
	}
	if len(signers) != len(s.signers) || len(sessions) != len(s.sessions) || len(units) != len(s.units) {
		return fmt.Errorf("counter maps differ: signers %d/%d sessions %d/%d units %d/%d",
			len(signers), len(s.signers), len(sessions), len(s.sessions), len(units), len(s.units))
	}
	for k, v := range signers {
		if got := s.signers[k]; got == nil || *got != v {
			return fmt.Errorf("signer %x counts %+v, want %+v", k[:4], got, v)
		}
	}
	for k, v := range sessions {
		if s.sessions[k] != v {
			return fmt.Errorf("session count %d, want %d", s.sessions[k], v)
		}
	}
	for k, v := range units {
		if s.units[k] != v {
			return fmt.Errorf("unit %s count %d, want %d", k, s.units[k], v)
		}
	}
	if len(s.cache) != s.lru.Len() || len(s.cache) > s.cacheMax {
		return fmt.Errorf("cache %d entries, list %d, max %d", len(s.cache), s.lru.Len(), s.cacheMax)
	}
	return nil
}
