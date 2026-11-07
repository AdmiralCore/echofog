package voice

import (
	"encoding/hex"
	"strings"
	"sync"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
)

// Entry is a voice message the receiver knows. A clip that came inline is playable at once; a file
// clip becomes playable after files.Fetch has stored it and Finish has checked it.
type Entry struct {
	mu       sync.Mutex
	id       string
	meta     Meta
	offer    files.Offer
	hasOffer bool
	audio    []byte
}

// ID is the message ID.
func (e *Entry) ID() string { return e.id }

// Meta is the length, waveform and, for a file clip, the offer.
func (e *Entry) Meta() Meta { return e.meta }

// Offer is what to give files.Fetch; false for an inline clip.
func (e *Entry) Offer() (files.Offer, bool) { return e.offer, e.hasOffer }

// Audio is the clean clip, once there is one.
func (e *Entry) Audio() ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.audio, e.audio != nil
}

// Finish takes the file that files.Fetch stored (its Merkle root was proved there). The receiver
// never plays the sender's container: the clip is cleaned again, and its length must be the one the
// message states.
func (e *Entry) Finish(file []byte) error {
	if !e.hasOffer {
		return invalid("not a file clip")
	}
	clean, ms, err := Sanitize(file)
	if err != nil {
		return err
	}
	if !sameLength(ms, e.meta.DurationMS) {
		return ErrDuration
	}
	e.mu.Lock()
	e.audio = clean
	e.mu.Unlock()
	return nil
}

func sameLength(actual, claimed uint32) bool {
	d := int64(actual) - int64(claimed)
	return d >= -durationSlackMS && d <= durationSlackMS
}

type inboxKey struct{ sender, id string }

// Inbox is the receiver's list of voice messages, keyed by (sender, message ID): a message that
// arrives twice (a retry, the mailbox and the direct copy) is one entry, and the ID of one sender
// cannot take over the ID of another. It holds at most max entries; the oldest goes first.
type Inbox struct {
	mu    sync.Mutex
	max   int
	m     map[inboxKey]*Entry
	order []inboxKey
}

// NewInbox returns a list of at most limit entries (at least one).
func NewInbox(limit int) *Inbox {
	return &Inbox{max: max(limit, 1), m: map[inboxKey]*Entry{}}
}

// Len is the number of entries.
func (b *Inbox) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// Add checks a voice message and registers it. fresh is false when the same message was added
// before; the earlier entry is returned. The sender named in the message is the one the chat layer
// authenticated for it.
func (b *Inbox) Add(from [32]byte, m *chatpb.PersonalMessageProto) (e *Entry, fresh bool, err error) {
	if m == nil || m.Type != chatpb.PersonalMessageType_MSG_TYPE_VOICE {
		return nil, false, invalid("not a voice message")
	}
	if m.Id == "" || len(m.Id) > 64 || m.Text != "" || len(m.ReplyToId) > 64 {
		return nil, false, invalid("message fields")
	}
	if raw, err := hex.DecodeString(m.SenderId); err != nil || len(raw) != 32 {
		return nil, false, invalid("sender")
	}
	key := inboxKey{strings.ToLower(m.SenderId), m.Id}

	b.mu.Lock()
	defer b.mu.Unlock()
	if old, ok := b.m[key]; ok {
		return old, false, nil
	}
	meta, err := metaOf(m)
	if err != nil {
		return nil, false, err
	}
	e = &Entry{id: m.Id, meta: meta}
	if o := meta.Offer; o != nil {
		if !strings.EqualFold(hex.EncodeToString(o.Sender[:]), m.SenderId) {
			return nil, false, invalid("the offer is not the sender's")
		}
		if m.ContentHash == "" || !strings.EqualFold(m.ContentHash, hex.EncodeToString(o.Root[:])) {
			return nil, false, invalid("content hash differs from the root")
		}
		e.offer, e.hasOffer = *o, true
	} else {
		clean, ms, err := Sanitize(meta.Inline)
		if err != nil {
			return nil, false, err
		}
		if !sameLength(ms, meta.DurationMS) {
			return nil, false, ErrDuration
		}
		e.audio = clean
	}
	if len(b.order) >= b.max {
		delete(b.m, b.order[0])
		b.order = b.order[1:]
	}
	b.m[key] = e
	b.order = append(b.order, key)
	return e, true, nil
}
