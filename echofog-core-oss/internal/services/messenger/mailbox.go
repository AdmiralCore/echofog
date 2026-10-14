package messenger

import (
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
)

var (
	// ErrMailboxFull indicates that the mailbox quota for the peer or total node capacity is exceeded.
	// The client receiving this error should search for another relay node via DHT.
	ErrMailboxFull = errors.New("mailbox quota exceeded: node is full of messages, search for another relay node")

	// ErrItemTooLarge indicates a single message exceeds the allowed size limit.
	ErrItemTooLarge = errors.New("mailbox item exceeds maximum allowed size")
)

type MailboxLimits struct {
	MaxMessagesPerPeer   int   // Max count of messages held for a single recipient
	MaxBytesPerPeer      int64 // Max total size of payloads held for a single recipient (bytes)
	MaxItemSizeBytes     int64 // Max size of a single payload (bytes)
	MaxTotalStorageBytes int64 // Max total storage for all mailboxes on this node (bytes)
}

func DefaultMailboxLimits() MailboxLimits {
	return MailboxLimits{
		MaxMessagesPerPeer:   500,
		MaxBytesPerPeer:      10 * 1024 * 1024,  // 10 MB per recipient
		MaxItemSizeBytes:     1 * 1024 * 1024,   // 1 MB per message
		MaxTotalStorageBytes: 100 * 1024 * 1024, // 100 MB total node limit
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

type MailboxManager struct {
	store  storage.Storage
	limits MailboxLimits
	mu     sync.RWMutex
}

func NewMailboxManager(store storage.Storage) *MailboxManager {
	return &MailboxManager{
		store:  store,
		limits: DefaultMailboxLimits(),
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

func (m *MailboxManager) makeKey(recipient types.PeerID, id string) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", MailboxPrefix, recipient.String(), id))
}

// Store saves an offline message/notification with a TTL and quota validation.
func (m *MailboxManager) Store(item MailboxItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Check single item size limit
	itemSize := int64(len(item.Payload))
	if m.limits.MaxItemSizeBytes > 0 && itemSize > m.limits.MaxItemSizeBytes {
		return ErrItemTooLarge
	}

	// 2. Check per-peer quotas (count and total bytes)
	prefix := []byte(fmt.Sprintf("%s%s:", MailboxPrefix, item.RecipientID.String()))
	existing, err := m.store.FindValues(prefix, 1000)
	if err == nil {
		now := time.Now().Unix()
		currentCount := 0
		var currentBytes int64 = 0

		for _, e := range existing {
			var mi MailboxItem
			if json.Unmarshal(e.Value, &mi) == nil {
				// Don't count expired items
				if mi.ExpiresAt == 0 || mi.ExpiresAt >= now {
					currentCount++
					currentBytes += int64(len(mi.Payload))
				}
			}
		}

		if m.limits.MaxMessagesPerPeer > 0 && currentCount >= m.limits.MaxMessagesPerPeer {
			return ErrMailboxFull
		}

		if m.limits.MaxBytesPerPeer > 0 && (currentBytes+itemSize) > m.limits.MaxBytesPerPeer {
			return ErrMailboxFull
		}
	}

	now := time.Now().Unix()
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

	key := m.makeKey(item.RecipientID, item.ID)
	return m.store.Set(key, data)
}

// GetPending returns all valid (non-expired) messages for the recipient.
func (m *MailboxManager) GetPending(recipient types.PeerID) ([]MailboxItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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
			_ = m.store.Delete(entry.Key)
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

	for _, id := range itemIDs {
		key := m.makeKey(recipient, id)
		_ = m.store.Delete(key)
	}
	return nil
}

// CleanupExpired scans and purges all expired messages across all recipients.
func (m *MailboxManager) CleanupExpired() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	prefix := []byte(MailboxPrefix)
	entries, err := m.store.FindValues(prefix, 5000)
	if err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	purged := 0

	for _, entry := range entries {
		var item MailboxItem
		if err := json.Unmarshal(entry.Value, &item); err != nil {
			_ = m.store.Delete(entry.Key)
			purged++
			continue
		}

		if item.ExpiresAt > 0 && item.ExpiresAt < now {
			_ = m.store.Delete(entry.Key)
			purged++
		}
	}

	return purged, nil
}

// Waiting returns how many items wait for any recipient (up to 100000 are counted).
func (m *MailboxManager) Waiting() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entries, err := m.store.FindValues([]byte(MailboxPrefix), 100000)
	if err != nil {
		return 0
	}
	return len(entries)
}

// Count returns the number of pending items for a recipient.
func (m *MailboxManager) Count(recipient types.PeerID) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	prefix := []byte(fmt.Sprintf("%s%s:", MailboxPrefix, recipient.String()))
	entries, err := m.store.FindValues(prefix, 1000)
	if err != nil {
		return 0
	}

	now := time.Now().Unix()
	count := 0
	for _, entry := range entries {
		var item MailboxItem
		if err := json.Unmarshal(entry.Value, &item); err == nil {
			if item.ExpiresAt == 0 || item.ExpiresAt >= now {
				count++
			}
		}
	}
	return count
}
