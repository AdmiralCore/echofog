package convlog

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Legacy is a message that did not come as a log event: the history from before the log, or one
// that still travels the old way (a file message, a peer that has not upgraded). It is kept and
// shown like any message, and operations (reactions, replies, pins, edits by its author) work on
// it, but it is not synced.
type Legacy struct {
	ID       string // the old message id
	Author   types.PeerID
	TS       int64 // ms
	Text     string
	Type     uint32
	Metadata string
	ReplyTo  string // the old id of the message it answers
}

// LegacyID is the id a legacy message has in the log: the same on both sides, since both know the
// old id.
func LegacyID(oldID string) EventID {
	h := sha256.Sum256([]byte("echofog-legacy-v1" + oldID))
	var id EventID
	copy(id[:], h[:])
	return id
}

// legacyOrder places migrated history before every log event (the clock is 0 there), newest
// first among itself.
func legacyOrder(bc string, ts int64, id string) []byte {
	tie := hex.EncodeToString(binary.BigEndian.AppendUint64(nil, ^uint64(ts))) + id
	return orderKey(bc, 0, tie)
}

// AddLegacy records legacy messages. live places them at the present end of the conversation (a
// message that just came the old way); otherwise they are history from before the log and go
// before everything in it. Messages already recorded are skipped.
func (l *Log) AddLegacy(c Conv, msgs []Legacy, live bool) ([]Message, error) {
	l.mu.Lock()
	bc := l.bc(c.ID)
	meta := l.loadMeta(bc, c)
	t := l.newTxn(c, bc, meta)
	for _, m := range msgs {
		if m.ID == "" || !validText(m.Text) {
			continue
		}
		id := LegacyID(m.ID).String()
		v := t.view(id)
		if v.Present {
			continue
		}
		v.Present, v.Legacy = true, true
		v.Author, v.TS, v.Text, v.Type, v.Metadata = peerHex(m.Author), m.TS, m.Text, m.Type, m.Metadata
		if m.ReplyTo != "" {
			v.ReplyTo = LegacyID(m.ReplyTo).String()
		}
		if live {
			meta.MaxLamport++
			v.Lamport = meta.MaxLamport
			t.put(orderKey(bc, v.Lamport, id), []byte(id))
			if v.Lamport >= meta.LastLamport {
				meta.LastLamport, meta.LastID, meta.LastTS = v.Lamport, id, v.TS
			}
			if v.Author == l.selfHex {
				meta.ReadLamport, meta.Unread = v.Lamport, 0
			} else {
				meta.Unread++
			}
		} else {
			t.put(legacyOrder(bc, v.TS, id), []byte(id))
			if meta.LastID == "" || (meta.LastLamport == 0 && v.TS > meta.LastTS) {
				meta.LastID, meta.LastTS = id, v.TS
			}
		}
		if v.Pin != nil && v.Pin.On {
			if del, _ := v.deleted(); !del {
				t.put(pinKey(bc, id), []byte(id))
			}
		}
		t.touch(id)
	}
	if len(t.changed) == 0 {
		l.mu.Unlock()
		return nil, nil
	}
	err := t.commit(nil, "")
	var ch *Change
	if err == nil {
		ch = t.change()
	}
	l.mu.Unlock()
	l.notify(ch)
	if err != nil {
		return nil, err
	}
	return ch.Messages, nil
}

// Peek reads an event's kind and thread without checking it (for one that was just ingested).
func Peek(raw []byte) (kind uint32, thread bool) {
	var e Event
	if proto.Unmarshal(raw, &e) != nil {
		return 0, false
	}
	return e.Kind, len(e.Thread) > 0
}

// HasMessage reports whether the conversation holds a message with this id (an event or a legacy
// message).
func (l *Log) HasMessage(c Conv, id EventID) bool {
	bc := l.bc(c.ID)
	k := viewKey(bc, id.String())
	if !l.st.Exists(k) {
		return false
	}
	raw, err := l.st.Get(k)
	if err != nil {
		return false
	}
	var v View
	return json.Unmarshal(raw, &v) == nil && v.Present
}
