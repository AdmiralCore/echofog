package media

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
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
	ErrBadAuth        = errors.New("media v3: bad CALL_AUTH")
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

const (
	senderKeyInfo = "echofog/call2/sender-key"
	epochInfo     = "echofog/call2/epoch"
	unitHashLabel = "echofog/call2/unit"

	maxPendingUnits3 = 128
	pendingTTL3      = time.Second
)

// Marshal returns the 13 header bytes.
func (h Header3) Marshal() []byte {
	b := make([]byte, Header3Len)
	b[0], b[1], b[2], b[3], b[4] = h.Kind, h.Epoch, h.Sender, h.Stream, h.Gen
	binary.BigEndian.PutUint32(b[5:9], h.Seq)
	binary.BigEndian.PutUint32(b[9:13], h.TS)
	return b
}

// ParseHeader3 reads a 13-byte header.
func ParseHeader3(b []byte) (Header3, error) {
	if len(b) != Header3Len {
		return Header3{}, ErrBadHeader3
	}
	return Header3{
		Kind: b[0], Epoch: b[1], Sender: b[2], Stream: b[3], Gen: b[4],
		Seq: binary.BigEndian.Uint32(b[5:9]), TS: binary.BigEndian.Uint32(b[9:13]),
	}, nil
}

// Encode returns the datagram payload.
func (f Fragment3) Encode() []byte {
	b := make([]byte, 0, Header3Len+frag3FieldsLen+len(f.Payload))
	b = append(b, f.Header.Marshal()...)
	b = append(b, f.Idx, f.Count)
	return append(b, f.Payload...)
}

// DecodeFragment3 parses a datagram payload; Payload aliases b.
func DecodeFragment3(b []byte) (Fragment3, error) {
	if len(b) < Header3Len+frag3FieldsLen {
		return Fragment3{}, ErrBadFragment3
	}
	h, err := ParseHeader3(b[:Header3Len])
	if err != nil {
		return Fragment3{}, err
	}
	f := Fragment3{Header: h, Idx: b[Header3Len], Count: b[Header3Len+1], Payload: b[Header3Len+frag3FieldsLen:]}
	if f.Count == 0 || f.Count > MaxFragments3 || f.Idx >= f.Count {
		return Fragment3{}, ErrBadFragment3
	}
	return f, nil
}

// Epoch0Secret is HKDF-SHA256(room secret, info = "echofog/call2/epoch" || u32(0)).
func Epoch0Secret(room []byte) []byte {
	info := binary.BigEndian.AppendUint32([]byte(epochInfo), 0)
	k, err := hkdf.Key(sha256.New, room, nil, string(info), 32)
	if err != nil {
		return nil
	}
	return k
}

// SenderKey3 is the per-sender, per-join key (services.md 9.2).
func SenderKey3(epochSecret []byte, device [32]byte, joinSalt []byte) ([]byte, error) {
	if len(epochSecret) != 32 || len(joinSalt) != JoinSalt3Len {
		return nil, ErrBadKey3
	}
	info := append(append([]byte(senderKeyInfo), device[:]...), joinSalt...)
	return hkdf.Key(sha256.New, epochSecret, nil, string(info), 32)
}

// Nonce3 is stream || kind || 0x0000 || seq || 0x00000000.
func Nonce3(h Header3) [12]byte {
	var n [12]byte
	n[0], n[1] = h.Stream, h.Kind
	binary.BigEndian.PutUint32(n[4:8], h.Seq)
	return n
}

func gcm3(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrBadKey3
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrBadKey3
	}
	return cipher.NewGCM(blk)
}

// Seal3 returns ciphertext || tag; the nonce comes from the header and is not sent.
func Seal3(h Header3, key, plain []byte) ([]byte, error) {
	g, err := gcm3(key)
	if err != nil {
		return nil, err
	}
	n := Nonce3(h)
	return g.Seal(nil, n[:], plain, h.Marshal()), nil
}

// Open3 reverses Seal3.
func Open3(h Header3, key, ct []byte) ([]byte, error) {
	g, err := gcm3(key)
	if err != nil {
		return nil, err
	}
	n := Nonce3(h)
	pt, err := g.Open(nil, n[:], ct, h.Marshal())
	if err != nil {
		return nil, ErrOpen3
	}
	return pt, nil
}

// Split3 cuts a sealed unit into at most 16 fragments of near-equal size.
func Split3(h Header3, ct []byte) ([]Fragment3, error) {
	count := (len(ct) + MaxFragmentPayload3 - 1) / MaxFragmentPayload3
	if count == 0 {
		count = 1
	}
	if count > MaxFragments3 {
		return nil, ErrUnitTooLarge3
	}
	size := (len(ct) + count - 1) / count
	out := make([]Fragment3, count)
	for i := range out {
		lo, hi := min(i*size, len(ct)), min((i+1)*size, len(ct))
		out[i] = Fragment3{Header: h, Idx: byte(i), Count: byte(count), Payload: ct[lo:hi]}
	}
	return out, nil
}

// UnitHash3 is SHA-256("echofog/call2/unit" || header || ciphertext || tag)[0:16].
func UnitHash3(h Header3, ct []byte) [UnitHash3Len]byte {
	d := sha256.New()
	d.Write([]byte(unitHashLabel))
	d.Write(h.Marshal())
	d.Write(ct)
	var out [UnitHash3Len]byte
	copy(out[:], d.Sum(nil))
	return out
}

type partial3 struct {
	count byte
	have  byte
	parts [][]byte
	at    time.Time
}

// Reassembler3 joins fragments of one unit; bounded in units and time. It is not safe for
// concurrent use: one per receive loop.
type Reassembler3 struct {
	now     func() time.Time
	pending map[Header3]*partial3
}

// NewReassembler3 returns a reassembler on the given clock.
func NewReassembler3(now func() time.Time) *Reassembler3 {
	return &Reassembler3{now: now, pending: map[Header3]*partial3{}}
}

// Add returns the sealed unit once its last fragment arrived. Incomplete units older than one
// second are dropped, and at most 128 are held: the oldest goes first.
func (r *Reassembler3) Add(f Fragment3) (Header3, []byte, bool) {
	now := r.now()
	if f.Count == 1 {
		return f.Header, append([]byte(nil), f.Payload...), true
	}
	p := r.pending[f.Header]
	if p != nil && now.Sub(p.at) > pendingTTL3 {
		delete(r.pending, f.Header)
		p = nil
	}
	if p == nil {
		if len(r.pending) >= maxPendingUnits3 {
			r.evict(now)
		}
		p = &partial3{count: f.Count, parts: make([][]byte, f.Count), at: now}
		r.pending[f.Header] = p
	}
	if p.count != f.Count || f.Idx >= p.count || p.parts[f.Idx] != nil {
		return Header3{}, nil, false
	}
	p.parts[f.Idx] = append([]byte{}, f.Payload...)
	p.have++
	if p.have < p.count {
		return Header3{}, nil, false
	}
	delete(r.pending, f.Header)
	var out []byte
	for _, part := range p.parts {
		out = append(out, part...)
	}
	return f.Header, out, true
}

func (r *Reassembler3) evict(now time.Time) {
	var oldest Header3
	var oldestAt time.Time
	first := true
	for k, p := range r.pending {
		if now.Sub(p.at) > pendingTTL3 {
			delete(r.pending, k)
			continue
		}
		if first || p.at.Before(oldestAt) {
			oldest, oldestAt, first = k, p.at, false
		}
	}
	if len(r.pending) >= maxPendingUnits3 && !first {
		delete(r.pending, oldest)
	}
}
