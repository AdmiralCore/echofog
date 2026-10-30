package convlog

import (
	"bytes"
	"encoding/json"

	"google.golang.org/protobuf/proto"
)

// A group's picture: a PROFILE event (last writer wins) by who manages the group's channels, so
// every member shows the same one. It travels with the log, so it is kept small: the app scales
// it down before it is set.

// KindProfile sets a group's picture (body ProfileBody).
const KindProfile uint32 = 9

// MaxPhotoBytes bounds a group's picture.
const MaxPhotoBytes = 48 << 10

// Profile is a group's picture as the log has it.
type Profile struct {
	Photo []byte `json:"p,omitempty"`
	Reg   Reg    `json:"r"`
}

// photoOK reports whether b is empty (no picture) or looks like a JPEG, PNG or WebP image.
func photoOK(b []byte) bool {
	switch {
	case len(b) == 0:
		return true
	case len(b) > MaxPhotoBytes:
		return false
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return true
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return true
	case len(b) > 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return true
	}
	return false
}

func (t *txn) applyProfile(e *Event, author string, rights Rights) error {
	if t.c.Kind != KindGroup || !rights.Manage {
		return ErrForbidden
	}
	var b ProfileBody
	if proto.Unmarshal(e.Body, &b) != nil || !photoOK(b.Photo) {
		return ErrBadBody
	}
	next := Profile{Photo: b.Photo, Reg: Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs}}
	if cur, ok := t.profile(); ok && !next.Reg.after(cur.Reg) {
		return nil // an older picture: the newer one stays
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	t.put(profileKey(t.bc), raw)
	return nil
}

// profile is the group's picture as this transaction sees it.
func (t *txn) profile() (Profile, bool) {
	k := profileKey(t.bc)
	var cur Profile
	known := false
	if raw, err := t.l.st.Get(k); err == nil && json.Unmarshal(raw, &cur) == nil {
		known = true
	}
	for _, s := range t.sets {
		if string(s.Key) == string(k) && json.Unmarshal(s.Value, &cur) == nil {
			known = true
		}
	}
	return cur, known
}

// GroupPhoto is a group's picture (nil: none).
func (l *Log) GroupPhoto(c Conv) []byte {
	raw, err := l.st.Get(profileKey(l.bc(c.ID)))
	var p Profile
	if err != nil || json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return p.Photo
}

// AppendGroupPhoto sets a group's picture (empty: takes it away).
func (l *Log) AppendGroupPhoto(c Conv, photo []byte) ([]byte, EventID, error) {
	if c.Kind != KindGroup || !photoOK(photo) {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindProfile, Body: encodeBody(&ProfileBody{Photo: photo})})
}
