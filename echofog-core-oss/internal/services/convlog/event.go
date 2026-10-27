// Package convlog keeps every conversation (a direct chat or a group) as an append-only log of
// signed, typed events — messages and the operations on them (edits, deletes, reactions, pins) —
// and syncs the logs between participants. See docs/design/conversation-log.md.
package convlog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"unicode/utf8"

	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// Event kinds. A kind this build does not know is stored and synced but not shown, so a newer
// client's events survive a pass through an older one.
const (
	KindMsg    uint32 = 1
	KindEdit   uint32 = 2
	KindDelete uint32 = 3
	KindReact  uint32 = 4
	KindPin    uint32 = 5
)

// MsgTypeSystem marks a message the app wrote itself (a call that started, a member who joined).
const MsgTypeSystem uint32 = 100

const (
	convIDLen  = 16
	eventIDLen = 16
	authorLen  = ed25519.PublicKeySize
	hashLen    = sha256.Size

	// MaxEventSize bounds one encoded event; larger content (files) travels on its own and is
	// referenced from metadata.
	MaxEventSize = 64 << 10
	// MaxTextLen bounds a message or edit text.
	MaxTextLen = 16 << 10
	// maxEmojiLen bounds a reaction key.
	maxEmojiLen = 64
	// maxLamportJump is how far an event may push the conversation clock in one step; more is
	// clock-inflation spam (it would pin the author's messages to the bottom forever).
	maxLamportJump = 1 << 24
)

// ConvID names a conversation.
type ConvID [convIDLen]byte

func (c ConvID) String() string { return hex.EncodeToString(c[:]) }

// EventID names an event: a hash of its signed bytes and signature.
type EventID [eventIDLen]byte

func (e EventID) String() string { return hex.EncodeToString(e[:]) }

// IsZero reports whether the id is unset.
func (e EventID) IsZero() bool { return e == EventID{} }

// ParseEventID reads an id from its hex form.
func ParseEventID(s string) (EventID, error) {
	var id EventID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != eventIDLen {
		return id, errors.New("bad event id")
	}
	copy(id[:], b)
	return id, nil
}

// ParseConvID reads a conversation id from its hex form.
func ParseConvID(s string) (ConvID, error) {
	var id ConvID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != convIDLen {
		return id, errors.New("bad conversation id")
	}
	copy(id[:], b)
	return id, nil
}

// DMConv is the conversation of two peers; both compute the same id.
func DMConv(a, b types.PeerID) ConvID {
	lo, hi := a, b
	if string(hi[:]) < string(lo[:]) {
		lo, hi = hi, lo
	}
	h := sha256.New()
	h.Write([]byte("echofog-conv-dm-v1"))
	h.Write(lo[:])
	h.Write(hi[:])
	var c ConvID
	copy(c[:], h.Sum(nil))
	return c
}

// GroupConv is the conversation of a group.
func GroupConv(groupID string) ConvID {
	h := sha256.New()
	h.Write([]byte("echofog-conv-group-v1"))
	h.Write([]byte(groupID))
	var c ConvID
	copy(c[:], h.Sum(nil))
	return c
}

// bodyHash is what the signature covers in place of the body: its hash, or the hash kept by a
// stripped event.
func bodyHash(e *Event) []byte {
	if e.Body == nil && len(e.BodyHash) == hashLen {
		return e.BodyHash
	}
	h := sha256.Sum256(e.Body)
	return h[:]
}

func appendLP(out, b []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(b)))
	return append(out, b...)
}

