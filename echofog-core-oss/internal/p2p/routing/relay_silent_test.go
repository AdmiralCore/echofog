package routing

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func silentRelay(t *testing.T, answers bool) (*RelayServer, *messenger.MailboxManager, *atomic.Int32) {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mb := messenger.NewMailboxManager(storage.NewBadgerStorage(db))
	rs := NewRelayServer(nil, mb, nil)
	pings := &atomic.Int32{}
	rs.answers = func(types.PeerID) bool {
		pings.Add(1)
		time.Sleep(50 * time.Millisecond) // the ping is under way while more messages come
		return answers
	}
	return rs, mb, pings
}

func forwardTo(rs *RelayServer, recipient types.PeerID, n int) {
	for i := 0; i < n; i++ {
		rs.keepUnlessAnswered(messenger.MailboxItem{ID: string(rune('a' + i)), RecipientID: recipient,
			SenderID: types.PeerID{9}, Payload: []byte{'s', byte(i)},
			CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()})
	}
}

// A recipient that vanished without the relay noticing does not answer the ping that follows the
// forward, so what was forwarded to it waits in its mailbox: one ping for all of it.
func TestASilentRecipientsMessagesAreKept(t *testing.T) {
	rs, mb, pings := silentRelay(t, false)
	bob := types.PeerID{7}
	mb.NoteRecipientSeen(bob)

	forwardTo(rs, bob, 3)
	deadline := time.Now().Add(2 * time.Second)
	for mb.Count(bob) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := mb.Count(bob); got != 3 {
		t.Fatalf("kept %d of 3 messages", got)
	}
	if got := pings.Load(); got != 1 {
		t.Fatalf("%d pings for messages forwarded while one was under way, want 1", got)
	}
	if got := rs.Stats().Stored; got != 3 {
		t.Fatalf("stored counter %d, want 3", got)
	}
}

// A recipient that answers got the forward: nothing is kept.
func TestAnAnsweringRecipientGetsNoCopies(t *testing.T) {
	rs, mb, pings := silentRelay(t, true)
	bob := types.PeerID{7}
	mb.NoteRecipientSeen(bob)

	forwardTo(rs, bob, 2)
	deadline := time.Now().Add(2 * time.Second)
	for pings.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := mb.Count(bob); got != 0 {
		t.Fatalf("kept %d copies for a recipient that answered", got)
	}
}
