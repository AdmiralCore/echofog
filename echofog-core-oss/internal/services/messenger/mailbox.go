package messenger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	MailboxPrefix = "mb:v1:"
	DefaultTTL    = 7 * 24 * time.Hour
	// DefaultRecipientSeenTTL is how long after a peer last used this relay we keep accepting
	// offline messages addressed to it.
	DefaultRecipientSeenTTL = 24 * time.Hour
	// MaxSeenRecipients bounds the table of recipients that used this relay.
	MaxSeenRecipients = 100000
	// CleanupInterval is how often the node purges expired items (see CleanupExpired).
	CleanupInterval = 10 * time.Minute
)

var (
	// ErrMailboxFull indicates that the mailbox quota for the peer or total node capacity is exceeded.
	// The client receiving this error should search for another relay node via DHT.
	ErrMailboxFull = errors.New("mailbox quota exceeded: node is full of messages, search for another relay node")

	// ErrItemTooLarge indicates a single message exceeds the allowed size limit.
	ErrItemTooLarge = errors.New("mailbox item exceeds maximum allowed size")

	// ErrRecipientUnknown indicates the relay will not keep a message for a recipient that has not
	// recently used it. This stops a stranger from filling the node's disk by naming random
	// recipients that never connect here.
	ErrRecipientUnknown = errors.New("mailbox refuses message: recipient has not recently used this relay")
)

type MailboxLimits struct {
	MaxMessagesPerPeer   int   // Max count of messages held for a single recipient
	MaxBytesPerPeer      int64 // Max total size of payloads held for a single recipient (bytes)
	MaxItemSizeBytes     int64 // Max size of a single payload (bytes)
	MaxTotalStorageBytes int64 // Max total payload storage for all mailboxes on this node (bytes)
	MaxTotalMessages     int   // Max count of messages held across all mailboxes on this node
	MaxMessagesPerSender int   // Max count of messages a single sender may have stored across all recipients
	MaxBytesPerSender    int64 // Max total payload bytes a single sender may have stored (bytes)

	// RequireRecipientSeen, when true, makes Store reject a recipient that has not been marked seen
	// (via NoteRecipientSeen) within RecipientSeenTTL. RecipientSeenTTL <= 0 means "ever seen".
	RequireRecipientSeen bool
	RecipientSeenTTL     time.Duration
}

func DefaultMailboxLimits() MailboxLimits {
	return MailboxLimits{
		MaxMessagesPerPeer:   500,
		MaxBytesPerPeer:      10 * 1024 * 1024,  // 10 MB per recipient
		MaxItemSizeBytes:     1 * 1024 * 1024,   // 1 MB per message
		MaxTotalStorageBytes: 100 * 1024 * 1024, // 100 MB total node limit
		MaxTotalMessages:     100000,            // total message count across all mailboxes
		MaxMessagesPerSender: 2000,              // one sender's share of the message count
		MaxBytesPerSender:    20 * 1024 * 1024,  // 20 MB one sender may occupy
		RequireRecipientSeen: true,
		RecipientSeenTTL:     DefaultRecipientSeenTTL,
	}
}

type MailboxItem struct {
	ID          string       `json:"id"`
	RecipientID types.PeerID `json:"recipient_id"`
	SenderID    types.PeerID `json:"sender_id"`
	Payload     []byte       `json:"payload"` // E2EE envelope / message data
	CreatedAt   int64        `json:"created_at"`
	ExpiresAt   int64        `json:"expires_at"`
}

// usage is a running (count, bytes) tally kept per recipient and per sender so quotas are checked
// without rescanning storage on every Store.
type usage struct {
	count int
	bytes int64
}

// meta is what the manager remembers about one stored item so it can keep the running totals exact
// when the item is delivered, acknowledged or expires.
type meta struct {
	recipient types.PeerID
	sender    types.PeerID
	size      int64
	expiresAt int64
}

type MailboxManager struct {
	store  storage.Storage
	limits MailboxLimits
	mu     sync.RWMutex

	// running state, rebuilt from storage once on first use (see ensureLoadedLocked)
	loaded      bool
	index       map[string]meta         // storage key -> item metadata
	byRecipient map[types.PeerID]*usage // live tally per recipient
	bySender    map[types.PeerID]*usage // live tally per sender
	totalBytes  int64
	totalCount  int
	lastPurge   int64 // unix second of the last purgeExpiredLocked

	seenMu sync.Mutex
	seen   map[types.PeerID]int64 // recipient -> unix time it last used this relay
}

func NewMailboxManager(store storage.Storage) *MailboxManager {
	return &MailboxManager{
		store:       store,
		limits:      DefaultMailboxLimits(),
		index:       make(map[string]meta),
		byRecipient: make(map[types.PeerID]*usage),
		bySender:    make(map[types.PeerID]*usage),
		seen:        make(map[types.PeerID]int64),
	}
}

func (m *MailboxManager) SetLimits(limits MailboxLimits) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits = limits
}

func (m *MailboxManager) GetLimits() MailboxLimits {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.limits
}

