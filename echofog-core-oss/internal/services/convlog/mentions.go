package convlog

import "github.com/echofog/echofog-core/pkg/api/types"

// PermMentionAll lets a member write @all: a message that names everybody in the conversation.
const PermMentionAll uint32 = 1 << 15

// MaxMentions bounds the people one message names.
const MaxMentions = 50

// Mentions are the people a message names.
type Mentions struct {
	Who []types.PeerID
	All bool
}

// AppendMessageMentioning writes a message that names people (and, with the right, everybody).
func (l *Log) AppendMessageMentioning(c Conv, channel, text string, m Mentions, replyTo, thread EventID) ([]byte, EventID, error) {
	return nil, EventID{}, ErrBadBody // not written yet
}
