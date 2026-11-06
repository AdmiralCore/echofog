package voice

import (
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
)

// Entry is a voice message the receiver knows.
type Entry struct{}

func (e *Entry) Meta() Meta                 { return Meta{} }
func (e *Entry) Offer() (files.Offer, bool) { return files.Offer{}, false }
func (e *Entry) Finish(file []byte) error   { return ErrInvalid }
func (e *Entry) Audio() ([]byte, bool)      { return nil, false }
func (e *Entry) ID() string                 { return "" }

// Inbox is the receiver's list, keyed by (sender, message ID).
type Inbox struct{}

func NewInbox(max int) *Inbox { return &Inbox{} }

// Add registers a message. fresh is false when the same message was added before.
func (b *Inbox) Add(m *chatpb.PersonalMessageProto) (e *Entry, fresh bool, err error) {
	return &Entry{}, false, ErrInvalid
}

func (b *Inbox) Len() int { return 0 }
