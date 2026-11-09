// Package voice is the voice message of chat/2 (T7.6): a mono Opus clip in an Ogg container, up to
// five minutes. The core knows nothing about voice: a short clip rides inside the chat message, a
// longer one is an ordinary file of files/2. This package only builds and reads the message, strips
// what identifies the recorder from the clip, and keeps the receiver's list without duplicates.
//
// The message is a PersonalMessageProto of the existing type VOICE. Its metadata field holds
// (base64url) a Meta: version, length, waveform and either the clip itself or a files/2 offer. No
// field of chat/2 or files/2 is added: the offer is the unchanged files Offer, and the voice data
// live in the metadata string the message already has.
//
// What leaves the node says nothing about the recorder: the file name is fixed, the text and the
// timestamp are empty, and Sanitize removes the vendor, comments, sample rate and serial from the
// Ogg headers.
package voice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"google.golang.org/protobuf/encoding/protowire"
)

// Bounds.
const (
	MIME          = "audio/ogg"
	FileName      = "voice.ogg"
	MaxDurationMS = 300_000
	MinWavePoints = 32
	MaxWavePoints = 64
	WavePoints    = 48 // every clip sent by this version has the same number of points
	// MaxInlineAudio is the largest clip that rides inside the message: in base64 with the waveform
	// and the fields of the message it stays below the largest chat/2 bucket (57344 bytes).
	MaxInlineAudio = 40_000
	MaxAudioBytes  = 2 << 20
	MetaVersion    = 1

	// durationSlackMS is how far the length in the container may differ from the one in the message.
	durationSlackMS = 200
	// maxMetadataChars bounds the metadata field before it is decoded.
	maxMetadataChars = 64 << 10
)

var (
	ErrEmpty       = errors.New("voice: empty recording")
	ErrTooLong     = errors.New("voice: recording is longer than five minutes")
	ErrInvalid     = errors.New("voice: invalid voice message")
	ErrUnsupported = errors.New("voice: message of a newer version")
	ErrOgg         = errors.New("voice: not a plain mono Opus stream")
	ErrDuration    = errors.New("voice: the clip does not last as long as the message says")
	ErrTooLarge    = errors.New("voice: clip is too large")
)

func invalid(what string) error { return fmt.Errorf("%w: %s", ErrInvalid, what) }

// Meta is the body of the message's metadata field: everything the receiver needs besides the
// audio. Exactly one of Inline and Offer is set. Fields: 1 version, 2 duration in ms, 3 waveform
// (one byte per point), 4 inline clip, 5 files/2 offer.
type Meta struct {
	Version    uint32
	DurationMS uint32
	Wave       []byte
	Inline     []byte
	Offer      *files.Offer
}

// Marshal encodes the meta canonically (fields in order, empty ones omitted).
func (m Meta) Marshal() []byte {
	var b []byte
	b = appendVarint(b, 1, uint64(m.Version))
	b = appendVarint(b, 2, uint64(m.DurationMS))
	b = appendBytes(b, 3, m.Wave)
	b = appendBytes(b, 4, m.Inline)
	if m.Offer != nil {
		b = appendBytes(b, 5, m.Offer.Marshal())
	}
	return b
}

func appendVarint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), v)
}

func appendBytes(b []byte, num protowire.Number, v []byte) []byte {
	if len(v) == 0 {
		return b
	}
	return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), v)
}

// A clip of d milliseconds takes at most 16 KiB and 64 kbit/s: a voice message at 32 kbit/s is far
// below that, and a claim of a short clip cannot hide a big file.
func maxBytesFor(durationMS uint32) uint64 { return 16<<10 + uint64(durationMS)*8 }

// ParseMeta decodes and validates the body of the metadata field. Fields it does not know are
// skipped; a version it does not know is ErrUnsupported (the interface shows a placeholder).
func ParseMeta(b []byte) (Meta, error) {
	var m Meta
	var seen [6]bool
	for rest := b; len(rest) > 0; {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 || num < 1 {
			return Meta{}, invalid("tag")
		}
		rest = rest[n:]
		l := protowire.ConsumeFieldValue(num, typ, rest)
		if l < 0 {
			return Meta{}, invalid("field")
		}
		val := rest[:l]
		rest = rest[l:]
		if num > 5 {
			continue // a later version's field
		}
		if seen[num] {
			return Meta{}, invalid("repeated field")
		}
		seen[num] = true
		switch num {
		case 1, 2:
			if typ != protowire.VarintType {
				return Meta{}, invalid("field type")
			}
			v, _ := protowire.ConsumeVarint(val)
			if v > math.MaxUint32 {
				return Meta{}, invalid("number")
			}
			if num == 1 {
				m.Version = uint32(v)
			} else {
				m.DurationMS = uint32(v)
			}
		default:
			if typ != protowire.BytesType {
				return Meta{}, invalid("field type")
			}
			v, _ := protowire.ConsumeBytes(val)
			switch num {
			case 3:
				m.Wave = append([]byte(nil), v...)
			case 4:
				m.Inline = append([]byte(nil), v...)
			case 5:
				o, err := files.ParseOffer(v)
				if err != nil {
					return Meta{}, invalid("offer")
				}
				m.Offer = &o
			}
		}
	}
	switch {
	case m.Version == 0:
		return Meta{}, invalid("version")
	case m.Version > MetaVersion:
		return Meta{}, ErrUnsupported
	case m.DurationMS == 0 || m.DurationMS > MaxDurationMS:
		return Meta{}, invalid("duration")
	case len(m.Wave) < MinWavePoints || len(m.Wave) > MaxWavePoints:
		return Meta{}, invalid("waveform")
	case (len(m.Inline) == 0) == (m.Offer == nil):
		return Meta{}, invalid("the clip is either inline or a file")
	case len(m.Inline) > MaxInlineAudio:
		return Meta{}, invalid("inline clip")
	}
	if o := m.Offer; o != nil {
		if o.Mime != MIME || o.Name != FileName || o.Size == 0 || o.Size > MaxAudioBytes || o.Size > maxBytesFor(m.DurationMS) {
			return Meta{}, invalid("offer")
		}
	}
	return m, nil
}

