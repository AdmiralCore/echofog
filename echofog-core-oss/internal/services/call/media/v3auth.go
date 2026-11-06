package media

import (
	"crypto/ed25519"
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

// Marshal returns every byte before the signature.
func (a *Auth) Marshal() []byte { return nil }

// Sign signs the CALL_AUTH with the per-call media key and returns the payload.
func (a *Auth) Sign(priv ed25519.PrivateKey) []byte { return nil }

// Verify checks the signature against the sender's announced media key.
func (a *Auth) Verify(pub ed25519.PublicKey) bool { return false }

// DecodeAuth parses a CALL_AUTH payload.
func DecodeAuth(b []byte) (*Auth, error) { return nil, ErrNotImplemented }

// Batcher groups consecutive units of one (sender, stream, kind) and signs them in batches.
type Batcher struct{}

// NewBatcher returns a batcher that signs with the per-call media key.
func NewBatcher(priv ed25519.PrivateKey, now func() time.Time) *Batcher { return &Batcher{} }

// Add records one sealed unit. It returns the CALL_AUTH payload of every batch it closed
// (the previous one when it had expired, then this one when it filled).
func (b *Batcher) Add(h Header3, ct []byte) [][]byte { return nil }

// Tick closes the open batch when it is older than BatchMaxAge.
func (b *Batcher) Tick() [][]byte { return nil }
