package mailbox

import (
	"bytes"
	"context"
	"crypto/hkdf"
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
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

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
	// Per private queue (services.md §5.4).
	PrivateItems int
	PrivateBytes int64
	// Per public queue (services.md §5.2: separate from private queues).
	PublicItems int
	PublicBytes int64
	// Whole relay. Private and public deposits have separate counters, so a flood of first
	// contacts cannot exhaust the budget of contacts' mail.
	TotalItems int
	TotalBytes int64
	// ItemMax is the largest blob in one DEPOSIT (services.md §5).
	ItemMax int
	// QueuesPerNode bounds empty queue records one node may keep at once. The spec has per-queue
	// and total item limits but no queue-count limit; without this an authenticated peer could
	// fill the store with small records.
	QueuesPerNode int
	// Daily quota of the node that hands deposits to this relay (F) and of its diversity unit
	// (services.md §5.4). The unit's quota is QuotaFactor times the node's.
	QuotaItemsPerDay int
	QuotaBytesPerDay int64
	QuotaFactor      int
	// Lifetimes.
	QueueTTL  time.Duration // at most 30 days (services.md §5.2)
	ItemTTL   time.Duration // unacknowledged items live 7 days (services.md §5.3)
	DedupeTTL time.Duration // SHA-256(sealed) memory (services.md §5.3)
	// One FETCH long-poll (services.md §5.3) and its page.
	FetchWait  time.Duration
	FetchItems int
}