// NoteRecipientSeen records that a peer used this relay (connected or registered), so messages may
// be kept for it while it is offline.
func (m *MailboxManager) NoteRecipientSeen(id types.PeerID) {
	now := time.Now().Unix()
	ttl := m.GetLimits().RecipientSeenTTL
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	if _, ok := m.seen[id]; !ok && len(m.seen) >= MaxSeenRecipients {
		// peer IDs are free: a full table first drops who is past the TTL, then anybody, down to
		// 90% so that the walk is not repeated on every new recipient
		m.pruneSeenLocked(now, ttl)
		for k := range m.seen {
			if len(m.seen) < MaxSeenRecipients*9/10 {
				break
			}
			delete(m.seen, k)
		}
	}
	m.seen[id] = now
}

// pruneSeenLocked forgets recipients last seen more than ttl ago. Caller holds m.seenMu.
func (m *MailboxManager) pruneSeenLocked(now int64, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	for k, last := range m.seen {
		if now-last > int64(ttl.Seconds()) {
			delete(m.seen, k)
		}
	}
}

func (m *MailboxManager) recipientSeen(id types.PeerID, ttl time.Duration) bool {
	m.seenMu.Lock()
	last, ok := m.seen[id]
	m.seenMu.Unlock()
	if !ok {
		return false
	}
	if ttl <= 0 {
		return true
	}
	return time.Now().Unix()-last <= int64(ttl.Seconds())
}

func (m *MailboxManager) makeKey(recipient types.PeerID, id string) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", MailboxPrefix, recipient.String(), id))
}

// storageID derives the per-item id from the sender and payload, server-side. The sender cannot
// choose it, so it cannot overwrite another peer's stored item by reusing a message id, and two
// identical submissions collapse to one entry instead of multiplying.
func storageID(sender types.PeerID, payload []byte) string {
	h := sha256.New()
	h.Write(sender[:])
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// ensureLoadedLocked rebuilds the running tallies from storage exactly once. Caller holds m.mu.
func (m *MailboxManager) ensureLoadedLocked() {
	if m.loaded {
		return
	}
	m.loaded = true

	entries, err := m.store.FindValues([]byte(MailboxPrefix), 0)
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, e := range entries {
		var item MailboxItem
		if json.Unmarshal(e.Value, &item) != nil {
			// unreadable entry: drop it
			_ = m.store.Delete(e.Key)
			continue
		}
		if item.ExpiresAt > 0 && item.ExpiresAt < now {
			_ = m.store.Delete(e.Key)
			continue
		}
		m.addLocked(string(e.Key), meta{
			recipient: item.RecipientID,
			sender:    item.SenderID,
			size:      int64(len(item.Payload)),
			expiresAt: item.ExpiresAt,
		})
	}
}

// addLocked registers an item in the running tallies. Caller holds m.mu.
func (m *MailboxManager) addLocked(key string, md meta) {
	m.index[key] = md
	m.totalBytes += md.size
	m.totalCount++
	r := m.byRecipient[md.recipient]
	if r == nil {
		r = &usage{}
		m.byRecipient[md.recipient] = r
	}
	r.count++
	r.bytes += md.size
	s := m.bySender[md.sender]
	if s == nil {
		s = &usage{}
		m.bySender[md.sender] = s
	}
	s.count++
	s.bytes += md.size
}

// removeLocked deletes an item from storage and the running tallies. Caller holds m.mu.
func (m *MailboxManager) removeLocked(key string) {
	md, ok := m.index[key]
	if !ok {
		_ = m.store.Delete([]byte(key))
		return
	}
	delete(m.index, key)
	_ = m.store.Delete([]byte(key))

	m.totalBytes -= md.size
	m.totalCount--
	if r := m.byRecipient[md.recipient]; r != nil {
		r.count--
		r.bytes -= md.size
		if r.count <= 0 {
			delete(m.byRecipient, md.recipient)
		}
	}
	if s := m.bySender[md.sender]; s != nil {
		s.count--
		s.bytes -= md.size
		if s.count <= 0 {
			delete(m.bySender, md.sender)
		}
	}
}

func (md meta) expired(now int64) bool { return md.expiresAt > 0 && md.expiresAt < now }

// quotaLocked checks the per-recipient, per-sender and node-wide quotas for one more item.
// Caller holds m.mu.
func (m *MailboxManager) quotaLocked(limits MailboxLimits, item MailboxItem, itemSize int64) error {
	// 4. Per-recipient quotas (count and bytes).
	if r := m.byRecipient[item.RecipientID]; r != nil {
		if limits.MaxMessagesPerPeer > 0 && r.count >= limits.MaxMessagesPerPeer {
			return ErrMailboxFull
		}
		if limits.MaxBytesPerPeer > 0 && r.bytes+itemSize > limits.MaxBytesPerPeer {
			return ErrMailboxFull
		}
	}

	// 5. Per-sender quotas (count and bytes), keyed by the authenticated sender.
	if s := m.bySender[item.SenderID]; s != nil {
		if limits.MaxMessagesPerSender > 0 && s.count >= limits.MaxMessagesPerSender {
			return ErrMailboxFull
		}
		if limits.MaxBytesPerSender > 0 && s.bytes+itemSize > limits.MaxBytesPerSender {
			return ErrMailboxFull
		}
	}

	// 6. Node-wide total quotas (count and bytes).
	if limits.MaxTotalMessages > 0 && m.totalCount >= limits.MaxTotalMessages {
		return ErrMailboxFull
	}
	if limits.MaxTotalStorageBytes > 0 && m.totalBytes+itemSize > limits.MaxTotalStorageBytes {
		return ErrMailboxFull
	}
	return nil
}

// purgeExpiredLocked removes every item past its expiry, going by the in-memory index, and
// returns how many. Caller holds m.mu.
func (m *MailboxManager) purgeExpiredLocked(now int64) int {
	m.lastPurge = now
	purged := 0
	for key, md := range m.index {
		if md.expired(now) {
			m.removeLocked(key)
			purged++
		}
	}
	return purged
}

// Store saves an offline message/notification with a TTL and quota validation.
func (m *MailboxManager) Store(item MailboxItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()

	limits := m.limits
	itemSize := int64(len(item.Payload))

	// 1. Single item size limit
	if limits.MaxItemSizeBytes > 0 && itemSize > limits.MaxItemSizeBytes {
		return ErrItemTooLarge
	}

	// 2. Only keep messages for recipients that recently used this relay.
	if limits.RequireRecipientSeen && !m.recipientSeen(item.RecipientID, limits.RecipientSeenTTL) {
		return ErrRecipientUnknown
	}

	// 3. Derive the storage id server-side so the sender cannot overwrite another peer's item.
	item.ID = storageID(item.SenderID, item.Payload)
	key := string(m.makeKey(item.RecipientID, item.ID))

	// An identical resubmission (same sender + payload for the same recipient) is already stored;
	// refresh its bytes on disk but do not double-count.
	now := time.Now().Unix()
	if md, exists := m.index[key]; exists {
		if !md.expired(now) {
			return nil
		}
		m.removeLocked(key) // expired: store it afresh
	}

	// 4-6. Quotas. Items past their expiry count against none: when one would refuse, the expired
	// items are purged first and the quotas checked again.
	// The purge walks the whole index, so a sender held at its quota gets at most one a second.
	err := m.quotaLocked(limits, item, itemSize)
	if err != nil && now != m.lastPurge && m.purgeExpiredLocked(now) > 0 {
		err = m.quotaLocked(limits, item, itemSize)
	}
	if err != nil {
		return err
	}

	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	if item.ExpiresAt == 0 {
		item.ExpiresAt = now + int64(DefaultTTL.Seconds())
	}

	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("failed to marshal mailbox item: %w", err)
	}

	if err := m.store.Set([]byte(key), data); err != nil {
		return err
	}
	m.addLocked(key, meta{
		recipient: item.RecipientID,
		sender:    item.SenderID,
		size:      itemSize,
		expiresAt: item.ExpiresAt,
	})
	return nil
}

