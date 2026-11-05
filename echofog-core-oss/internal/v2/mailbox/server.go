package mailbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Errors of the queue protocol. Status-level outcomes travel in DEPOSITED and do not use them.
var (
	ErrUnknownQueue = errors.New("mailbox: unknown or expired queue")
	ErrQueueExists  = errors.New("mailbox: queue already exists")
	ErrBadRequest   = errors.New("mailbox: bad request")
	ErrRefused      = errors.New("mailbox: refused by policy")
	ErrCorrupt      = errors.New("mailbox: damaged queue record")
)

// Limits are the mailbox relay's local limits. A zero field takes the value of DefaultLimits;
// a caller that needs another value sets that field.
type Limits struct {
	// Per private queue (services.md Â§5.4).
	PrivateItems int
	PrivateBytes int64
	// Per public queue (services.md Â§5.2: separate from private queues).
	PublicItems int
	PublicBytes int64
	// Whole relay. Private and public deposits have separate counters, so a flood of first
	// contacts cannot exhaust the budget of contacts' mail.
	TotalItems int
	TotalBytes int64
	// ItemMax is the largest blob in one DEPOSIT (services.md Â§5).
	ItemMax int
	// Daily quota of the node that hands deposits to this relay (F) and of its diversity unit
	// (services.md Â§5.4). The unit's quota is QuotaFactor times the node's.
	QuotaItemsPerDay int
	QuotaBytesPerDay int64
	QuotaFactor      int
	// Lifetimes.
	QueueTTL  time.Duration // at most 30 days (services.md Â§5.2)
	ItemTTL   time.Duration // unacknowledged items live 7 days (services.md Â§5.3)
	DedupeTTL time.Duration // SHA-256(sealed) memory (services.md Â§5.3)
	// One FETCH long-poll (services.md Â§5.3) and its page.
	FetchWait  time.Duration
	FetchItems int
}