// DefaultLimits returns the normative limits of services.md §5.
func DefaultLimits() Limits {
	return Limits{
		PrivateItems:     500,
		PrivateBytes:     10 << 20,
		PublicItems:      100,
		PublicBytes:      2 << 20,
		TotalItems:       100000,
		TotalBytes:       100 << 20,
		ItemMax:          MaxBlob,
		QueuesPerNode:    64,
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
		{&l.QueuesPerNode, d.QueuesPerNode},
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

// defaultFetchMax is what FETCH.max = 0 asks for: a useful page, not an empty one
// (decisions.md T7.3 fix 3). It is still capped by Limits.FetchItems.
const defaultFetchMax = 32

// fetchScanChunk bounds how many item values one store scan materialises. The page is assembled
// chunk by chunk so a 160-byte FETCH cannot read tens of MiB of blobs before the 1 MiB page
// limit applies (fix 6). The look-ahead chunk is the reserve above the page budget.
const fetchScanChunk = 8

// pruneBatch bounds one scan of the expiry index. Pruning stops at the first live entry, so the
// usual cost of a deposit is one small index record.
const pruneBatch = 128

// MaxQueueDays is the queue lifetime unit (services.md §5.2): QUEUE_CREATE.ttl is in days.
const MaxQueueDays = 30

// storeGrace is how far past its logical lifetime the store may keep a record. The store's TTL is
// only a backstop against garbage (AD19): every lifetime that matters is in the E/ index, and the
// counters never depend on the store dropping a record.
const storeGrace = 24 * time.Hour

// maxItemOverhead is the conservative encoded overhead of one Item (tags, lengths, id, timestamp).
const maxItemOverhead = 64

// Peer is the authenticated peer of the session a stream belongs to (02-architecture §3.1). The
// package does not import the node runtime.
type Peer struct {
	Node   [32]byte
	Device [32]byte
	// SID is the session id; FETCH and ACK auth is bound to it (services.md §5.2).
	SID [16]byte
	// Path is the kind of the active path: the diversity unit of a relayed peer is unknown.
	Path mux.PathKind
	// Remote is the carrier address of the active path when known.
	Remote netip.AddrPort
}

// Config assembles a mailbox relay.
type Config struct {
	// Self is the node ID used in FORWARD.relay (services.md §5.3: M recognises relay = self).
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
	// (platforms.md §2).
	Metered bool
	// Limits override DefaultLimits field by field.
	Limits *Limits
	// SelfDevice is this relay's device ID. hints name a relay by device ID, so FORWARD.relay may be
	// the node ID (Self) or the device ID.
	SelfDevice [32]byte
	// Forwarder makes this node a forwarding relay F (services.md 5.3): a FORWARD for another relay
	// is passed on. Nil: such a FORWARD is answered relay-unreachable.
	Forwarder Forwarder
}

// Forwarder carries a FORWARD to the mailbox relay it names, over a session of this node.
type Forwarder interface {
	// Connected reports whether a session with the relay exists, so that no dial is needed.
	Connected(relay []byte) bool
	// Forward sends m to the relay (dialing it only with a card F verified itself or a relay_card signed
	// by the relay, never to a private or reserved address) and returns the status M answered.
	Forward(ctx context.Context, m Forward) (uint32, error)
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

	// underflows counts deductions that would have taken a counter below zero. The counters are
	// clamped, but each one means an item was deducted without being found (AD19).
	underflows atomic.Uint64

	salt [32]byte

	// mu serialises queue accounting, quota counters and the waiter set. Store I/O happens under
	// it; a mailbox relay's writes are small and this keeps the quotas exact without a second
	// consistency mechanism.
	mu        sync.Mutex
	waiters   map[string][]chan struct{}
	waiterNum int

	// fwdMu serialises the dedupe check and the deposit of one sealed body.
	fwdMu sync.Mutex

	// dials counts the new connections F made for each sender session in the last minute.
	dials map[[16]byte][]time.Time
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
		dials:   map[[16]byte][]time.Time{},
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

// Queue records are versioned; the version is the first byte. Version 2 keeps a hash of the
// queue's auth key and the key itself sealed under the profile salt, never the raw key bytes.
const (
	queueRecordVersion       byte = 2
	queueRecordVersionLegacy byte = 1
	authKeySealedSize             = chacha20poly1305.NonceSize + 32 + chacha20poly1305.Overhead
)

// queueRecordSize is version(1) + flags(1) + pow_bits(1) + auth_id(32) + auth_sealed(60) +
// created(8) + expires(8) + count(4) + bytes(8) + owner(32).
const queueRecordSize = 1 + 1 + 1 + 32 + authKeySealedSize + 8 + 8 + 4 + 8 + 32

// queueRecordLegacySize is the read-only layout written before the fix round: it carried the
// raw auth_key. Records of this layout are accepted and rewritten as version 2.
const queueRecordLegacySize = 1 + 1 + 1 + 32 + 8 + 8 + 4 + 8 + 32

type queueRecord struct {
	Public    bool
	PowBits   uint32
	AuthKeyID [32]byte
	// AuthSealed is ChaCha20-Poly1305(nonce, key = HKDF(profile salt, queue), aad = queue) over
	// the 32-byte raw auth key. The store sees a verifier and a sealed binding, not the key.
	AuthSealed [authKeySealedSize]byte
	Created    int64
	Expires    int64
	Count      uint32
	Bytes      int64
	Owner      [32]byte

	// legacyAuthKey is filled only when a version 1 record is read; putQueueLocked re-seals it.
	legacyAuthKey [32]byte
}

func (r queueRecord) encode() []byte {
	b := make([]byte, queueRecordSize)
	b[0] = queueRecordVersion
	if r.Public {
		b[1] = 1
	}
	b[2] = byte(r.PowBits)
	copy(b[3:35], r.AuthKeyID[:])
	copy(b[35:35+authKeySealedSize], r.AuthSealed[:])
	off := 35 + authKeySealedSize
	binary.BigEndian.PutUint64(b[off:off+8], uint64(r.Created))
	binary.BigEndian.PutUint64(b[off+8:off+16], uint64(r.Expires))
	binary.BigEndian.PutUint32(b[off+16:off+20], r.Count)
	binary.BigEndian.PutUint64(b[off+20:off+28], uint64(r.Bytes))
	copy(b[off+28:off+60], r.Owner[:])
	return b
}

func parseQueueRecord(b []byte) (queueRecord, error) {
	var r queueRecord
	switch {
	case len(b) == queueRecordSize && b[0] == queueRecordVersion:
		r.Public = b[1]&1 != 0
		r.PowBits = uint32(b[2])
		copy(r.AuthKeyID[:], b[3:35])
		copy(r.AuthSealed[:], b[35:35+authKeySealedSize])
		off := 35 + authKeySealedSize
		r.Created = int64(binary.BigEndian.Uint64(b[off : off+8]))
		r.Expires = int64(binary.BigEndian.Uint64(b[off+8 : off+16]))
		r.Count = binary.BigEndian.Uint32(b[off+16 : off+20])
		r.Bytes = int64(binary.BigEndian.Uint64(b[off+20 : off+28]))
		copy(r.Owner[:], b[off+28:off+60])
		return r, nil
	case len(b) == queueRecordLegacySize && b[0] == queueRecordVersionLegacy:
		r.Public = b[1]&1 != 0
		r.PowBits = uint32(b[2])
		copy(r.legacyAuthKey[:], b[3:35])
		r.AuthKeyID = sha256.Sum256(r.legacyAuthKey[:])
		r.Created = int64(binary.BigEndian.Uint64(b[35:43]))
		r.Expires = int64(binary.BigEndian.Uint64(b[43:51]))
		r.Count = binary.BigEndian.Uint32(b[51:55])
		r.Bytes = int64(binary.BigEndian.Uint64(b[55:63]))
		copy(r.Owner[:], b[63:95])
		return r, nil
	default:
		return r, ErrCorrupt
	}
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

// The expiry index is sorted by expiry time first: one scan from "E/" walks every entry that has
// passed its logical lifetime, so the relay counters never depend on badger's TTL deleting an
// item before someone looks at it (decisions.md T7.3 fix 1).
const expiryIndexPrefix = "E/"

func expiryKey(q, id []byte, expires int64, public bool) []byte {
	b := make([]byte, 0, 2+8+1+len(q)+len(id))
	b = append(b, expiryIndexPrefix...)
	b = binary.BigEndian.AppendUint64(b, uint64(expires))
	if public {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = append(b, q...)
	return append(b, id...)
}

// queueExpiryKey is the index entry of a whole queue: flag bit 1, zero item id.
func queueExpiryKey(q []byte, expires int64, public bool) []byte {
	b := expiryKey(q, make([]byte, ItemSize), expires, public)
	b[10] |= 2
	return b
}

func parseExpiryKey(b []byte) (expires int64, public, isQueue bool, q, id []byte, err error) {
	const fixed = 2 + 8 + 1
	if len(b) != fixed+QueueSize+ItemSize || !bytes.HasPrefix(b, []byte(expiryIndexPrefix)) {
		return 0, false, false, nil, nil, ErrCorrupt
	}
	expires = int64(binary.BigEndian.Uint64(b[2:10]))
	public = b[10]&1 != 0
	isQueue = b[10]&2 != 0
	q = b[fixed : fixed+QueueSize]
	id = b[fixed+QueueSize:]
	return expires, public, isQueue, q, id, nil
}

func expiryIndexValue(size int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(size))
	return b
}

func parseExpiryIndexValue(b []byte) (int64, error) {
	if len(b) != 8 {
		return 0, ErrCorrupt
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func dedupeKey(hash []byte) []byte {
	out := make([]byte, 0, 2+len(hash))
	out = append(out, "d/"...)
	return append(out, hash...)
}

func ownerPrefix(opaque []byte) []byte {
	out := make([]byte, 0, 2+len(opaque)+1)
	out = append(out, "o/"...)
	out = append(out, opaque...)
	return append(out, '/')
}

func ownerKey(opaque, q []byte) []byte {
	out := ownerPrefix(opaque)
	return append(out, q...)
}

func totalsKey(public bool) []byte {
	if public {
		return []byte("s/public")
	}
	return []byte("s/private")
}

// itemValueVersion marks the nanosecond layout. Version 0 is the old 8-byte Unix-seconds layout;
// its first byte is zero at current dates, so the two are unambiguous.
const itemValueVersion byte = 2

func encodeItem(stored int64, blob []byte) []byte {
	b := make([]byte, 9, 9+len(blob))
	b[0] = itemValueVersion
	binary.BigEndian.PutUint64(b[1:9], uint64(stored))
	return append(b, blob...)
}

// parseItemValue returns the internal stored time in Unix nanoseconds.
func parseItemValue(b []byte) (stored int64, blob []byte, err error) {
	if len(b) >= 9 && b[0] == itemValueVersion {
		return int64(binary.BigEndian.Uint64(b[1:9])), b[9:], nil
	}
	if len(b) >= 8 {
		return int64(binary.BigEndian.Uint64(b[:8])) * int64(time.Second), b[8:], nil
	}
	return 0, nil, ErrCorrupt
}

func itemBlobLen(raw []byte) int64 {
	if len(raw) >= 9 && raw[0] == itemValueVersion {
		return int64(len(raw) - 9)
	}
	if len(raw) > 8 {
		return int64(len(raw) - 8)
	}
	return 0
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
	if len(m.Queue) != QueueSize {
		return 0, ErrBadRequest
	}
	auth, err := normalizeAuthKey(m.AuthKey)
	if err != nil {
		return 0, ErrBadRequest
	}
	if m.TTL > MaxQueueDays {
		return 0, ErrBadRequest
	}
	ttl := s.lim.QueueTTL
	if m.TTL > 0 {
		ttl = time.Duration(m.TTL) * 24 * time.Hour
		if ttl > s.lim.QueueTTL {
			return 0, ErrBadRequest
		}
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
	copy(rec.legacyAuthKey[:], auth.Key())
	copy(rec.Owner[:], p.Node[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.cfg.Store.Get(queueKey(m.Queue)); err == nil {
		return 0, ErrQueueExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	owner := s.opaque("mailbox-owner", p.Node[:])
	owned, err := s.cfg.Store.Scan(ownerPrefix(owner), s.lim.QueuesPerNode+1)
	if err != nil {
		return 0, err
	}
	if len(owned) >= s.lim.QueuesPerNode {
		return 0, ErrRefused
	}
	if err := s.putQueueLocked(m.Queue, rec, now); err != nil {
		return 0, err
	}
	remaining := time.Unix(rec.Expires, 0).Sub(now) + storeGrace
	if err := s.cfg.Store.PutTTL(ownerKey(owner, m.Queue), []byte{1}, remaining); err != nil {
		_ = s.cfg.Store.Delete(queueKey(m.Queue))
		_ = s.cfg.Store.Delete(queueExpiryKey(m.Queue, rec.Expires*int64(time.Second), rec.Public))
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
	if err := s.pruneExpiredLocked(now); err != nil {
		s.mu.Unlock()
		return 0, nil, err
	}
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
	stored := now.UnixNano()
	expires := s.itemExpiry(stored)
	itemStoreTTL := s.lim.ItemTTL + storeGrace
	if err := s.cfg.Store.PutTTL(itemKey(m.Queue, id), encodeItem(stored, m.Blob), itemStoreTTL); err != nil {
		s.mu.Unlock()
		return 0, nil, err
	}
	indexKey := expiryKey(m.Queue, id, expires, rec.Public)
	if err := s.cfg.Store.Put(indexKey, expiryIndexValue(int64(len(m.Blob)))); err != nil {
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		s.mu.Unlock()
		return 0, nil, err
	}
	rec.Count++
	rec.Bytes += int64(len(m.Blob))
	if err := s.putQueueLocked(m.Queue, rec, now); err != nil {
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		_ = s.cfg.Store.Delete(indexKey)
		s.mu.Unlock()
		return 0, nil, err
	}
	if err := s.addTotalLocked(rec.Public, 1, int64(len(m.Blob))); err != nil {
		rec.Count--
		rec.Bytes -= int64(len(m.Blob))
		_ = s.putQueueLocked(m.Queue, rec, now)
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		_ = s.cfg.Store.Delete(indexKey)
		s.mu.Unlock()
		return 0, nil, err
	}
	if err := s.quotaAddLocked(now, p, 1, uint64(len(m.Blob))); err != nil {
		_ = s.addTotalLocked(rec.Public, -1, -int64(len(m.Blob)))
		rec.Count--
		rec.Bytes -= int64(len(m.Blob))
		_ = s.putQueueLocked(m.Queue, rec, now)
		_ = s.cfg.Store.Delete(itemKey(m.Queue, id))
		_ = s.cfg.Store.Delete(indexKey)
		s.mu.Unlock()
		return 0, nil, err
	}
	s.mu.Unlock()

	s.notify(string(m.Queue))
	return StatusStored, id, nil
}

// Fetch returns up to m.Max unacknowledged items after m.After, waiting for a deposit for at most
// m.Wait seconds (services.md §5.3). more reports that another page exists.
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
	limit := int(m.Max)
	if limit == 0 {
		limit = defaultFetchMax
	}
	if limit > s.lim.FetchItems {
		limit = s.lim.FetchItems
	}
	wait := time.Duration(m.Wait) * time.Second
	if wait > s.lim.FetchWait {
		wait = s.lim.FetchWait
	}
	deadline := s.now().Add(wait)

	for {
		now := s.now()
		s.mu.Lock()
		if err := s.pruneExpiredLocked(now); err != nil {
			s.mu.Unlock()
			return nil, false, err
		}
		rec, err := s.loadQueueLocked(m.Queue, now)
		if err != nil {
			s.mu.Unlock()
			return nil, false, err
		}
		items, more, err := s.itemsLocked(m.Queue, rec, m.After, limit, now)
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
		now = s.now()
		s.mu.Lock()
		if err := s.pruneExpiredLocked(now); err != nil {
			s.mu.Unlock()
			s.removeWaiter(string(m.Queue), ch)
			return nil, false, err
		}
		rec, err = s.loadQueueLocked(m.Queue, now)
		if err != nil {
			s.mu.Unlock()
			s.removeWaiter(string(m.Queue), ch)
			return nil, false, err
		}
		items, more, err = s.itemsLocked(m.Queue, rec, m.After, limit, now)
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
		if remaining > s.lim.FetchWait {
			remaining = s.lim.FetchWait
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
// (services.md §5.3). There is no answer frame for a successful ACK.
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
	if err := s.pruneExpiredLocked(now); err != nil {
		return err
	}
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
		stored, _, err := parseItemValue(raw)
		if err != nil {
			return err
		}
		if err := s.cfg.Store.Delete(itemKey(m.Queue, id)); err != nil {
			return err
		}
		if err := s.cfg.Store.Delete(expiryKey(m.Queue, id, s.itemExpiry(stored), rec.Public)); err != nil {
			return err
		}
		delItems++
		delBytes += itemBlobLen(raw)
	}
	if delItems == 0 {
		return nil
	}
	s.subRecord(&rec, delItems, delBytes)
	if err := s.putQueueLocked(m.Queue, rec, now); err != nil {
		return err
	}
	return s.addTotalLocked(rec.Public, -delItems, -delBytes)
}

// Forward handles the receiving half of a forwarded deposit (services.md §5.3). F addressed this
// node as M in relay; the sealed body is a DEPOSIT. Replays of the same sealed bytes are answered
// stored without a second copy for DedupeTTL.
func (s *Server) Forward(ctx context.Context, p Peer, m Forward) (uint32, error) {
	if !s.isSelf(m.Relay) {
		return s.forwardOn(ctx, p, m)
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

// isSelf reports whether a FORWARD names this relay: by node ID, or by device ID as hints do.
func (s *Server) isSelf(relay []byte) bool {
	if len(relay) != 32 {
		return false
	}
	if bytes.Equal(relay, s.cfg.Self[:]) {
		return true
	}
	return s.cfg.SelfDevice != ([32]byte{}) && bytes.Equal(relay, s.cfg.SelfDevice[:])
}

// Limits of the forwarding role (services.md 5.3).
const (
	// MaxDialsPerMinute is how many new connections F makes for one sender session in a minute.
	MaxDialsPerMinute = 10
	// ForwardTimeout is the time F gives a forwarded deposit (services.md 5.1).
	ForwardTimeout = 20 * time.Second
)

// forwardOn is the role F: pass a FORWARD on to the mailbox relay it names. F cannot read the sealed
// deposit. The sender's quota per day (node ID and unit) is checked first and spent whatever M
// answers; a new connection costs one of ten dials a minute for the sender's session.
func (s *Server) forwardOn(ctx context.Context, p Peer, m Forward) (uint32, error) {
	if s.cfg.Forwarder == nil || len(m.Relay) != 32 {
		return StatusRelayUnreachable, nil
	}
	if s.cfg.Metered {
		return StatusRefused, nil
	}
	now := s.now()
	connected := s.cfg.Forwarder.Connected(m.Relay)

	s.mu.Lock()
	if !s.quotaAllowsLocked(now, p, 1, uint64(len(m.Sealed))) {
		s.mu.Unlock()
		return StatusRefused, nil
	}
	if !connected {
		for sid, ts := range s.dials {
			kept := ts[:0:0]
			for _, t := range ts {
				if now.Sub(t) < time.Minute {
					kept = append(kept, t)
				}
			}
			if len(kept) == 0 {
				delete(s.dials, sid)
			} else {
				s.dials[sid] = kept
			}
		}
		ts := s.dials[p.SID]
		if len(ts) >= MaxDialsPerMinute || (len(ts) == 0 && len(s.dials) >= MaxWaiters*16) {
			s.mu.Unlock()
			return StatusRefused, nil
		}
		s.dials[p.SID] = append(ts, now)
	}
	if err := s.quotaAddLocked(now, p, 1, uint64(len(m.Sealed))); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Unlock()

	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		st  uint32
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, err := s.cfg.Forwarder.Forward(fctx, m)
		done <- result{st, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return StatusRelayUnreachable, nil
		}
		return r.st, nil
	case <-s.aft(ForwardTimeout):
		cancel()
		return StatusRelayUnreachable, nil
	case <-ctx.Done():
		return StatusRelayUnreachable, nil
	}
}

// ---- internals ------------------------------------------------------------------------------

const authKeyDeriveLabel = "echofog/mailbox2/queue-auth"

// authKeyCipherKey binds the sealed auth key to its queue and to this profile's salt.
func (s *Server) authKeyCipherKey(queue []byte) ([]byte, error) {
	info := make([]byte, 0, len(authKeyDeriveLabel)+len(queue))
	info = append(info, authKeyDeriveLabel...)
	info = append(info, queue...)
	key, err := hkdf.Key(sha256.New, s.salt[:], nil, string(info), chacha20poly1305.KeySize)
	if err != nil {
		return nil, fmt.Errorf("mailbox: auth key KDF: %w", err)
	}
	return key, nil
}

// sealAuthKey replaces the raw key with its SHA-256 id and a sealed copy under the profile salt.
// The store already encrypts values; this keeps the queue record itself from carrying the key
// bytes that decisions.md T7.3 fix 7 says not to store.
func (s *Server) sealAuthKey(queue []byte, rec *queueRecord) error {
	if rec.AuthSealed != ([authKeySealedSize]byte{}) {
		return nil
	}
	raw := rec.legacyAuthKey
	if raw == ([32]byte{}) {
		return ErrCorrupt
	}
	key, err := s.authKeyCipherKey(queue)
	if err != nil {
		return err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return fmt.Errorf("mailbox: auth key AEAD: %w", err)
	}
	nonce := rec.AuthSealed[:chacha20poly1305.NonceSize]
	if _, err := io.ReadFull(s.rnd, nonce); err != nil {
		return err
	}
	sealed := aead.Seal(nonce, nonce, raw[:], queue)
	if len(sealed) != authKeySealedSize {
		return ErrCorrupt
	}
	rec.AuthKeyID = sha256.Sum256(raw[:])
	return nil
}

func (s *Server) openAuthKey(queue []byte, rec queueRecord) []byte {
	if rec.AuthSealed != ([authKeySealedSize]byte{}) {
		key, err := s.authKeyCipherKey(queue)
		if err != nil {
			return nil
		}
		aead, err := chacha20poly1305.New(key)
		if err != nil {
			return nil
		}
		nonce := rec.AuthSealed[:chacha20poly1305.NonceSize]
		raw, err := aead.Open(nil, nonce, rec.AuthSealed[chacha20poly1305.NonceSize:], queue)
		if err != nil || len(raw) != 32 || sha256.Sum256(raw) != rec.AuthKeyID {
			return nil
		}
		return raw
	}
	if rec.legacyAuthKey != ([32]byte{}) {
		return rec.legacyAuthKey[:]
	}
	return nil
}

func (s *Server) authKeyOf(queue []byte) []byte {
	raw, err := s.cfg.Store.Get(queueKey(queue))
	if err != nil {
		return nil
	}
	rec, err := parseQueueRecord(raw)
	if err != nil {
		return nil
	}
	return s.openAuthKey(queue, rec)
}

func (s *Server) queueLimits(rec queueRecord) (int, int64) {
	if rec.Public {
		return s.lim.PublicItems, s.lim.PublicBytes
	}
	return s.lim.PrivateItems, s.lim.PrivateBytes
}

func (s *Server) itemExpiry(stored int64) int64 {
	return stored + int64(s.lim.ItemTTL)
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
		if err := s.expireQueueLocked(q, rec, now); err != nil {
			return queueRecord{}, err
		}
		return queueRecord{}, ErrUnknownQueue
	}
	return rec, nil
}

// expireQueueLocked removes an expired queue record and whatever it still holds. Items are
// normally gone by their 7-day life, but a queue created with a shorter ttl must not leave its
// blobs counted against the relay until the slower item expiry catches up.
func (s *Server) expireQueueLocked(q []byte, rec queueRecord, now time.Time) error {
	prefix := itemPrefix(q)
	var (
		start    []byte
		delItems int64
		delBytes int64
	)
	for {
		entries, err := s.cfg.Store.ScanFrom(prefix, start, 64)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			start = e.Key
			id := e.Key[len(prefix):]
			stored, blob, err := parseItemValue(e.Value)
			if err != nil {
				return err
			}
			if err := s.cfg.Store.Delete(itemKey(q, id)); err != nil {
				return err
			}
			_ = s.cfg.Store.Delete(expiryKey(q, id, s.itemExpiry(stored), rec.Public))
			delItems++
			delBytes += int64(len(blob))
		}
		if len(entries) < 64 {
			break
		}
	}
	if delItems > 0 {
		if err := s.addTotalLocked(rec.Public, -delItems, -delBytes); err != nil {
			return err
		}
	}
	_ = s.cfg.Store.Delete(queueKey(q))
	_ = s.cfg.Store.Delete(queueExpiryKey(q, rec.Expires*int64(time.Second), rec.Public))
	owner := s.opaque("mailbox-owner", rec.Owner[:])
	_ = s.cfg.Store.Delete(ownerKey(owner, q))
	return nil
}

// putQueueLocked writes the queue record. Its logical lifetime is the E/ entry written by
// CreateQueue; the store keeps the record one grace period longer as a backstop only.
func (s *Server) putQueueLocked(q []byte, rec queueRecord, now time.Time) error {
	if rec.AuthSealed == ([authKeySealedSize]byte{}) {
		// A v1 record has no queue entry in the index yet; add it while migrating.
		if err := s.cfg.Store.Put(queueExpiryKey(q, rec.Expires*int64(time.Second), rec.Public), expiryIndexValue(0)); err != nil {
			return err
		}
	}
	if err := s.sealAuthKey(q, &rec); err != nil {
		return err
	}
	remaining := time.Unix(rec.Expires, 0).Sub(now) + storeGrace
	if remaining < storeGrace {
		// A queue past its lifetime whose entry the index has not reached yet (a pass that
		// settles item shares first): keep the record until that entry removes it.
		remaining = storeGrace
	}
	return s.cfg.Store.PutTTL(queueKey(q), rec.encode(), remaining)
}

type expiryDelta struct {
	queue  []byte
	public bool
	items  int64
	bytes  int64
}

// pruneExpiredLocked walks the expiry index from the oldest entry and releases every item whose
// logical lifetime has passed. It stops at the first live entry, so an idle queue costs one small
// scan. The caller holds s.mu.
func (s *Server) pruneExpiredLocked(now time.Time) error {
	nowNanos := now.UnixNano()
	var cursor []byte
	for {
		entries, err := s.cfg.Store.ScanFrom([]byte(expiryIndexPrefix), cursor, pruneBatch)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		var (
			deltas []*expiryDelta
			byKey  = make(map[string]*expiryDelta)
			live   bool
		)
		for _, e := range entries {
			expires, public, isQueue, q, id, err := parseExpiryKey(e.Key)
			if err != nil {
				return err
			}
			if expires > nowNanos {
				live = true
				break
			}
			if isQueue {
				if err := s.applyExpiryDeltasLocked(deltas, now); err != nil {
					return err
				}
				deltas, byKey = nil, make(map[string]*expiryDelta)
				if err := s.expireQueueEntryLocked(q, expires, e.Key, now); err != nil {
					return err
				}
				cursor = append(cursor[:0], e.Key...)
				continue
			}
			// The snapshot may be stale: a queue entry earlier in this pass removes its items'
			// entries. Read the entry again; no entry found means no deduction.
			fresh, err := s.cfg.Store.Get(e.Key)
			if errors.Is(err, store.ErrNotFound) {
				cursor = append(cursor[:0], e.Key...)
				continue
			}
			if err != nil {
				return err
			}
			size, err := parseExpiryIndexValue(fresh)
			if err != nil {
				return err
			}
			if err := s.cfg.Store.Delete(itemKey(q, id)); err != nil {
				return err
			}
			if err := s.cfg.Store.Delete(e.Key); err != nil {
				return err
			}
			key := string(q)
			d := byKey[key]
			if d == nil {
				d = &expiryDelta{queue: append([]byte(nil), q...), public: public}
				byKey[key] = d
				deltas = append(deltas, d)
			}
			d.items++
			d.bytes += size
			cursor = append(cursor[:0], e.Key...)
		}
		if err := s.applyExpiryDeltasLocked(deltas, now); err != nil {
			return err
		}
		if live || len(entries) < pruneBatch {
			return nil
		}
	}
}

// expireQueueEntryLocked processes one queue entry of the index: the queue leaves with its items
// and their share of the counters. An entry that no longer matches the record is stale and is
// only dropped.
func (s *Server) expireQueueEntryLocked(q []byte, expires int64, indexKey []byte, now time.Time) error {
	raw, err := s.cfg.Store.Get(queueKey(q))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s.cfg.Store.Delete(indexKey)
	case err != nil:
		return err
	}
	rec, err := parseQueueRecord(raw)
	if err != nil {
		return err
	}
	if rec.Expires*int64(time.Second) != expires {
		return s.cfg.Store.Delete(indexKey)
	}
	return s.expireQueueLocked(q, rec, now)
}

func (s *Server) applyExpiryDeltasLocked(deltas []*expiryDelta, now time.Time) error {
	for _, d := range deltas {
		if err := s.addTotalLocked(d.public, -d.items, -d.bytes); err != nil {
			return err
		}
		raw, err := s.cfg.Store.Get(queueKey(d.queue))
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return err
		}
		rec, err := parseQueueRecord(raw)
		if err != nil {
			return err
		}
		s.subRecord(&rec, d.items, d.bytes)
		if err := s.putQueueLocked(d.queue, rec, now); err != nil {
			return err
		}
	}
	return nil
}

// itemsLocked returns one page and reaps expired records left by an older layout. Live expiry is
// handled by pruneExpiredLocked. The caller holds s.mu.
func (s *Server) itemsLocked(q []byte, rec queueRecord, after []byte, limit int, now time.Time) ([]Item, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	prefix := itemPrefix(q)
	start := []byte(nil)
	if len(after) > 0 {
		start = itemKey(q, after)
	}

	items := make([]Item, 0, min(limit, fetchScanChunk))
	var expiredItems, expiredBytes int64
	used := 0
	more := false
	live := 0
	want := limit + 1

scan:
	for {
		n := want - live
		if n > fetchScanChunk {
			n = fetchScanChunk
		}
		if n <= 0 {
			break
		}
		entries, err := s.cfg.Store.ScanFrom(prefix, start, n)
		if err != nil {
			return nil, false, err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			start = e.Key
			id := e.Key[len(prefix):]
			stored, blob, err := parseItemValue(e.Value)
			if err != nil {
				return nil, false, err
			}
			expires := s.itemExpiry(stored)
			if now.UnixNano() >= expires {
				if err := s.cfg.Store.Delete(itemKey(q, id)); err != nil {
					return nil, false, err
				}
				_ = s.cfg.Store.Delete(expiryKey(q, id, expires, rec.Public))
				expiredItems++
				expiredBytes += int64(len(blob))
				continue
			}
			if live >= limit {
				more = true
				break scan
			}
			// Keep the encoded ITEMS message under its normative maximum (services.md §5.1).
			// The per-item overhead of the protobuf encoding is well under 64 bytes.
			if len(items) > 0 && used+len(blob)+maxItemOverhead > MaxItemsBytes {
				more = true
				break scan
			}
			used += len(blob) + maxItemOverhead
			items = append(items, Item{
				ID:     append([]byte(nil), id...),
				Blob:   append([]byte(nil), blob...),
				Stored: uint64(time.Unix(0, stored).Unix()),
			})
			live++
		}
		if len(entries) < n {
			break
		}
	}
	if expiredItems > 0 {
		s.subRecord(&rec, expiredItems, expiredBytes)
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
	if int64(c.Items)+items < 0 || int64(c.Bytes)+bytes < 0 {
		s.noteUnderflow("relay totals", items, bytes)
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

// subRecord takes items and bytes off a queue record, clamping at zero and counting a clamp.
func (s *Server) subRecord(rec *queueRecord, items, bytes int64) {
	if items > int64(rec.Count) || bytes > rec.Bytes {
		s.noteUnderflow("queue record", items, bytes)
	}
	rec.Count = clampSub(rec.Count, uint32(items))
	rec.Bytes -= bytes
	if rec.Bytes < 0 {
		rec.Bytes = 0
	}
}

func (s *Server) noteUnderflow(what string, items, bytes int64) {
	s.underflows.Add(1)
	s.log.Error("mailbox counter would go below zero; clamped", "counter", what, "items", items, "bytes", bytes)
}

// UnderflowErrors reports how often a counter was asked to go below zero.
func (s *Server) UnderflowErrors() uint64 { return s.underflows.Load() }

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
