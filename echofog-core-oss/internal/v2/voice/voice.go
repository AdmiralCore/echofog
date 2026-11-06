// Package voice is the voice message of chat/2 (T7.6): a mono Opus clip in an Ogg container, up to
// five minutes. The core knows nothing about voice: a short clip rides inside the chat message, a
// longer one is an ordinary file of files/2. This package only builds and reads the message, strips
// what identifies the recorder from the clip, and keeps the receiver's list without duplicates.
package voice

import (
	"context"
	"errors"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
)

// Bounds.
const (
	MIME           = "audio/ogg"
	FileName       = "voice.ogg"
	MaxDurationMS  = 300_000
	MinWavePoints  = 32
	MaxWavePoints  = 64
	WavePoints     = 48
	MaxInlineAudio = 40_000
	MaxAudioBytes  = 2 << 20
	MetaVersion    = 1
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

// Meta is the body of the message's metadata field (base64url): everything the receiver needs
// besides the audio. Exactly one of Inline and Offer is set.
type Meta struct {
	Version    uint32
	DurationMS uint32
	Wave       []byte
	Inline     []byte
	Offer      *files.Offer
}

func (m Meta) Marshal() []byte { return nil }

func ParseMeta(b []byte) (Meta, error) { return Meta{}, ErrInvalid }

// Params ask the recorder for a clip.
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

// Draft is a voice message ready to send.
type Draft struct {
	Message    *chatpb.PersonalMessageProto
	Share      *files.Share // nil for an inline clip
	Audio      []byte
	DurationMS uint32
}

// Compose builds the message for a recording.
func Compose(rec Recording, sender [32]byte, recipients [][32]byte) (Draft, error) {
	return Draft{Message: &chatpb.PersonalMessageProto{}}, ErrEmpty
}

// Waveform reduces the levels to n points.
func Waveform(levels []byte, n int) []byte { return nil }

// Kind says how the interface shows a chat/2 message.
type Kind int

const (
	KindPlaceholder Kind = iota
	KindText
	KindFile
	KindVoice
)

// Classify tells what a message is. An unknown type or a voice message of a newer version is a
// placeholder, never an error.
func Classify(m *chatpb.PersonalMessageProto) (Kind, Meta, error) {
	return KindVoice, Meta{}, ErrInvalid
}

// PlaceholderText is the neutral text for what this version cannot show.
func PlaceholderText(lang string) string { return "" }

func voiceLabel(lang string) string { return "" }
