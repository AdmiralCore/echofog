package media

import (
	"errors"
	"time"
)

// Media packet v3 (docs/protocol/v2/services.md 9.2) lives beside the v1 format of packet.go; v1
// is untouched. The v3 names carry a "3" so both formats share one package.

const (
	Kind3Audio       byte = 1
	Kind3Video       byte = 2
	Kind3Heartbeat   byte = 3
	Kind3ScreenAudio byte = 4
	Kind3VideoFEC    byte = 6

	Header3Len     = 13
	frag3FieldsLen = 2
	// MaxDatagram3 is the datagram payload floor every path carries (wire.md 6.5).
	MaxDatagram3 = 1232
	// MaxFragmentPayload3 is the ciphertext one v3 fragment carries.
	MaxFragmentPayload3 = MaxDatagram3 - Header3Len - frag3FieldsLen
	MaxFragments3       = 16
	JoinSalt3Len        = 16
	UnitHash3Len        = 16
)

var (
	ErrNotImplemented = errors.New("media v3: not implemented")
	ErrBadHeader3     = errors.New("media v3: bad header")
	ErrBadFragment3   = errors.New("media v3: bad fragment")
	ErrBadKey3        = errors.New("media v3: bad key or salt")
	ErrOpen3          = errors.New("media v3: unit does not open")
	ErrUnitTooLarge3  = errors.New("media v3: unit needs more than 16 fragments")
)

// Header3 is the unit header: also the AEAD additional data.
type Header3 struct {
	Kind, Epoch, Sender, Stream, Gen byte
	Seq, TS                          uint32
}

// Fragment3 is one CALL_AUDIO / CALL_VIDEO datagram payload.
type Fragment3 struct {
	Header     Header3
	Idx, Count byte
	Payload    []byte
}

// Marshal returns the 13 header bytes.
func (h Header3) Marshal() []byte { return nil }

// ParseHeader3 reads a 13-byte header.
func ParseHeader3(b []byte) (Header3, error) { return Header3{}, ErrNotImplemented }

// Encode returns the datagram payload.
func (f Fragment3) Encode() []byte { return nil }

// DecodeFragment3 parses a datagram payload; Payload aliases b.
func DecodeFragment3(b []byte) (Fragment3, error) { return Fragment3{}, ErrNotImplemented }

// Epoch0Secret is HKDF-SHA256(room secret, info = "echofog/call2/epoch" || u32(0)).
func Epoch0Secret(room []byte) []byte { return nil }

// SenderKey3 is the per-sender, per-join key (services.md 9.2).
func SenderKey3(epochSecret []byte, device [32]byte, joinSalt []byte) ([]byte, error) {
	return nil, ErrNotImplemented
}

// Nonce3 is stream || kind || 0x0000 || seq || 0x00000000.
func Nonce3(h Header3) [12]byte { return [12]byte{} }

// Seal3 returns ciphertext || tag; the nonce comes from the header and is not sent.
func Seal3(h Header3, key, plain []byte) ([]byte, error) { return nil, ErrNotImplemented }

// Open3 reverses Seal3.
func Open3(h Header3, key, ct []byte) ([]byte, error) { return nil, ErrNotImplemented }

// Split3 cuts a sealed unit into at most 16 fragments.
func Split3(h Header3, ct []byte) ([]Fragment3, error) { return nil, ErrNotImplemented }

// UnitHash3 is SHA-256("echofog/call2/unit" || header || ciphertext || tag)[0:16].
func UnitHash3(h Header3, ct []byte) [UnitHash3Len]byte { return [UnitHash3Len]byte{} }

// Reassembler3 joins fragments of one unit; bounded in units and time.
type Reassembler3 struct{ now func() time.Time }

// NewReassembler3 returns a reassembler on the given clock.
func NewReassembler3(now func() time.Time) *Reassembler3 { return &Reassembler3{now: now} }

// Add returns the sealed unit once its last fragment arrived.
func (r *Reassembler3) Add(f Fragment3) (Header3, []byte, bool) { return Header3{}, nil, false }
