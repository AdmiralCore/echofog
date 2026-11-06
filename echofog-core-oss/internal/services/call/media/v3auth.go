package media

import (
	"crypto/ed25519"
	"encoding/binary"
	"time"
)

const (
	// AuthSigLen3 is the Ed25519 signature that ends a CALL_AUTH.
	AuthSigLen3 = ed25519.SignatureSize
	// BatchAudioUnits and BatchVideoUnits close a batch (services.md 9.3, D39).
	BatchAudioUnits = 5
	BatchVideoUnits = 8
	BatchMaxAge     = 100 * time.Millisecond
	maxAuthHashes   = 16
)

// Auth is one CALL_AUTH datagram (type 0x13), without the type byte.
type Auth struct {
	Kind, Epoch, Sender, Stream, Gen byte
	FirstSeq                         uint32
	// Prev are the hashes of the previous batch (first_seq-len(Prev) ...), Cur those of this one.
	Prev, Cur [][UnitHash3Len]byte
	Sig       []byte
}

// authLabel prefixes every signed CALL_AUTH (registries.md 7.5).
const authLabel = "echofog/call2/batch"

// authFixedLen is kind, epoch, sender, stream, gen, first_seq, prev_count, count.
const authFixedLen = 5 + 4 + 1 + 1

// Marshal returns every byte before the signature.
func (a *Auth) Marshal() []byte {
	b := make([]byte, 0, authFixedLen+(len(a.Prev)+len(a.Cur))*UnitHash3Len)
	b = append(b, a.Kind, a.Epoch, a.Sender, a.Stream, a.Gen)
	b = binary.BigEndian.AppendUint32(b, a.FirstSeq)
	b = append(b, byte(len(a.Prev)), byte(len(a.Cur)))
	for _, h := range a.Prev {
		b = append(b, h[:]...)
	}
	for _, h := range a.Cur {
		b = append(b, h[:]...)
	}
	return b
}

func (a *Auth) signedBytes() []byte { return append([]byte(authLabel), a.Marshal()...) }

// Sign signs the CALL_AUTH with the per-call media key and returns the payload.
func (a *Auth) Sign(priv ed25519.PrivateKey) []byte {
	a.Sig = ed25519.Sign(priv, a.signedBytes())
	return append(a.Marshal(), a.Sig...)
}

// Verify checks the signature against the sender's announced media key.
func (a *Auth) Verify(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && len(a.Sig) == AuthSigLen3 &&
		ed25519.Verify(pub, a.signedBytes(), a.Sig)
}

// DecodeAuth parses a CALL_AUTH payload.
func DecodeAuth(b []byte) (*Auth, error) {
	if len(b) < authFixedLen+UnitHash3Len+AuthSigLen3 {
		return nil, ErrBadAuth
	}
	a := &Auth{Kind: b[0], Epoch: b[1], Sender: b[2], Stream: b[3], Gen: b[4],
		FirstSeq: binary.BigEndian.Uint32(b[5:9])}
	prev, cur := int(b[9]), int(b[10])
	if prev > maxAuthHashes || cur < 1 || cur > maxAuthHashes ||
		len(b) != authFixedLen+(prev+cur)*UnitHash3Len+AuthSigLen3 {
		return nil, ErrBadAuth
	}
	off := authFixedLen
	read := func(n int) [][UnitHash3Len]byte {
		out := make([][UnitHash3Len]byte, n)
		for i := range out {
			copy(out[i][:], b[off:off+UnitHash3Len])
			off += UnitHash3Len
		}
		return out
	}
	a.Prev = read(prev)
	a.Cur = read(cur)
	a.Sig = append([]byte(nil), b[off:]...)
	return a, nil
}

// batchLimit is the number of units that closes a batch of this kind.
func batchLimit(kind byte) int {
	if kind == Kind3Audio || kind == Kind3ScreenAudio {
		return BatchAudioUnits
	}
	return BatchVideoUnits
}

// Batcher groups consecutive units of one (sender, stream, kind) and signs them in batches. It is
// not safe for concurrent use.
type Batcher struct {
	priv ed25519.PrivateKey
	now  func() time.Time

	cur      []Header3
	curHash  [][UnitHash3Len]byte
	curStart time.Time

	prevFirst uint32
	prevHash  [][UnitHash3Len]byte
	havePrev  bool
}

// NewBatcher returns a batcher that signs with the per-call media key.
func NewBatcher(priv ed25519.PrivateKey, now func() time.Time) *Batcher {
	return &Batcher{priv: priv, now: now}
}

// Add records one sealed unit. It returns the CALL_AUTH payload of every batch it closed
// (the previous one when it had expired or the sequence jumped, then this one when it filled).
func (b *Batcher) Add(h Header3, ct []byte) [][]byte {
	var out [][]byte
	if n := len(b.cur); n > 0 {
		last := b.cur[n-1]
		if h.Seq != last.Seq+1 || b.now().Sub(b.curStart) >= BatchMaxAge {
			out = append(out, b.close())
		}
	}
	if len(b.cur) == 0 {
		b.curStart = b.now()
	}
	b.cur = append(b.cur, h)
	b.curHash = append(b.curHash, UnitHash3(h, ct))
	if len(b.cur) >= batchLimit(h.Kind) {
		out = append(out, b.close())
	}
	return out
}

// Tick closes the open batch when it is older than BatchMaxAge.
func (b *Batcher) Tick() [][]byte {
	if len(b.cur) > 0 && b.now().Sub(b.curStart) >= BatchMaxAge {
		return [][]byte{b.close()}
	}
	return nil
}

func (b *Batcher) close() []byte {
	first := b.cur[0]
	a := &Auth{Kind: first.Kind, Epoch: first.Epoch, Sender: first.Sender, Stream: first.Stream,
		Gen: first.Gen, FirstSeq: first.Seq, Cur: b.curHash}
	// The previous batch rides along only when it ends right before this one.
	if b.havePrev && b.prevFirst+uint32(len(b.prevHash)) == first.Seq {
		a.Prev = b.prevHash
	}
	payload := a.Sign(b.priv)
	b.prevFirst, b.prevHash, b.havePrev = first.Seq, b.curHash, true
	b.cur, b.curHash = nil, nil
	return payload
}
