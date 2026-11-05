package pex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Namespace is the store namespace of the PEX candidate tables (T4.3 namespaces are flat).
const Namespace = "v2/pex/peers"

// ConfirmWindow is how long a working session keeps a card eligible for CARDS answers
// (services.md §3: "reached ... within 24 hours").
const ConfirmWindow = 24 * time.Hour

// Table caps keep a flood of valid self-signed cards from growing the store without bound. The
// specification does not state a number; these are local policy and evict the oldest entries, never
// a tried card because of a new one.
const (
	DefaultMaxNew   = 1024
	DefaultMaxTried = 1024
)

// Record prefixes inside Namespace.
const (
	keyNew   byte = 'n'
	keyTried byte = 't'
)

// recordVersion is the first byte of a stored candidate record.
const recordVersion byte = 1

// ErrConfig is a local wiring error.
var ErrConfig = errors.New("pex: invalid configuration")

// Options configures a candidate store.
type Options struct {
	// KV is the persistent namespace (node.Host.Store(Namespace)). Required.
	KV store.KV
	// Log receives persistence warnings. Nil is silent.
	Log *slog.Logger
	// Now is the injectable clock (AD13). Nil means time.Now.
	Now func() time.Time
	// MaxNew and MaxTried override the table caps (tests).
	MaxNew   int
	MaxTried int
}

// entry is one candidate known to the node.
type entry struct {
	card *objects.Card
	// first is when the entry was first learned (unix seconds).
	first uint64
	// last is the last working session with the device (unix seconds).
	last uint64
	// reached is the last working session at an address that the card itself lists.
	reached uint64
	// addr is the canonical AddrPort of that session.
	addr netip.AddrPort
}

// Store is the addrman-style candidate table of pex/2 (services.md §3): the "new" table holds
// cards received from PEX and never reached, the "tried" table holds devices the node has reached
// in a working session. A new card is never a guard candidate. Both tables persist in the store.
//
// Its methods are safe for concurrent use.
type Store struct {
	kv       store.KV
	log      *slog.Logger
	now      func() time.Time
	maxNew   int
	maxTried int

	mu    sync.Mutex
	new   map[[32]byte]*entry
	tried map[[32]byte]*entry
}

