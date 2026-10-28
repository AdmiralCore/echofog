package convlog

import (
	"sort"

	"google.golang.org/protobuf/proto"
)

// Threads: the comments under a message (thread messages) and the thread's title. A title is a
// THREAD event on the root (last writer wins), by the root's author or a moderator.

// KindThread names the thread under a message (body ThreadBody, target the root).
const KindThread uint32 = 8

const maxThreadTitle = 100

func (t *txn) applyThread(e *Event, author string, rights Rights) error {
	var b ThreadBody
	if proto.Unmarshal(e.Body, &b) != nil || len(b.Title) > maxThreadTitle || !validText(b.Title) {
		return ErrBadBody
	}
	target := hexRef(e.Target)
	v := t.view(target)
	if v.Present && v.Author != author && !rights.Moderate && !rights.Manage {
		return ErrForbidden
	}
	r := Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs, Text: b.Title}
	if v.ThreadTitle == nil || r.after(*v.ThreadTitle) {
		v.ThreadTitle = &r
	}
	t.touch(target)
	return nil
}

// AppendThreadTitle names the thread under root ("" clears the name).
func (l *Log) AppendThreadTitle(c Conv, root EventID, title string) ([]byte, EventID, error) {
	if len(title) > maxThreadTitle || !validText(title) {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindThread, Target: root, Body: encodeBody(&ThreadBody{Title: title})})
}

// Threads lists the messages of a channel ("" the main one) that have comments, the most recently
// active first; it looks through the newest scan messages of the channel.
func (l *Log) Threads(c Conv, channel string, scan int) []Message {
	bc := l.bc(c.ID)
	prefix := orderPrefix(bc)
	if channel != "" {
		prefix = chanOrderPrefix(bc, channel)
	}
	if scan <= 0 || scan > 5000 {
		scan = 2000
	}
	entries, err := l.st.FindValuesSeek(prefix, prefix, scan)
	if err != nil {
		return nil
	}
	var out []Message
	for _, e := range entries {
		if m, ok := l.render(bc, c.ID.String(), string(e.Value)); ok && m.Replies > 0 && !m.Deleted {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastReplyAt > out[j].LastReplyAt })
	return out
}