// DefaultLimits returns the normative limits of services.md Â§5.
func DefaultLimits() Limits {
	return Limits{
		PrivateItems:     500,
		PrivateBytes:     10 << 20,
		PublicItems:      100,
		PublicBytes:      2 << 20,
		TotalItems:       100000,
		TotalBytes:       100 << 20,
		ItemMax:          MaxBlob,
		QuotaItemsPerDay: 5000,
		QuotaBytesPerDay: 50 << 20,
		QuotaFactor:      3,
		QueueTTL:         30 * 24 * time.Hour,
		ItemTTL:          7 * 24 * time.Hour,
		DedupeTTL:        7 * 24 * time.Hour,
		FetchWait:        MaxWait,
		FetchItems:       500,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	for _, f := range []struct {
		dst *int
		v   int
	}{
		{&l.PrivateItems, d.PrivateItems}, {&l.PublicItems, d.PublicItems},
		{&l.TotalItems, d.TotalItems}, {&l.ItemMax, d.ItemMax},
		{&l.QuotaItemsPerDay, d.QuotaItemsPerDay}, {&l.QuotaFactor, d.QuotaFactor},
		{&l.FetchItems, d.FetchItems},
	} {
		if *f.dst == 0 {
			*f.dst = f.v
		}
	}
	for _, f := range []struct {
		dst *int64
		v   int64
	}{
		{&l.PrivateBytes, d.PrivateBytes}, {&l.PublicBytes, d.PublicBytes},
		{&l.TotalBytes, d.TotalBytes}, {&l.QuotaBytesPerDay, d.QuotaBytesPerDay},
	} {
		if *f.dst == 0 {
			*f.dst = f.v
		}
	}
	for _, f := range []struct {
		dst *time.Duration
		v   time.Duration
	}{
		{&l.QueueTTL, d.QueueTTL}, {&l.ItemTTL, d.ItemTTL},
		{&l.DedupeTTL, d.DedupeTTL}, {&l.FetchWait, d.FetchWait},
	} {
		if *f.dst == 0 {
			*f.dst = f.v
		}
	}
	if l.FetchItems > MaxScanForFetch {
		l.FetchItems = MaxScanForFetch
	}
	return l
}

// MaxScanForFetch bounds one FETCH page below the store's scan ceiling.
const MaxScanForFetch = 500

// maxItemOverhead is the conservative encoded overhead of one Item (tags, lengths, id, timestamp).
const maxItemOverhead = 64

// Peer is the authenticated peer of the session a stream belongs to (02-architecture Â§3.1). The
// package does not import the node runtime.
type Peer struct {
	Node   [32]byte
	Device [32]byte
	// SID is the session id; FETCH and ACK auth is bound to it (services.md Â§5.2).
	SID [16]byte
	// Path is the kind of the active path: the diversity unit of a relayed peer is unknown.
	Path mux.PathKind
	// Remote is the carrier address of the active path when known.
	Remote netip.AddrPort
}

// Config assembles a mailbox relay.
type Config struct {
	// Self is the node ID used in FORWARD.relay (services.md Â§5.3: M recognises relay = self).
	Self [32]byte
	// HS is the node's handshake static key; forwarded deposits are sealed to it.
	HS *identity.KeyPair
	// Store is the mailbox namespace (node/modules_mailbox.go passes Host.Store(...)).
	Store store.KV
	// Now is the corrected clock; nil means time.Now.
	Now func() time.Time
	// After supplies long-poll timers; nil means time.After.
	After func(time.Duration) <-chan time.Time
	// Rand is the randomness source for item ids and the quota salt; nil means crypto/rand.
	Rand io.Reader
	// Log receives warnings; nil is silent.
	Log *slog.Logger
	// Metered is the platform flag: a metered node does not store deposits for others
	// (platforms.md Â§2).
	Metered bool
	// Limits override DefaultLimits field by field.
	Limits *Limits
}

// Server is the mailbox relay state. It is safe for concurrent use.
type Server struct {
	cfg Config
	lim Limits
	now func() time.Time
	aft func(time.Duration) <-chan time.Time
	rnd io.Reader
	log *slog.Logger
	hs  *identity.KeyPair

	salt [32]byte

	// mu serialises queue accounting, quota counters and the waiter set. Store I/O happens under
	// it; a mailbox relay's writes are small and this keeps the quotas exact without a second
	// consistency mechanism.
	mu        sync.Mutex
	waiters   map[string][]chan struct{}
	waiterNum int

	// fwdMu serialises the dedupe check and the deposit of one sealed body.
	fwdMu sync.Mutex
}

// MaxWaiters bounds the goroutines parked in long-poll FETCH, so a peer cannot hold one per
// stream indefinitely. Hitting the cap returns an empty ITEMS instead of blocking.
const (
	MaxWaiters       = 256
	MaxWaitersPerKey = 8
)

// New validates the configuration and opens (or creates) the per-profile quota salt.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.HS == nil:
		return nil, fmt.Errorf("%w: no handshake key", ErrBadRequest)
	case cfg.Store == nil:
		return nil, fmt.Errorf("%w: no store", ErrBadRequest)
	case cfg.Self == ([32]byte{}):
		return nil, fmt.Errorf("%w: no node id", ErrBadRequest)
	}
	s := &Server{
		cfg:     cfg,
		lim:     Limits{}.withDefaults(),
		now:     cfg.Now,
		aft:     cfg.After,
		rnd:     cfg.Rand,
		log:     cfg.Log,
		hs:      cfg.HS,
		waiters: map[string][]chan struct{}{},
	}
	if cfg.Limits != nil {
		s.lim = cfg.Limits.withDefaults()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.aft == nil {
		s.aft = time.After
	}
	if s.rnd == nil {
		s.rnd = rand.Reader
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	salt, err := s.loadSalt()
	if err != nil {
		return nil, err
	}
	s.salt = salt
	return s, nil
}

// Limits returns the effective limits.
func (s *Server) Limits() Limits { return s.lim }

func (s *Server) loadSalt() ([32]byte, error) {
	var salt [32]byte
	raw, err := s.cfg.Store.Get([]byte("m/salt"))
	switch {
	case err == nil:
		if len(raw) != len(salt) {
			return salt, ErrCorrupt
		}
		copy(salt[:], raw)
		return salt, nil
	case errors.Is(err, store.ErrNotFound):
		if _, err := io.ReadFull(s.rnd, salt[:]); err != nil {
			return salt, fmt.Errorf("mailbox: quota salt: %w", err)
		}
		if err := s.cfg.Store.Put([]byte("m/salt"), salt[:]); err != nil {
			return salt, err
		}
		return salt, nil
	default:
		return salt, err
	}
}

// ---- storage layout -------------------------------------------------------------------------

// Queue records are versioned; the version is the first byte.
const queueRecordVersion byte = 1

// queueRecordSize is version(1) + flags(1) + pow_bits(1) + auth_key(32) + created(8) +
// expires(8) + count(4) + bytes(8) + owner(32).
const queueRecordSize = 1 + 1 + 1 + 32 + 8 + 8 + 4 + 8 + 32

type queueRecord struct {
	Public  bool
	PowBits uint32
	AuthKey [32]byte
	Created int64
	Expires int64
	Count   uint32
	Bytes   int64
	Owner   [32]byte
}

func (r queueRecord) encode() []byte {
	b := make([]byte, queueRecordSize)
	b[0] = queueRecordVersion
	if r.Public {
		b[1] = 1
	}
	b[2] = byte(r.PowBits)
	copy(b[3:35], r.AuthKey[:])
	binary.BigEndian.PutUint64(b[35:43], uint64(r.Created))
	binary.BigEndian.PutUint64(b[43:51], uint64(r.Expires))
	binary.BigEndian.PutUint32(b[51:55], r.Count)
	binary.BigEndian.PutUint64(b[55:63], uint64(r.Bytes))
	copy(b[63:95], r.Owner[:])
	return b
}

func parseQueueRecord(b []byte) (queueRecord, error) {
	var r queueRecord
	if len(b) != queueRecordSize || b[0] != queueRecordVersion {
		return r, ErrCorrupt
	}
	r.Public = b[1]&1 != 0
	r.PowBits = uint32(b[2])
	copy(r.AuthKey[:], b[3:35])
	r.Created = int64(binary.BigEndian.Uint64(b[35:43]))
	r.Expires = int64(binary.BigEndian.Uint64(b[43:51]))
	r.Count = binary.BigEndian.Uint32(b[51:55])
	r.Bytes = int64(binary.BigEndian.Uint64(b[55:63]))
	copy(r.Owner[:], b[63:95])
	return r, nil
}

func queueKey(q []byte) []byte {
	out := make([]byte, 0, 2+len(q))
	out = append(out, "q/"...)
	return append(out, q...)
}

func itemPrefix(q []byte) []byte {
	out := make([]byte, 0, 3+len(q))
	out = append(out, "i/"...)
	out = append(out, q...)
	return append(out, '/')
}

func itemKey(q, id []byte) []byte {
	out := itemPrefix(q)
	return append(out, id...)
}

func dedupeKey(hash []byte) []byte {
	out := make([]byte, 0, 2+len(hash))
	out = append(out, "d/"...)
	return append(out, hash...)
}

func totalsKey(public bool) []byte {
	if public {
		return []byte("s/public")
	}
	return []byte("s/private")
}

func encodeItem(stored int64, blob []byte) []byte {
	b := make([]byte, 8, 8+len(blob))
	binary.BigEndian.PutUint64(b, uint64(stored))
	return append(b, blob...)
}

func parseItemValue(b []byte) (stored int64, blob []byte, err error) {
	if len(b) < 8 {
		return 0, nil, ErrCorrupt
	}
	return int64(binary.BigEndian.Uint64(b[:8])), b[8:], nil
}

type counter struct {
	Items uint64
	Bytes uint64
}

func encodeCounter(c counter) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[:8], c.Items)
	binary.BigEndian.PutUint64(b[8:], c.Bytes)
	return b
}