// Open loads both tables. Cards are verified again on load with the injected clock: a damaged,
// expired or unlisted record is dropped, not trusted.
func Open(opts Options) (*Store, error) {
	if opts.KV == nil {
		return nil, fmt.Errorf("%w: no store namespace", ErrConfig)
	}
	s := &Store{
		kv:       opts.KV,
		log:      opts.Log,
		now:      opts.Now,
		maxNew:   opts.MaxNew,
		maxTried: opts.MaxTried,
		new:      map[[32]byte]*entry{},
		tried:    map[[32]byte]*entry{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxNew <= 0 {
		s.maxNew = DefaultMaxNew
	}
	if s.maxTried <= 0 {
		s.maxTried = DefaultMaxTried
	}
	if err := s.load(keyNew, s.new); err != nil {
		return nil, err
	}
	if err := s.load(keyTried, s.tried); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load(prefix byte, into map[[32]byte]*entry) error {
	now := s.now()
	var cursor []byte
	for {
		entries, err := s.kv.ScanFrom([]byte{prefix}, cursor, store.MaxScanLimit)
		if err != nil {
			return fmt.Errorf("pex: reading candidates: %w", err)
		}
		for _, rec := range entries {
			if len(rec.Key) != 1+32 {
				cursor = rec.Key
				continue
			}
			var id [32]byte
			copy(id[:], rec.Key[1:])
			e, ok := decodeRecord(rec.Value)
			if !ok {
				cursor = rec.Key
				continue
			}
			if e.card.DeviceID != id {
				cursor = rec.Key
				continue
			}
			card, err := objects.VerifyCard(e.card.Env.Raw, now)
			if err != nil || !card.Publishable() {
				cursor = rec.Key
				continue
			}
			e.card = card
			into[id] = e
			cursor = rec.Key
		}
		if len(entries) < store.MaxScanLimit {
			return nil
		}
	}
}

// AddCandidate records a card received from a PEX answer. It only ever creates or refreshes the
// "new" table: receiving a card is not reaching its device. A tried entry is never downgraded; a
// newer signed card of a known device may still refresh its envelope.
//
// It returns true when the device was new to both tables.
func (s *Store) AddCandidate(c *objects.Card, now time.Time) bool {
	if c == nil || !c.Publishable() {
		return false
	}
	nowu := unix(now)
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.tried[c.DeviceID]; ok {
		if c.Env.Seq > t.card.Env.Seq {
			t.card = c
			s.save(keyTried, t)
		}
		return false
	}
	if n, ok := s.new[c.DeviceID]; ok {
		if c.Env.Seq > n.card.Env.Seq {
			n.card = c
			s.save(keyNew, n)
		}
		return false
	}
	if len(s.new) >= s.maxNew {
		s.evictNewLocked()
	}
	e := &entry{card: c, first: nowu}
	s.new[c.DeviceID] = e
	s.save(keyNew, e)
	return true
}

// NoteSession records a working session with the device. The card moves to the tried table (a
// reached device is never a mere candidate again) and, when remote is an address listed by the card,
// the 24-hour confirmation used by CARDS is refreshed.
//
// An unlisted or non-public card removes a known device: the node must not keep forwarding it.
func (s *Store) NoteSession(c *objects.Card, remote netip.AddrPort, now time.Time) bool {
	if c == nil {
		return false
	}
	nowu := unix(now)
	s.mu.Lock()
	defer s.mu.Unlock()

	if !c.Publishable() {
		if _, ok := s.new[c.DeviceID]; ok {
			delete(s.new, c.DeviceID)
			s.drop(keyNew, c.DeviceID)
		}
		if _, ok := s.tried[c.DeviceID]; ok {
			delete(s.tried, c.DeviceID)
			s.drop(keyTried, c.DeviceID)
		}
		return false
	}

	e, ok := s.tried[c.DeviceID]
	if !ok {
		delete(s.new, c.DeviceID)
		s.drop(keyNew, c.DeviceID)
		e = &entry{card: c, first: nowu}
		s.tried[c.DeviceID] = e
		if len(s.tried) > s.maxTried {
			s.evictTriedLocked(c.DeviceID)
		}
	}
	if c.Env.Seq > e.card.Env.Seq {
		e.card = c
	}
	e.last = nowu
	if addr, ok := cardAddress(c, remote); ok {
		e.reached = nowu
		e.addr = addr
	}
	s.save(keyTried, e)
	return true
}

// Serve returns the cards the node may answer with: tried devices whose card address was reached
// in a working session within ConfirmWindow. New candidates are never served (services.md §3).
func (s *Store) Serve(now time.Time) []*objects.Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := unix(now)
	var out []*entry
	for _, e := range s.tried {
		if e.reached == 0 || e.addr.IsValid() == false {
			continue
		}
		if cutoff < e.reached || cutoff-e.reached > uint64(ConfirmWindow/time.Second) {
			continue
		}
		if _, ok := cardAddress(e.card, e.addr); !ok {
			// The card no longer lists the address that was reached; it must be reached again
			// before it is forwarded.
			continue
		}
		out = append(out, e)
	}
	sortEntries(out, func(e *entry) uint64 { return e.reached })
	cards := make([]*objects.Card, len(out))
	for i, e := range out {
		cards[i] = e.card
	}
	return cards
}

// Guards returns guard candidates: devices in the tried table only, most recently reached first
// (services.md §3: a card never yet reached is not a guard candidate).
func (s *Store) Guards(limit int) []*objects.Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*entry, 0, len(s.tried))
	for _, e := range s.tried {
		out = append(out, e)
	}
	sortEntries(out, func(e *entry) uint64 { return e.last })
	return limitCards(out, limit)
}

// DialCandidates returns cards for outgoing connections, tried before new (as Bitcoin's addrman).
// Within a table the most recently active card comes first.
func (s *Store) DialCandidates(limit int) []*objects.Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	tried := make([]*entry, 0, len(s.tried))
	for _, e := range s.tried {
		tried = append(tried, e)
	}
	sortEntries(tried, func(e *entry) uint64 { return e.last })

	fresh := make([]*entry, 0, len(s.new))
	for _, e := range s.new {
		fresh = append(fresh, e)
	}
	sortEntries(fresh, func(e *entry) uint64 { return e.first })

	out := make([]*entry, 0, len(tried)+len(fresh))
	out = append(out, tried...)
	out = append(out, fresh...)
	return limitCards(out, limit)
}

// Working returns the number of devices in the tried table: the "working peers" of the lazy PEX
// trigger (services.md §3).
func (s *Store) Working() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tried)
}

// HasRelay reports whether a tried card carries the relay flag.
func (s *Store) HasRelay() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.tried {
		if e.card.Body.GetFlags()&objects.FlagRelay != 0 {
			return true
		}
	}
	return false
}

// LenNew returns the size of the new table.
func (s *Store) LenNew() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.new)
}

// LenTried returns the size of the tried table.
func (s *Store) LenTried() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tried)
}

func limitCards(in []*entry, limit int) []*objects.Card {
	if limit <= 0 || limit > len(in) {
		limit = len(in)
	}
	out := make([]*objects.Card, limit)
	for i := 0; i < limit; i++ {
		out[i] = in[i].card
	}
	return out
}

