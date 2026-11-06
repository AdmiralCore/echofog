package media

import (
	"bytes"
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

	pendingTTL3 = time.Second
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

const (
	maxVariants3          = 4  // distinct payloads kept per fragment index
	maxAssemblies3        = 64 // combinations tried per added fragment
	maxPendingPerSource3  = 16 // incomplete units per (sender, gen)
	maxCompletedPerSource = 32 // completed units kept to catch late variants
)

type partial3 struct {
	count     byte
	variants  [][][]byte // per index, up to maxVariants3 distinct payloads
	missing   int        // indexes with no payload yet
	returned  map[[UnitHash3Len]byte]bool
	completed bool
	at        time.Time
}

type srcID struct{ sender, gen byte }

// Reassembler3 joins fragments of one unit. Fragments are not authenticated one by one, so a
// forged fragment with the real header must not shadow the real one: up to four variants are
// kept per index and every distinct assembly is returned, for the receiver to match against
// the CALL_AUTH. State is bounded per sender, so one member cannot evict another's units.
// Not safe for concurrent use: one per receive loop.
type Reassembler3 struct {
	// Known says whether the sender announced a media key; fragments of others are dropped before
	// any state is created. Nil accepts all.
	Known   func(sender, gen byte) bool
	now     func() time.Time
	pending map[Header3]*partial3
}

// NewReassembler3 returns a reassembler on the given clock.
func NewReassembler3(now func() time.Time) *Reassembler3 {
	return &Reassembler3{now: now, pending: map[Header3]*partial3{}}
}

// Assembled3 is a sealed unit put together from fragments.
type Assembled3 struct {
	Header     Header3
	Ciphertext []byte
}

// Add returns the first sealed unit the fragment completed.
func (r *Reassembler3) Add(f Fragment3) (Header3, []byte, bool) {
	if out := r.AddAll(f); len(out) > 0 {
		return out[0].Header, out[0].Ciphertext, true
	}
	return Header3{}, nil, false
}

// AddAll takes one fragment and returns every sealed unit it completed.
func (r *Reassembler3) AddAll(f Fragment3) []Assembled3 {
	if r.Known != nil && !r.Known(f.Header.Sender, f.Header.Gen) {
		return nil
	}
	if f.Count == 1 {
		return []Assembled3{{f.Header, append([]byte(nil), f.Payload...)}}
	}
	if f.Count == 0 || f.Count > MaxFragments3 || f.Idx >= f.Count {
		return nil
	}
	now := r.now()
	p := r.pending[f.Header]
	if p != nil && now.Sub(p.at) > pendingTTL3 {
		delete(r.pending, f.Header)
		p = nil
	}
	if p == nil {
		r.makeRoom(f.Header, now)
		p = &partial3{count: f.Count, variants: make([][][]byte, f.Count), missing: int(f.Count),
			returned: map[[UnitHash3Len]byte]bool{}, at: now}
		r.pending[f.Header] = p
	}
	if p.count != f.Count {
		return nil
	}
	vs := p.variants[f.Idx]
	for _, v := range vs {
		if bytes.Equal(v, f.Payload) {
			return nil
		}
	}
	if len(vs) >= maxVariants3 {
		return nil
	}
	if len(vs) == 0 {
		p.missing--
	}
	p.variants[f.Idx] = append(vs, append([]byte{}, f.Payload...))
	if p.missing > 0 {
		return nil
	}
	return r.assemble(f.Header, p)
}

// assemble tries the combinations of variants and returns those not returned before.
func (r *Reassembler3) assemble(h Header3, p *partial3) []Assembled3 {
	var out []Assembled3
	pick := make([]int, p.count)
	for tried := 0; tried < maxAssemblies3; tried++ {
		var ct []byte
		for i, v := range pick {
			ct = append(ct, p.variants[i][v]...)
		}
		if k := UnitHash3(h, ct); !p.returned[k] {
			p.returned[k] = true
			out = append(out, Assembled3{h, ct})
		}
		i := 0 // odometer over the variants of each index
		for ; i < len(pick); i++ {
			if pick[i]++; pick[i] < len(p.variants[i]) {
				break
			}
			pick[i] = 0
		}
		if i == len(pick) {
			break
		}
	}
	p.completed = true
	return out
}

// makeRoom frees a slot for a new unit of h's sender: expired entries first, then the oldest of
// the same sender. Other senders' units are never touched.
func (r *Reassembler3) makeRoom(h Header3, now time.Time) {
	var pend, done int
	var oldestPend, oldestDone *Header3
	var atPend, atDone time.Time
	for k, p := range r.pending {
		if now.Sub(p.at) > pendingTTL3 {
			delete(r.pending, k)
			continue
		}
		if k.Sender != h.Sender || k.Gen != h.Gen {
			continue
		}
		kk := k
		if p.completed {
			done++
			if oldestDone == nil || p.at.Before(atDone) {
				oldestDone, atDone = &kk, p.at
			}
		} else {
			pend++
			if oldestPend == nil || p.at.Before(atPend) {
				oldestPend, atPend = &kk, p.at
			}
		}
	}
	if pend >= maxPendingPerSource3 && oldestPend != nil {
		delete(r.pending, *oldestPend)
	}
	if done >= maxCompletedPerSource && oldestDone != nil {
		delete(r.pending, *oldestDone)
	}
}
