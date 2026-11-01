package messenger

import (
	"time"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// memStore is a minimal in-memory storage.Storage for the mailbox tests.
type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemStore() *memStore { return &memStore{m: map[string][]byte{}} }

func (s *memStore) Set(k, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(k)] = append([]byte(nil), v...)
	return nil
}
func (s *memStore) Get(k []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[string(k)]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}
func (s *memStore) Delete(k []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(k))
	return nil
}
func (s *memStore) FindValues(prefix []byte, _ int) ([]storage.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Entry
	for k, v := range s.m {
		if strings.HasPrefix(k, string(prefix)) {
			out = append(out, storage.Entry{Key: []byte(k), Value: append([]byte(nil), v...)})
		}
	}
	return out, nil
}
func (s *memStore) FindValuesSeek(prefix, _ []byte, limit int) ([]storage.Entry, error) {
	return s.FindValues(prefix, limit)
}
func (s *memStore) Exists(k []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[string(k)]
	return ok
}
func (s *memStore) Close() {}

func peerID(b byte) types.PeerID { var id types.PeerID; id[0] = b; return id }

// With only per-recipient quotas, a stranger could fill a node's disk by naming a different random
// recipient each time. The node-wide total cap must stop that flood.
func TestMailbox_TotalCapStopsFlood(t *testing.T) {
	m := NewMailboxManager(newMemStore())
	m.SetLimits(MailboxLimits{
		MaxItemSizeBytes:     4096,
		MaxBytesPerPeer:      1 << 30, // effectively unlimited per recipient
		MaxMessagesPerPeer:   1 << 20,
		MaxBytesPerSender:    1 << 30, // and per sender
		MaxMessagesPerSender: 1 << 20,
		MaxTotalStorageBytes: 10 * 1024, // the real bound: 10 KB total
		RequireRecipientSeen: false,     // isolate the total cap from the seen gate
	})

	payload := make([]byte, 1024) // 1 KB each
	stored := 0
	var lastErr error
	for i := 0; i < 1000; i++ {
		err := m.Store(MailboxItem{
			RecipientID: peerID(byte(i)),       // a different recipient every time
			SenderID:    peerID(byte(128 + i)), // and a different sender every time
			Payload:     append([]byte(nil), payload...),
		})
		if err != nil {
			lastErr = err
			break
		}
		stored++
	}

	if !errors.Is(lastErr, ErrMailboxFull) {
		t.Fatalf("flood should be stopped with ErrMailboxFull, got %v after %d stores", lastErr, stored)
	}
	if got := int64(m.Waiting()) * 1024; got > 10*1024 {
		t.Fatalf("total stored bytes %d exceeded the 10 KB cap", got)
	}
	if stored == 0 || stored > 11 {
		t.Fatalf("expected roughly 10 items to fit under the cap, stored %d", stored)
	}
}

// One sender must not occupy more than its quota, even when it spreads its messages across many
// different recipients (so the per-recipient limits never trip). A different sender is unaffected.
func TestMailbox_PerSenderQuota(t *testing.T) {
	m := NewMailboxManager(newMemStore())
	m.SetLimits(MailboxLimits{
		MaxItemSizeBytes:     4096,
		MaxBytesPerPeer:      1 << 30,
		MaxMessagesPerPeer:   1 << 20,
		MaxTotalStorageBytes: 1 << 30,
		MaxTotalMessages:     1 << 20,
		MaxMessagesPerSender: 5, // the bound under test
		MaxBytesPerSender:    1 << 30,
		RequireRecipientSeen: false,
	})

	flooder := peerID(0x66)
	stored := 0
	var lastErr error
	for i := 0; i < 50; i++ {
		err := m.Store(MailboxItem{
			RecipientID: peerID(byte(i)), // distinct recipient each time
			SenderID:    flooder,
			Payload:     []byte("hello"),
		})
		if err != nil {
			lastErr = err
			break
		}
		stored++
	}

	if !errors.Is(lastErr, ErrMailboxFull) {
		t.Fatalf("per-sender quota should trip ErrMailboxFull, got %v", lastErr)
	}
	if stored != 5 {
		t.Fatalf("expected exactly 5 messages from one sender, stored %d", stored)
	}

	// A different sender still has its own quota.
	if err := m.Store(MailboxItem{RecipientID: peerID(0x01), SenderID: peerID(0x02), Payload: []byte("hi")}); err != nil {
		t.Fatalf("a different sender must still be able to store: %v", err)
	}
}

