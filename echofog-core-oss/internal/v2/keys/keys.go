// Package keys implements the algorithm-tagged keys of protocol v2 (docs/protocol/v2/objects.md §1,
// registries.md §8): akey = alg(u8) ‖ key bytes, with the length fixed by alg.
package keys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"filippo.io/edwards25519"
)

// Alg is an algorithm tag (registries.md §8).
type Alg uint8

const (
	Ed25519        Alg = 0x01
	X25519         Alg = 0x02
	MLKEM768       Alg = 0x03
	MLDSA65        Alg = 0x10 // reserved
	Ed25519MLDSA65 Alg = 0x11 // reserved: hybrid ed25519+mldsa65
)

var (
	// ErrUnknownAlg reports a tag this implementation does not know. Objects or keys with an unknown
	// tag are unusable (skipped), not malformed (registries.md §8).
	ErrUnknownAlg = errors.New("keys: unknown algorithm tag")
	// ErrMalformed reports a key whose length does not match its tag, or another encoding error.
	ErrMalformed = errors.New("keys: malformed key")
	// ErrWrongKind reports a key of the wrong kind (for example a KEM key where a signature key is needed).
	ErrWrongKind = errors.New("keys: wrong key kind")
	// ErrSmallOrder reports an Ed25519 public key of small order or with a non-canonical encoding.
	ErrSmallOrder = errors.New("keys: Ed25519 key of small order or non-canonical")
	// ErrBadSignature reports a signature that does not verify.
	ErrBadSignature = errors.New("keys: bad signature")
)

type algInfo struct {
	name   string
	keyLen int
	sigLen int // signature algorithms only; 0 otherwise
	known  bool
	signs  bool
}

var algs = map[Alg]algInfo{
	Ed25519:        {"ed25519", 32, 64, true, true},
	X25519:         {"x25519", 32, 0, true, false},
	MLKEM768:       {"mlkem768", 1184, 0, true, false},
	MLDSA65:        {"mldsa65", 1952, 3309, false, true},
	Ed25519MLDSA65: {"ed25519+mldsa65", 32 + 1952, 64 + 3309, false, true},
}

// String returns the registry name of the tag, or "alg(0x..)" for an unknown tag.
func (a Alg) String() string {
	if i, ok := algs[a]; ok {
		return i.name
	}
	return fmt.Sprintf("alg(0x%02x)", uint8(a))
}

// Known reports whether the tag is implemented in v2 (reserved tags are not).
func (a Alg) Known() bool { return algs[a].known }

// IsSignature reports whether the tag is an implemented signature algorithm.
func (a Alg) IsSignature() bool { i := algs[a]; return i.known && i.signs }

// KeyLen returns the key length of the tag, or 0 if the tag is not registered.
func (a Alg) KeyLen() int { return algs[a].keyLen }

// SigLen returns the signature length of a signature tag, or 0.
func (a Alg) SigLen() int { return algs[a].sigLen }

// AKey is an algorithm-tagged key in its wire form.
type AKey []byte

// Parse checks the tag and length of an akey and returns it. An unknown or reserved tag gives
// ErrUnknownAlg.
func Parse(b []byte) (AKey, error) {
	if len(b) < 1 {
		return nil, ErrMalformed
	}
	a := Alg(b[0])
	if !a.Known() {
		return nil, ErrUnknownAlg
	}
	if len(b) != 1+a.KeyLen() {
		return nil, fmt.Errorf("%w: %s key of %d bytes", ErrMalformed, a, len(b)-1)
	}
	return AKey(b), nil
}

// ParseKind parses an akey and checks that it has the tag want.
func ParseKind(b []byte, want Alg) (AKey, error) {
	k, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if k.Alg() != want {
		return nil, fmt.Errorf("%w: %s, want %s", ErrWrongKind, k.Alg(), want)
	}
	return k, nil
}

// New builds an akey from a tag and the raw key bytes.
func New(a Alg, key []byte) AKey {
	out := make([]byte, 1+len(key))
	out[0] = byte(a)
	copy(out[1:], key)
	return out
}

// Alg returns the tag of the key.
func (k AKey) Alg() Alg { return Alg(k[0]) }

// Key returns the key bytes without the tag.
func (k AKey) Key() []byte { return k[1:] }

// ID returns SHA-256(akey): the node ID of an account key, the device ID of a device key (D1, D35).
func (k AKey) ID() [32]byte { return sha256.Sum256(k) }

// Equal reports whether two akeys are identical.
func (k AKey) Equal(o AKey) bool { return bytes.Equal(k, o) }

// Text returns the text form "<alg name>:<base64url-nopad(key bytes)>".
func (k AKey) Text() string {
	return k.Alg().String() + ":" + base64.RawURLEncoding.EncodeToString(k.Key())
}

// ParseText parses the text form of a key.
func ParseText(s string) (AKey, error) {
	name, b64, ok := strings.Cut(s, ":")
	if !ok {
		return nil, ErrMalformed
	}
	for a, i := range algs {
		if i.name == name {
			raw, err := base64.RawURLEncoding.Strict().DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
			}
			return Parse(New(a, raw))
		}
	}
	return nil, ErrUnknownAlg
}

// CheckEd25519Public rejects Ed25519 public keys that are not canonical encodings of a point or
// that have small order (objects.md §2.2 step 3).
func CheckEd25519Public(pub []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrMalformed
	}
	p, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		return ErrSmallOrder
	}
	if !bytes.Equal(p.Bytes(), pub) {
		return ErrSmallOrder
	}
	if new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 1 {
		return ErrSmallOrder
	}
	return nil
}

// Verify checks sig over msg with the signature key k (RFC 8032 §5.1.7 with S < L for Ed25519;
// small-order keys are rejected).
func Verify(k AKey, msg, sig []byte) error {
	switch k.Alg() {
	case Ed25519:
		if err := CheckEd25519Public(k.Key()); err != nil {
			return err
		}
		if len(sig) != ed25519.SignatureSize {
			return ErrBadSignature
		}
		// crypto/ed25519 rejects S >= L (it requires a canonical scalar).
		if !ed25519.Verify(ed25519.PublicKey(k.Key()), msg, sig) {
			return ErrBadSignature
		}
		return nil
	default:
		if !k.Alg().Known() {
			return ErrUnknownAlg
		}
		return ErrWrongKind
	}
}

// Signer is a private signature key.
type Signer struct {
	priv ed25519.PrivateKey
	pub  AKey
}

// NewEd25519Signer returns the signer for a 32-byte Ed25519 seed.
func NewEd25519Signer(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, ErrMalformed
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: New(Ed25519, priv.Public().(ed25519.PublicKey))}, nil
}

// Public returns the akey of the signer.
func (s *Signer) Public() AKey { return s.pub }

// Alg returns the signature algorithm of the signer.
func (s *Signer) Alg() Alg { return s.pub.Alg() }

// Sign signs msg.
func (s *Signer) Sign(msg []byte) []byte { return ed25519.Sign(s.priv, msg) }
