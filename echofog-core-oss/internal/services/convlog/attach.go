package convlog

import (
	"errors"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Files in a group and the closing of a group (T11.3e, conversation-log.md §2.5).

// KindClose is the event that closes a group (group.close).
const KindClose uint32 = 10

// ErrClosed reports an event refused because the group is closed.
var ErrClosed = errors.New("convlog: the group is closed")

// AttRef is a file a group's log offers: the message that carries it and the Merkle root.
type AttRef struct {
	Msg  Message
	Root [32]byte
}

// AppendAttachment writes a message that carries a file (a at most one per event) to a channel of c.
func (l *Log) AppendAttachment(c Conv, channel string, a *Attachment, text string, replyTo, thread EventID) ([]byte, EventID, error) {
	return nil, EventID{}, errors.New("not built")
}

// AttachmentIn finds the message of c that carries the file fid, if it is there and not deleted.
func (l *Log) AttachmentIn(c Conv, fid [16]byte) (AttRef, bool) { return AttRef{}, false }

// AppendClose closes the group c: only its owner may. The event carries the last seq the owner has
// seen per author.
func (l *Log) AppendClose(c Conv) ([]byte, EventID, error) {
	return nil, EventID{}, errors.New("not built")
}

// Closed reports whether the group c is closed.
func (l *Log) Closed(c Conv) bool { return false }

var _ = types.PeerID{}

// checkAttachment reports whether an attachment is well formed.
func checkAttachment(a *Attachment) error { return errors.New("not built") }