func parseCounter(b []byte) (counter, error) {
	if len(b) != 16 {
		return counter{}, ErrCorrupt
	}
	return counter{
		Items: binary.BigEndian.Uint64(b[:8]),
		Bytes: binary.BigEndian.Uint64(b[8:]),
	}, nil
}

// ---- operations -----------------------------------------------------------------------------

// CreateQueue creates a queue and returns its expiry in Unix seconds. The queue belongs to the
// peer that created it; its auth key is the only credential for FETCH and ACK.
func (s *Server) CreateQueue(p Peer, m QueueCreate) (uint64, error) {
	if s.cfg.Metered {
		return 0, ErrRefused
	}
	if len(m.Queue) != QueueSize {
		return 0, ErrBadRequest
	}
	auth, err := normalizeAuthKey(m.AuthKey)
	if err != nil {
		return 0, ErrBadRequest
	}
	ttl := time.Duration(m.TTL) * time.Second
	if ttl > s.lim.QueueTTL {
		return 0, ErrBadRequest
	}
	if ttl == 0 {
		ttl = s.lim.QueueTTL
	}
	powBits := m.PowBits
	if m.Public {
		if powBits == 0 {
			powBits = DefaultPowBits
		}
		if powBits > MaxPowBits {
			return 0, ErrBadRequest
		}
	}

	now := s.now()
	rec := queueRecord{
		Public:  m.Public,
		PowBits: powBits,
		Created: now.Unix(),
		Expires: now.Add(ttl).Unix(),
	}
	copy(rec.AuthKey[:], auth.Key())
	copy(rec.Owner[:], p.Node[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.cfg.Store.Get(queueKey(m.Queue)); err == nil {
		return 0, ErrQueueExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	if err := s.putQueueLocked(m.Queue, rec, now); err != nil {
		return 0, err
	}
	return uint64(rec.Expires), nil
}

// Deposit stores one blob in a queue. It returns the DEPOSITED status and, when stored, the item
// id. A proof of work below the queue's threshold is ErrPowInvalid (ERROR 0x100); a missing one is
// StatusPowRequired.
func (s *Server) Deposit(p Peer, m Deposit) (uint32, []byte, error) {
	if len(m.Queue) != QueueSize {
		return StatusUnknownQueue, nil, nil
	}
	if len(m.Blob) > s.lim.ItemMax {
		return StatusTooLarge, nil, nil
	}

	now := s.now()
	s.mu.Lock()
	rec, err := s.loadQueueLocked(m.Queue, now)
	if err != nil {
		s.mu.Unlock()
		if errors.Is(err, ErrUnknownQueue) {
			return StatusUnknownQueue, nil, nil
		}
		return 0, nil, err
	}
	if s.cfg.Metered {
		s.mu.Unlock()
		return StatusRefused, nil, nil
	}
	if rec.Public && !ValidPow(m.Queue, m.Blob, m.Pow, rec.PowBits) {
		s.mu.Unlock()
		if m.Pow == 0 {
			return StatusPowRequired, nil, nil
		}
		return StatusRefused, nil, ErrPowInvalid
	}

	items, bytes := s.queueLimits(rec)
	if int(rec.Count)+1 > items || rec.Bytes+int64(len(m.Blob)) > bytes {
		s.mu.Unlock()
		return StatusFull, nil, nil
	}
	total, err := s.totalLocked(rec.Public)
	if err != nil {
		s.mu.Unlock()
		return 0, nil, err
	}
	if int(total.Items)+1 > s.lim.TotalItems || total.Bytes+uint64(len(m.Blob)) > uint64(s.lim.TotalBytes) {
		s.mu.Unlock()
		return StatusFull, nil, nil
	}
	if !s.quotaAllowsLocked(now, p, 1, uint64(len(m.Blob))) {
		s.mu.Unlock()
		return StatusRefused, nil, nil
	}

	id := make([]byte, ItemSize)
	if _, err := io.ReadFull(s.rnd, id); err != nil {
		s.mu.Unlock()
		return 0, nil, err
	}
	if err := s.cfg.Store.PutTTL(itemKey(m.Queue, id), encodeItem(now.Unix(), m.Blob), s.lim.ItemTTL); err != nil {
		s.mu.Unlock()
		return 0, nil, err
	}
	rec.Count++
	rec.Bytes += int64(len(m.Blob))
	if err := s.putQueueLocked(m.Queue, rec, now); err != nil {
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		s.mu.Unlock()
		return 0, nil, err
	}
	if err := s.addTotalLocked(rec.Public, 1, int64(len(m.Blob))); err != nil {
		rec.Count--
		rec.Bytes -= int64(len(m.Blob))
		_ = s.putQueueLocked(m.Queue, rec, now)
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		s.mu.Unlock()
		return 0, nil, err
	}
	if err := s.quotaAddLocked(now, p, 1, uint64(len(m.Blob))); err != nil {
		_ = s.addTotalLocked(rec.Public, -1, -int64(len(m.Blob)))
		rec.Count--
		rec.Bytes -= int64(len(m.Blob))
		_ = s.putQueueLocked(m.Queue, rec, now)
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		s.mu.Unlock()
		return 0, nil, err
	}
	s.mu.Unlock()

	s.notify(string(m.Queue))
	return StatusStored, id, nil
}

// Fetch returns up to m.Max unacknowledged items after m.After, waiting for a deposit for at most
// m.Wait seconds (services.md Â§5.3). more reports that another page exists.
func (s *Server) Fetch(ctx context.Context, p Peer, m Fetch) ([]Item, bool, error) {
	if len(m.Queue) != QueueSize {
		return nil, false, ErrBadRequest
	}
	if len(m.After) != 0 && len(m.After) != ItemSize {
		return nil, false, ErrBadRequest
	}
	if err := VerifyAuth(s.authKeyOf(m.Queue), m.Auth, OpFetch, p.SID, m.Queue, m.Req); err != nil {
		return nil, false, err
	}
	if m.Max == 0 {
		return nil, false, nil
	}
	limit := int(m.Max)
	if limit > s.lim.FetchItems {
		limit = s.lim.FetchItems
	}
	wait := time.Duration(m.Wait) * time.Second
	if wait > s.lim.FetchWait {
		wait = s.lim.FetchWait
	}
	deadline := s.now().Add(wait)

	for {
		s.mu.Lock()
		rec, err := s.loadQueueLocked(m.Queue, s.now())
		if err != nil {
			s.mu.Unlock()
			return nil, false, err
		}
		items, more, err := s.itemsLocked(m.Queue, rec, m.After, limit, s.now())
		s.mu.Unlock()
		if err != nil {
			return nil, false, err
		}
		if len(items) > 0 || wait <= 0 || !s.now().Before(deadline) {
			return items, more, nil
		}

		ch, ok := s.addWaiter(string(m.Queue))
		if !ok {
			return items, more, nil
		}

		// Close the race between the scan above and registering the waiter: a deposit that
		// landed in between is visible here. The record is reloaded so that counters a
		// concurrent deposit advanced are not overwritten by the expiry housekeeping.
		s.mu.Lock()
		rec, err = s.loadQueueLocked(m.Queue, s.now())
		if err != nil {
			s.mu.Unlock()
			s.removeWaiter(string(m.Queue), ch)
			return nil, false, err
		}
		items, more, err = s.itemsLocked(m.Queue, rec, m.After, limit, s.now())
		s.mu.Unlock()
		if err != nil || len(items) > 0 {
			s.removeWaiter(string(m.Queue), ch)
			return items, more, err
		}

		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			s.removeWaiter(string(m.Queue), ch)
			return nil, false, nil
		}
		select {
		case <-ch:
		case <-s.aft(remaining):
		case <-ctx.Done():
			s.removeWaiter(string(m.Queue), ch)
			return nil, false, ctx.Err()
		}
		s.removeWaiter(string(m.Queue), ch)
	}
}

// Ack deletes the acknowledged items. Anything not acknowledged stays until its 7-day expiry
// (services.md Â§5.3). There is no answer frame for a successful ACK.
func (s *Server) Ack(p Peer, m Ack) error {
	if len(m.Queue) != QueueSize {
		return ErrBadRequest
	}
	if err := VerifyAuth(s.authKeyOf(m.Queue), m.Auth, OpAck, p.SID, m.Queue, m.Req); err != nil {
		return err
	}
	if len(m.IDs) > MaxAckIDs {
		return ErrBadRequest
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.loadQueueLocked(m.Queue, now)
	if err != nil {
		return err
	}
	var delItems, delBytes int64
	for _, id := range m.IDs {
		if len(id) != ItemSize {
			return ErrBadRequest
		}
		raw, err := s.cfg.Store.Get(itemKey(m.Queue, id))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := s.cfg.Store.Delete(itemKey(m.Queue, id)); err != nil {
			return err
		}
		delItems++
		if len(raw) > 8 {
			delBytes += int64(len(raw) - 8)
		}
	}
	if delItems == 0 {
		return nil
	}
	rec.Count = clampSub(rec.Count, uint32(delItems))
	rec.Bytes -= delBytes
	if rec.Bytes < 0 {
		rec.Bytes = 0
	}
	if err := s.putQueueLocked(m.Queue, rec, now); err != nil {
		return err
	}
	return s.addTotalLocked(rec.Public, -delItems, -delBytes)
}

// Forward handles the receiving half of a forwarded deposit (services.md Â§5.3). F addressed this
// node as M in relay; the sealed body is a DEPOSIT. Replays of the same sealed bytes are answered
// stored without a second copy for DedupeTTL.
func (s *Server) Forward(_ context.Context, p Peer, m Forward) (uint32, error) {
	if len(m.Relay) != len(s.cfg.Self) || !bytes.Equal(m.Relay, s.cfg.Self[:]) {
		return StatusRelayUnreachable, nil
	}
	if s.cfg.Metered {
		return StatusRefused, nil
	}
	plain, err := OpenForward(s.hs, m.Sealed)
	if err != nil {
		return StatusRefused, nil
	}
	dep, err := ParseDeposit(plain)
	if err != nil {
		return StatusRefused, nil
	}

	sum := sha256.Sum256(m.Sealed)
	key := dedupeKey(sum[:])

	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()
	_, err = s.cfg.Store.Get(key)
	switch {
	case err == nil:
		return StatusStored, nil
	case !errors.Is(err, store.ErrNotFound):
		return 0, err
	}

	status, _, err := s.Deposit(p, dep)
	if err != nil {
		return status, err
	}
	if status == StatusStored {
		if err := s.cfg.Store.PutTTL(key, dep.Queue, s.lim.DedupeTTL); err != nil {
			return 0, err
		}
	}
	return status, nil
}

// ---- internals ------------------------------------------------------------------------------

func (s *Server) authKeyOf(queue []byte) []byte {
	raw, err := s.cfg.Store.Get(queueKey(queue))
	if err != nil {
		return nil
	}
	rec, err := parseQueueRecord(raw)
	if err != nil {
		return nil
	}
	return rec.AuthKey[:]
}

func (s *Server) queueLimits(rec queueRecord) (int, int64) {
	if rec.Public {
		return s.lim.PublicItems, s.lim.PublicBytes
	}
	return s.lim.PrivateItems, s.lim.PrivateBytes
}

func (s *Server) loadQueueLocked(q []byte, now time.Time) (queueRecord, error) {
	raw, err := s.cfg.Store.Get(queueKey(q))
	if errors.Is(err, store.ErrNotFound) {
		return queueRecord{}, ErrUnknownQueue
	}
	if err != nil {
		return queueRecord{}, err
	}
	rec, err := parseQueueRecord(raw)
	if err != nil {
		return queueRecord{}, err
	}
	if now.Unix() >= rec.Expires {
		return queueRecord{}, ErrUnknownQueue
	}
	return rec, nil
}

func (s *Server) putQueueLocked(q []byte, rec queueRecord, now time.Time) error {
	remaining := time.Unix(rec.Expires, 0).Sub(now)
	if remaining <= 0 {
		return ErrUnknownQueue
	}
	return s.cfg.Store.PutTTL(queueKey(q), rec.encode(), remaining)
}

// itemsLocked returns one page and removes items that passed their 7-day life. The caller holds
// s.mu.
func (s *Server) itemsLocked(q []byte, rec queueRecord, after []byte, limit int, now time.Time) ([]Item, bool, error) {
	prefix := itemPrefix(q)
	var (
		entries []store.Entry
		err     error
	)
	if len(after) > 0 {
		entries, err = s.cfg.Store.ScanFrom(prefix, itemKey(q, after), limit+1)
	} else {
		entries, err = s.cfg.Store.Scan(prefix, limit+1)
	}
	if err != nil {
		return nil, false, err
	}

	items := make([]Item, 0, min(limit, len(entries)))
	var expiredItems, expiredBytes int64
	used := 0
	more := false
	for _, e := range entries {
		id := e.Key[len(prefix):]
		stored, blob, err := parseItemValue(e.Value)
		if err != nil {
			return nil, false, err
		}
		if !now.Before(time.Unix(stored, 0).Add(s.lim.ItemTTL)) {
			if err := s.cfg.Store.Delete(itemKey(q, id)); err != nil {
				return nil, false, err
			}
			expiredItems++
			expiredBytes += int64(len(blob))
			continue
		}
		if len(items) >= limit {
			more = true
			break
		}
		// Keep the encoded ITEMS message under its normative maximum (services.md §5.1). The
		// per-item overhead of the protobuf encoding is well under 64 bytes.
		if len(items) > 0 && used+len(blob)+maxItemOverhead > MaxItemsBytes {
			more = true
			break
		}
		used += len(blob) + maxItemOverhead
		items = append(items, Item{
			ID:     append([]byte(nil), id...),
			Blob:   append([]byte(nil), blob...),
			Stored: uint64(stored),
		})
	}
	// Expired records consumed part of the look-ahead window, so another page may exist beyond
	// it even when this one is not full.
	if expiredItems > 0 && len(entries) == limit+1 {
		more = true
	}
	if expiredItems > 0 {
		rec.Count = clampSub(rec.Count, uint32(expiredItems))
		rec.Bytes -= expiredBytes
		if rec.Bytes < 0 {
			rec.Bytes = 0
		}
		if err := s.putQueueLocked(q, rec, now); err != nil {
			return nil, false, err
		}
		if err := s.addTotalLocked(rec.Public, -expiredItems, -expiredBytes); err != nil {
			return nil, false, err
		}
	}
	return items, more, nil
}

func (s *Server) totalLocked(public bool) (counter, error) {
	raw, err := s.cfg.Store.Get(totalsKey(public))
	if errors.Is(err, store.ErrNotFound) {
		return counter{}, nil
	}
	if err != nil {
		return counter{}, err
	}
	return parseCounter(raw)
}

func (s *Server) addTotalLocked(public bool, items, bytes int64) error {
	c, err := s.totalLocked(public)
	if err != nil {
		return err
	}
	c.Items = uint64(max(int64(c.Items)+items, 0))
	c.Bytes = uint64(max(int64(c.Bytes)+bytes, 0))
	return s.cfg.Store.Put(totalsKey(public), encodeCounter(c))
}

// ---- daily quota ----------------------------------------------------------------------------

func (s *Server) opaque(tag string, raw []byte) []byte {
	mac := hmac.New(sha256.New, s.salt[:])
	mac.Write([]byte(tag))
	mac.Write([]byte{0})
	mac.Write(raw)
	return mac.Sum(nil)
}

func quotaKey(tag string, opaque []byte, day uint64) []byte {
	b := make([]byte, 0, 2+len(opaque)+8)
	b = append(b, tag...)
	b = append(b, '/')
	b = append(b, opaque...)
	return binary.BigEndian.AppendUint64(b, day)
}

func diversityUnit(p Peer) string {
	if p.Path != mux.PathDirect || !p.Remote.IsValid() {
		return ""
	}
	addr := p.Remote.Addr().Unmap()
	if addr.Is4() {
		if pr, err := addr.Prefix(24); err == nil {
			return pr.String()
		}
	} else if pr, err := addr.Prefix(32); err == nil {
		return pr.String()
	}
	return ""
}

func (s *Server) quotaKeys(p Peer, day uint64) (node, unit []byte) {
	node = quotaKey("u", s.opaque("mailbox-node", p.Node[:]), day)
	if u := diversityUnit(p); u != "" {
		unit = quotaKey("v", s.opaque("mailbox-network", []byte(u)), day)
	}
	return node, unit
}

func (s *Server) quotaAllowsLocked(now time.Time, p Peer, items, bytes uint64) bool {
	nodeKey, unitKey := s.quotaKeys(p, uint64(now.Unix()/86400))
	ok := func(key []byte, factor uint64) bool {
		if key == nil {
			return true
		}
		c, err := s.readQuotaLocked(key)
		if err != nil {
			return false
		}
		return c.Items+items <= uint64(s.lim.QuotaItemsPerDay)*factor &&
			c.Bytes+bytes <= uint64(s.lim.QuotaBytesPerDay)*factor
	}
	return ok(nodeKey, 1) && ok(unitKey, uint64(s.lim.QuotaFactor))
}

func (s *Server) quotaAddLocked(now time.Time, p Peer, items, bytes uint64) error {
	nodeKey, unitKey := s.quotaKeys(p, uint64(now.Unix()/86400))
	for _, key := range [][]byte{nodeKey, unitKey} {
		if key == nil {
			continue
		}
		c, err := s.readQuotaLocked(key)
		if err != nil {
			return err
		}
		c.Items += items
		c.Bytes += bytes
		if err := s.cfg.Store.PutTTL(key, encodeCounter(c), 2*24*time.Hour); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) readQuotaLocked(key []byte) (counter, error) {
	raw, err := s.cfg.Store.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return counter{}, nil
	}
	if err != nil {
		return counter{}, err
	}
	return parseCounter(raw)
}

// ---- long-poll waiters ----------------------------------------------------------------------

func (s *Server) addWaiter(queue string) (chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiterNum >= MaxWaiters || len(s.waiters[queue]) >= MaxWaitersPerKey {
		return nil, false
	}
	ch := make(chan struct{})
	s.waiters[queue] = append(s.waiters[queue], ch)
	s.waiterNum++
	return ch, true
}

func (s *Server) removeWaiter(queue string, ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.waiters[queue]
	for i, c := range list {
		if c == ch {
			s.waiters[queue] = append(list[:i], list[i+1:]...)
			s.waiterNum--
			break
		}
	}
	if len(s.waiters[queue]) == 0 {
		delete(s.waiters, queue)
	}
}

func (s *Server) notify(queue string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.waiters[queue] {
		close(ch)
	}
	s.waiterNum -= len(s.waiters[queue])
	delete(s.waiters, queue)
}

func clampSub(v uint32, n uint32) uint32 {
	if n >= v {
		return 0
	}
	return v - n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
