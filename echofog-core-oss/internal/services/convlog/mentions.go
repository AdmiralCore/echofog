package convlog

import (
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

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
	if channel != "" && (c.Kind != KindGroup || !validChannelID(channel)) || !validText(text) || len(m.Who) > MaxMentions {
		return nil, EventID{}, ErrBadBody
	}
	body := &MsgBody{Text: text, All: m.All}
	for _, id := range m.Who {
		body.Mentions = append(body.Mentions, append([]byte(nil), id[:]...))
	}
	return l.Append(c, Draft{Kind: KindMsg, ReplyTo: replyTo, Thread: thread, Ext: extFor(channel), Body: encodeBody(body)})
}

// maxThreadMentions bounds the unread mentions kept per conversation (or channel) from threads.
const maxThreadMentions = 256

// pushMention adds the clock of a mention inside a thread, dropping the oldest past the bound.
func pushMention(list []uint64, lamport uint64) []uint64 {
	list = append(list, lamport)
	if len(list) > maxThreadMentions {
		list = list[len(list)-maxThreadMentions:]
	}
	return list
}

// aboveRead is how many of the clocks lie above the read point, and those clocks.
func aboveRead(list []uint64, read uint64) (int, []uint64) {
	var kept []uint64
	for _, l := range list {
		if l > read {
			kept = append(kept, l)
		}
	}
	return len(kept), kept
}

// validMentions: whole peer ids, not too many.
func validMentions(b *MsgBody) bool {
	if len(b.Mentions) > MaxMentions {
		return false
	}
	for _, m := range b.Mentions {
		if len(m) != len(types.PeerID{}) {
			return false
		}
	}
	return true
}

// namesEverybody reports whether a message event carries @all (one that cannot be read names nobody).
func namesEverybody(e *Event) bool {
	if Stripped(e) || len(e.Body) == 0 {
		return false
	}
	var b MsgBody
	return proto.Unmarshal(e.Body, &b) == nil && b.All
}

// names reports whether the message names the peer with this hex id, by id or by @all.
func (v *View) names(self string) bool {
	if self == "" || v.Author == self {
		return false
	}
	if v.MentionsAll {
		return true
	}
	for _, m := range v.Mentions {
		if m == self {
			return true
		}
	}
	return false
}