// signedBytes is the canonical encoding the author signs: fixed order, length prefixed, the body
// by its hash. It does not depend on protobuf's encoding.
func signedBytes(e *Event) []byte {
	out := make([]byte, 0, 160+len(e.Ext))
	out = append(out, "echofog-ev-sig-v1"...)
	out = appendLP(out, e.Conv)
	out = appendLP(out, e.Author)
	out = binary.BigEndian.AppendUint64(out, e.Seq)
	out = binary.BigEndian.AppendUint64(out, e.Lamport)
	out = binary.BigEndian.AppendUint64(out, uint64(e.TsMs))
	out = binary.BigEndian.AppendUint32(out, e.Kind)
	out = appendLP(out, e.Target)
	out = appendLP(out, e.ReplyTo)
	out = appendLP(out, e.Thread)
	out = appendLP(out, bodyHash(e))
	out = appendLP(out, e.Ext)
	return out
}

// idOf is the event's id: it stays the same when the body is stripped.
func idOf(e *Event) EventID {
	h := sha256.New()
	h.Write([]byte("echofog-ev-v1"))
	h.Write(signedBytes(e))
	h.Write(e.Sig)
	var id EventID
	copy(id[:], h.Sum(nil))
	return id
}

// Sign fills e.Sig with sign (the author's identity key).
func Sign(e *Event, sign func([]byte) ([]byte, error)) error {
	sig, err := sign(signedBytes(e))
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize {
		return errors.New("signature has the wrong size")
	}
	e.Sig = sig
	return nil
}

// AuthorID is the peer id of the event's author.
func AuthorID(e *Event) types.PeerID {
	var pub types.PeerPublicKey
	copy(pub[:], e.Author)
	return types.PeerPubKeyToID(pub)
}

func okRef(b []byte) bool { return len(b) == 0 || len(b) == eventIDLen }

// check validates the shape and signature of an event.
func check(e *Event) error {
	switch {
	case len(e.Conv) != convIDLen:
		return errors.New("bad conversation id")
	case len(e.Author) != authorLen:
		return errors.New("bad author key")
	case e.Seq == 0:
		return errors.New("sequence starts at 1")
	case e.Lamport == 0:
		return errors.New("clock starts at 1")
	case e.Kind == 0:
		return errors.New("no kind")
	case !okRef(e.Target) || !okRef(e.ReplyTo) || !okRef(e.Thread):
		return errors.New("bad reference")
	case e.Body != nil && len(e.BodyHash) != 0:
		return errors.New("both a body and a stripped body")
	case e.Body == nil && len(e.BodyHash) != 0 && len(e.BodyHash) != hashLen:
		return errors.New("bad body hash")
	case len(e.Sig) != ed25519.SignatureSize:
		return errors.New("bad signature size")
	}
	switch e.Kind {
	case KindEdit, KindDelete, KindReact, KindPin:
		if len(e.Target) == 0 {
			return errors.New("operation without a target")
		}
	}
	if !ed25519.Verify(ed25519.PublicKey(e.Author), signedBytes(e), e.Sig) {
		return errors.New("bad signature")
	}
	return nil
}

// Decode parses and verifies an encoded event.
func Decode(raw []byte) (*Event, EventID, error) {
	if len(raw) > MaxEventSize {
		return nil, EventID{}, errors.New("event too large")
	}
	var e Event
	if err := proto.Unmarshal(raw, &e); err != nil {
		return nil, EventID{}, err
	}
	if err := check(&e); err != nil {
		return nil, EventID{}, err
	}
	return &e, idOf(&e), nil
}

// Encode serialises an event.
func Encode(e *Event) ([]byte, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxEventSize {
		return nil, errors.New("event too large")
	}
	return raw, nil
}

// Strip returns the event without its body (keeping the body's hash), for a deleted message: the
// content is gone but the signature and id still check.
func Strip(e *Event) *Event {
	if e.Body == nil {
		return e
	}
	out := proto.Clone(e).(*Event)
	out.BodyHash = bodyHash(e)
	out.Body = nil
	return out
}

// Stripped reports whether the event's body was removed.
func Stripped(e *Event) bool { return e.Body == nil && len(e.BodyHash) == hashLen }

func validText(s string) bool {
	return len(s) <= MaxTextLen && utf8.ValidString(s)
}

func validEmoji(s string) bool {
	if s == "" || len(s) > maxEmojiLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
