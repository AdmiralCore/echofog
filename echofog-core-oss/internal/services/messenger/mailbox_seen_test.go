package messenger

import (
	"testing"
	"time"
)

// A relay that restarts still keeps mail for the recipients that used it, instead of refusing
// everybody until each one comes back.
func TestRecipientSeenSurvivesARestart(t *testing.T) {
	st := newMemStore()
	recipient, sender := peerID(7), peerID(9)
	item := MailboxItem{RecipientID: recipient, SenderID: sender, Payload: []byte("sealed"),
		CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}

	if err := NewMailboxManager(st).Store(item); err != ErrRecipientUnknown {
		t.Fatalf("a recipient never seen: %v", err)
	}
	NewMailboxManager(st).NoteRecipientSeen(recipient)

	restarted := NewMailboxManager(st)
	if err := restarted.Store(item); err != nil {
		t.Fatalf("after a restart the recipient is refused: %v", err)
	}
}