// GetPending returns all valid (non-expired) messages for the recipient.
func (m *MailboxManager) GetPending(recipient types.PeerID) ([]MailboxItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()

	prefix := []byte(fmt.Sprintf("%s%s:", MailboxPrefix, recipient.String()))
	entries, err := m.store.FindValues(prefix, 1000)
	if err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	var pending []MailboxItem

	for _, entry := range entries {
		var item MailboxItem
		if err := json.Unmarshal(entry.Value, &item); err != nil {
			continue
		}

		// Auto-prune expired message on read
		if item.ExpiresAt > 0 && item.ExpiresAt < now {
			m.removeLocked(string(entry.Key))
			continue
		}

		pending = append(pending, item)
	}

	return pending, nil
}

// Acknowledge removes delivered messages from storage.
func (m *MailboxManager) Acknowledge(recipient types.PeerID, itemIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()

	for _, id := range itemIDs {
		key := string(m.makeKey(recipient, id))
		m.removeLocked(key)
	}
	return nil
}

// CleanupExpired purges all expired messages across all recipients, and forgets recipients that
// have not used this relay within RecipientSeenTTL. The node runs it every CleanupInterval.
func (m *MailboxManager) CleanupExpired() (int, error) {
	now := time.Now().Unix()
	m.mu.Lock()
	m.ensureLoadedLocked()
	purged := m.purgeExpiredLocked(now)
	ttl := m.limits.RecipientSeenTTL
	m.mu.Unlock()

	m.seenMu.Lock()
	m.pruneSeenLocked(now, ttl)
	m.seenMu.Unlock()
	return purged, nil
}

// Waiting returns how many items wait for any recipient.
func (m *MailboxManager) Waiting() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	return m.totalCount
}

// Count returns the number of pending items for a recipient.
func (m *MailboxManager) Count(recipient types.PeerID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	if r := m.byRecipient[recipient]; r != nil {
		return r.count
	}
	return 0
}