// With the recipient-seen gate on (the default), the node refuses to keep a message for a recipient
// that has never used it, and accepts it once the recipient has been seen.
func TestMailbox_RecipientSeenGate(t *testing.T) {
	m := NewMailboxManager(newMemStore()) // default limits: RequireRecipientSeen = true
	recipient, sender := peerID(0x0b), peerID(0x0a)

	err := m.Store(MailboxItem{RecipientID: recipient, SenderID: sender, Payload: []byte("x")})
	if !errors.Is(err, ErrRecipientUnknown) {
		t.Fatalf("a message for an unseen recipient must be refused, got %v", err)
	}

	m.NoteRecipientSeen(recipient)
	if err := m.Store(MailboxItem{RecipientID: recipient, SenderID: sender, Payload: []byte("x")}); err != nil {
		t.Fatalf("a message for a seen recipient must be accepted, got %v", err)
	}
	if m.Count(recipient) != 1 {
		t.Fatalf("expected 1 stored item for the seen recipient, got %d", m.Count(recipient))
	}
}

// The storage key is derived from the sender and payload, server-side. A sender cannot overwrite
// another sender's stored item, and a sender resubmitting the same payload does not multiply it.
func TestMailbox_ServerDerivedKey_NoOverwrite(t *testing.T) {
	m := NewMailboxManager(newMemStore())
	lim := DefaultMailboxLimits()
	lim.RequireRecipientSeen = false
	m.SetLimits(lim)

	recipient := peerID(0x0b)
	senderA, senderB := peerID(0x01), peerID(0x02)
	payload := []byte("same bytes")

	// Two different senders, same recipient and payload, with the SAME attacker-chosen ID: both are
	// kept, because the key is derived from the sender, not from the id the caller supplied.
	_ = m.Store(MailboxItem{ID: "collide", RecipientID: recipient, SenderID: senderA, Payload: payload})
	_ = m.Store(MailboxItem{ID: "collide", RecipientID: recipient, SenderID: senderB, Payload: payload})
	if m.Count(recipient) != 2 {
		t.Fatalf("two senders must produce two items, got %d", m.Count(recipient))
	}

	// The same sender resubmitting the same payload is deduplicated, not doubled.
	_ = m.Store(MailboxItem{ID: "whatever", RecipientID: recipient, SenderID: senderA, Payload: payload})
	if m.Count(recipient) != 2 {
		t.Fatalf("a resubmission by the same sender must not add an item, got %d", m.Count(recipient))
	}
}

// A recipient whose box is full gets the new message and loses its oldest: refusing made senders
// give up on the relay, and a recipient away that long has moved on from the old ones. Each
// sender's own quota still refuses past it.
func TestMailbox_FullRecipientDropsItsOldest(t *testing.T) {
	m := NewMailboxManager(newMemStore())
	m.SetLimits(MailboxLimits{
		MaxItemSizeBytes:     1024,
		MaxMessagesPerPeer:   3,
		MaxBytesPerPeer:      1 << 20,
		MaxMessagesPerSender: 3,
		MaxBytesPerSender:    1 << 20,
		MaxTotalMessages:     100,
		MaxTotalStorageBytes: 1 << 20,
	})
	to := peerID(1)
	store := func(sender byte, n int, exp int64) error {
		return m.Store(MailboxItem{RecipientID: to, SenderID: peerID(sender), Payload: []byte{sender, byte(n)}, ExpiresAt: exp})
	}
	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		if err := store(10, i, now+int64(100+i)); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
	}
	if err := store(11, 0, now+500); err != nil {
		t.Fatalf("a full box must make room, got %v", err)
	}
	if got := m.Count(to); got != 3 {
		t.Fatalf("the box keeps its size: %d", got)
	}
	items, err := m.GetPending(to)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.SenderID == peerID(10) && it.Payload[1] == 0 {
			t.Fatal("the oldest message should be the one that went")
		}
	}
	// a sender at its own quota is still refused, and nothing is pushed out for it
	if err := store(11, 1, now+501); err != nil {
		t.Fatal(err)
	}
	if err := store(11, 2, now+502); err != nil {
		t.Fatal(err)
	}
	if err := store(11, 3, now+503); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("a sender past its quota must be refused, got %v", err)
	}
	if got := m.Count(to); got != 3 {
		t.Fatalf("a refused message pushes nothing out: %d", got)
	}
}
