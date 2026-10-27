package messenger

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestMailbox_CleanupPurgesExpiredFromDiskAndTallies(t *testing.T) {
	st := newMemStore()
	m := NewMailboxManager(st)
	rcpt := peerID(1)
	m.NoteRecipientSeen(rcpt)
	past := time.Now().Add(-time.Minute).Unix()
	if err := m.Store(MailboxItem{RecipientID: rcpt, SenderID: peerID(2), Payload: []byte("old"), ExpiresAt: past}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store(MailboxItem{RecipientID: rcpt, SenderID: peerID(2), Payload: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	n, err := m.CleanupExpired()
	if err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	onDisk, _ := st.FindValues([]byte(MailboxPrefix), 0) // mail only: the recipient's registration stays
	if m.Waiting() != 1 || len(onDisk) != 1 {
		t.Fatalf("waiting %d, on disk %d; want 1 and 1", m.Waiting(), len(onDisk))
	}
}

func TestMailbox_ExpiredItemsDoNotHoldTheQuota(t *testing.T) {
	m := NewMailboxManager(newMemStore())
	l := m.GetLimits()
	l.MaxMessagesPerSender = 2
	m.SetLimits(l)
	rcpt, sender := peerID(1), peerID(2)
	m.NoteRecipientSeen(rcpt)
	past := time.Now().Add(-time.Minute).Unix()
	for _, p := range []string{"a", "b"} {
		if err := m.Store(MailboxItem{RecipientID: rcpt, SenderID: sender, Payload: []byte(p), ExpiresAt: past}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Store(MailboxItem{RecipientID: rcpt, SenderID: sender, Payload: []byte("c")}); err != nil {
		t.Fatalf("two expired items still held the sender's quota: %v", err)
	}
	if m.Waiting() != 1 {
		t.Fatalf("waiting %d, want 1", m.Waiting())
	}
}

func TestMailbox_SeenTableIsBoundedAndPruned(t *testing.T) {
	m := NewMailboxManager(newMemStore())
	var id types.PeerID
	for i := 0; i < MaxSeenRecipients+100; i++ {
		id[0], id[1], id[2] = byte(i), byte(i>>8), byte(i>>16)
		m.NoteRecipientSeen(id)
	}
	if len(m.seen) > MaxSeenRecipients {
		t.Fatalf("seen table holds %d, bound %d", len(m.seen), MaxSeenRecipients)
	}
	if !m.recipientSeen(id, DefaultRecipientSeenTTL) {
		t.Fatal("the newest recipient must be kept")
	}

	old := time.Now().Add(-2 * DefaultRecipientSeenTTL).Unix()
	m.seenMu.Lock()
	for k := range m.seen {
		m.seen[k] = old
	}
	m.seenMu.Unlock()
	if _, err := m.CleanupExpired(); err != nil {
		t.Fatal(err)
	}
	if len(m.seen) != 0 {
		t.Fatalf("%d recipients past the TTL survived the cleanup", len(m.seen))
	}
}