func sortEntries(in []*entry, key func(*entry) uint64) {
	sort.Slice(in, func(i, j int) bool {
		a, b := key(in[i]), key(in[j])
		if a != b {
			return a > b
		}
		return string(in[i].card.DeviceID[:]) < string(in[j].card.DeviceID[:])
	})
}

func (s *Store) evictNewLocked() {
	var oldest *entry
	var id [32]byte
	for k, e := range s.new {
		if oldest == nil || e.first < oldest.first {
			oldest, id = e, k
		}
	}
	if oldest != nil {
		delete(s.new, id)
		s.drop(keyNew, id)
	}
}

func (s *Store) evictTriedLocked(keep [32]byte) {
	var oldest *entry
	var id [32]byte
	for k, e := range s.tried {
		if k == keep {
			continue
		}
		if oldest == nil || e.last < oldest.last {
			oldest, id = e, k
		}
	}
	if oldest != nil {
		delete(s.tried, id)
		s.drop(keyTried, id)
	}
}

// cardAddress returns the canonical address of the working session when the card lists remote.
//
// Only IP-literal addresses can be confirmed: Established.Remote is an AddrPort, so a DNS name in a
// card cannot be compared with it. Such a card stays tried but is not served until it is reached at
// an address the node can compare (the strict reading of services.md §3).
func cardAddress(c *objects.Card, remote netip.AddrPort) (netip.AddrPort, bool) {
	if c == nil || !remote.IsValid() || remote.Addr().IsUnspecified() {
		return netip.AddrPort{}, false
	}
	for _, a := range c.Addrs() {
		if a.GetHost() == "" || a.GetUrl() != "" || a.GetPort() == 0 || a.GetPort() > 65535 {
			continue
		}
		ip, err := netip.ParseAddr(a.GetHost())
		if err != nil {
			continue
		}
		if ip.Unmap() == remote.Addr().Unmap() && uint16(a.GetPort()) == remote.Port() {
			return netip.AddrPortFrom(ip.Unmap(), uint16(a.GetPort())), true
		}
	}
	return netip.AddrPort{}, false
}

func encodeRecord(e *entry) []byte {
	out := []byte{recordVersion}
	out = binary.BigEndian.AppendUint64(out, e.first)
	out = binary.BigEndian.AppendUint64(out, e.last)
	out = binary.BigEndian.AppendUint64(out, e.reached)
	addr := ""
	if e.addr.IsValid() {
		addr = e.addr.String()
	}
	out = binary.BigEndian.AppendUint16(out, uint16(len(addr)))
	out = append(out, addr...)
	out = append(out, e.card.Env.Raw...)
	return out
}

func decodeRecord(b []byte) (*entry, bool) {
	if len(b) < 1+8+8+8+2 || b[0] != recordVersion {
		return nil, false
	}
	b = b[1:]
	first := binary.BigEndian.Uint64(b[:8])
	last := binary.BigEndian.Uint64(b[8:16])
	reached := binary.BigEndian.Uint64(b[16:24])
	alen := int(binary.BigEndian.Uint16(b[24:26]))
	b = b[26:]
	if alen > len(b) {
		return nil, false
	}
	var addr netip.AddrPort
	if alen > 0 {
		a, err := netip.ParseAddrPort(string(b[:alen]))
		if err != nil {
			return nil, false
		}
		addr = a
	}
	raw := b[alen:]
	if len(raw) == 0 {
		return nil, false
	}
	// Parse only: the signature and every clock-dependent card rule are checked by Open with the
	// injected clock, which also replaces the partial card with the verified one.
	env, err := objects.Parse(raw)
	if err != nil {
		return nil, false
	}
	return &entry{
		card:    &objects.Card{Env: env, DeviceID: env.Signer.ID()},
		first:   first,
		last:    last,
		reached: reached,
		addr:    addr,
	}, true
}

func unix(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint64(s)
}

func (s *Store) save(prefix byte, e *entry) {
	if err := s.kv.Put(recordKey(prefix, e.card.DeviceID), encodeRecord(e)); err != nil && s.log != nil {
		s.log.Warn("pex: saving a candidate", "err", err)
	}
}

func (s *Store) drop(prefix byte, id [32]byte) {
	if err := s.kv.Delete(recordKey(prefix, id)); err != nil && s.log != nil {
		s.log.Warn("pex: dropping a candidate", "err", err)
	}
}

func recordKey(prefix byte, id [32]byte) []byte {
	out := make([]byte, 1+len(id))
	out[0] = prefix
	copy(out[1:], id[:])
	return out
}