// Params ask the recorder for a clip: Opus, mono, 24-32 kbit/s, at most five minutes.
type Params struct {
	Bitrate     int
	MaxDuration time.Duration
}

// Recording is what the recorder returns: an Ogg Opus clip and the loudness of every 20 ms.
type Recording struct {
	Ogg    []byte
	Levels []byte
}

// Recorder captures the microphone. The real one is the desktop's; tests use a fake.
type Recorder interface {
	Record(ctx context.Context, p Params) (Recording, error)
}

// Player plays a clip at a speed.
type Player interface {
	Play(ctx context.Context, ogg []byte, s Speed) error
}

// Draft is a voice message ready to send. Share is nil for an inline clip; otherwise the caller
// gives it to files.Server.Add and sends Message.
type Draft struct {
	Message    *chatpb.PersonalMessageProto
	Share      *files.Share
	Audio      []byte
	DurationMS uint32
}

// Compose builds the message for a recording: it cleans the clip, checks its length, and sends it
// inline when it fits the message, else as a file shared with the recipients.
func Compose(rec Recording, sender [32]byte, recipients [][32]byte) (Draft, error) {
	fail := func(err error) (Draft, error) { return Draft{Message: &chatpb.PersonalMessageProto{}}, err }
	if len(rec.Ogg) == 0 {
		return fail(ErrEmpty)
	}
	clean, ms, err := Sanitize(rec.Ogg)
	if err != nil {
		return fail(err)
	}
	if ms > MaxDurationMS {
		return fail(ErrTooLong)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fail(err)
	}
	meta := Meta{Version: MetaVersion, DurationMS: ms, Wave: Waveform(rec.Levels, WavePoints)}
	d := Draft{Audio: clean, DurationMS: ms}
	msg := &chatpb.PersonalMessageProto{Id: hex.EncodeToString(id[:]), SenderId: hex.EncodeToString(sender[:]),
		Type: chatpb.PersonalMessageType_MSG_TYPE_VOICE}
	if len(clean) <= MaxInlineAudio {
		meta.Inline = clean
	} else {
		if len(recipients) == 0 {
			return fail(invalid("no recipients"))
		}
		sh, err := files.PrepareShare(bytes.NewReader(clean), int64(len(clean)), FileName, MIME, sender, recipients)
		if err != nil {
			return fail(err)
		}
		meta.Offer = &sh.Offer
		msg.ContentHash = hex.EncodeToString(sh.Offer.Root[:])
		d.Share = &sh
	}
	raw := meta.Marshal()
	if _, err := ParseMeta(raw); err != nil { // never send what a receiver would refuse
		return fail(fmt.Errorf("%w: %v", ErrTooLarge, err))
	}
	msg.Metadata = base64.RawURLEncoding.EncodeToString(raw)
	if _, err := chatpb.PadPersonal(msg); err != nil {
		return fail(fmt.Errorf("%w: %v", ErrTooLarge, err))
	}
	d.Message = msg
	return d, nil
}

// Waveform reduces the levels (one per 20 ms) to n points, the loudest level of each stretch.
func Waveform(levels []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	l := len(levels)
	if l == 0 {
		return out
	}
	for i := range out {
		lo := i * l / n
		hi := max((i+1)*l/n, lo+1)
		for _, v := range levels[lo:min(hi, l)] {
			out[i] = max(out[i], v)
		}
	}
	return out
}

// Kind says how the interface shows a chat/2 message.
type Kind int

const (
	KindPlaceholder Kind = iota
	KindText
	KindFile
	KindVoice
)

// Classify tells what a message is. A type this version does not handle and a voice message of a
// newer version are placeholders, not errors (versioning.md 3.9); a damaged voice message is an error.
func Classify(m *chatpb.PersonalMessageProto) (Kind, Meta, error) {
	if m == nil {
		return KindPlaceholder, Meta{}, invalid("no message")
	}
	switch m.Type {
	case chatpb.PersonalMessageType_MSG_TYPE_TEXT:
		return KindText, Meta{}, nil
	case chatpb.PersonalMessageType_MSG_TYPE_FILE:
		return KindFile, Meta{}, nil
	case chatpb.PersonalMessageType_MSG_TYPE_VOICE:
		meta, err := metaOf(m)
		switch {
		case errors.Is(err, ErrUnsupported):
			return KindPlaceholder, Meta{}, nil
		case err != nil:
			return KindPlaceholder, Meta{}, err
		}
		return KindVoice, meta, nil
	}
	return KindPlaceholder, Meta{}, nil
}

func metaOf(m *chatpb.PersonalMessageProto) (Meta, error) {
	if len(m.Metadata) > maxMetadataChars {
		return Meta{}, invalid("metadata")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(m.Metadata)
	if err != nil {
		return Meta{}, invalid("metadata")
	}
	return ParseMeta(raw)
}

// PlaceholderText is the neutral text for what this version cannot show (ru, else en).
func PlaceholderText(lang string) string {
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		return "Сообщение из новой версии приложения — обновите приложение"
	}
	return "Message from a newer version of the app — please update"
}

func voiceLabel(lang string) string {
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		return "Голосовое сообщение"
	}
	return "Voice message"
}
