package chatpb

import (
	"errors"
	"fmt"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"google.golang.org/protobuf/proto"
)

// Sizes fixed by the constructions (services.md 4, objects.md 5).
const (
	KeyLen           = 32   // X25519 public key, one-time prekey id, KEM key id
	KEMCiphertextLen = 1088 // ML-KEM-768 ciphertext
	// MinCiphertext is a XChaCha20-Poly1305 nonce (24 bytes) and tag (16) around an empty plaintext.
	MinCiphertext = 40
	// MaxCiphertext bounds a ratchet ciphertext: every envelope fits in 61440 bytes (services.md 4.3).
	MaxCiphertext = 61440
	// MaxPrekeys is the largest prekeys/2 envelope (registries.md labels table).
	MaxPrekeys = 8192
)

// ErrInvalid is any malformed or non-canonical chat/2 message, or one that breaks a rule of
// services.md 4.4 (a classic X3DH init has no KEM part and is refused). The error never says
// which rule failed to a remote peer; callers answer every ErrInvalid alike (AD20).
var ErrInvalid = errors.New("chat/pb: invalid message")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// parse checks the canonical rules and decodes into m, ignoring unknown fields.
func parse(b []byte, m proto.Message) error {
	if err := objects.CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return invalid("%v", err)
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m); err != nil {
		return invalid("%v", err)
	}
	return nil
}

func checkMessage(m *SecureMessage) error {
	if m == nil || m.GetHeader() == nil {
		return invalid("no header")
	}
	if n := len(m.GetHeader().GetRatchetKey()); n != KeyLen {
		return invalid("ratchet key of %d bytes", n)
	}
	if n := len(m.GetCiphertext()); n < MinCiphertext || n > MaxCiphertext {
		return invalid("ciphertext of %d bytes", n)
	}
	return nil
}

// ParseSecureMessage parses a canonical SecureMessage with a 32-byte ratchet key and a ciphertext of
// MinCiphertext..MaxCiphertext bytes.
func ParseSecureMessage(b []byte) (*SecureMessage, error) {
	m := &SecureMessage{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	if err := checkMessage(m); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseX3DHInit parses a canonical PQXDH init: ephemeral and identity keys of 32 bytes, an optional
// one-time prekey id of 32 bytes, the KEM ciphertext (1088 bytes) and its key id (32 bytes), and
// the first message. A classic init (no KEM part) is refused (services.md 4.4); sender_bundle and
// bundle_created_at must be absent.
func ParseX3DHInit(b []byte) (*X3DHInit, error) {
	m := &X3DHInit{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	if len(m.GetEphemeralKey()) != KeyLen || len(m.GetIdentityKey()) != KeyLen {
		return nil, invalid("key of a wrong length")
	}
	if n := len(m.GetOneTimeKeyId()); n != 0 && n != KeyLen {
		return nil, invalid("one-time prekey id of %d bytes", n)
	}
	if len(m.GetKemCiphertext()) != KEMCiphertextLen || len(m.GetKemKeyId()) != KeyLen {
		return nil, invalid("no KEM part (classic X3DH is refused) or a wrong one")
	}
	if len(m.GetSenderBundle()) != 0 || m.GetBundleCreatedAt() != 0 {
		return nil, invalid("a v1-only field is set")
	}
	if err := checkMessage(m.GetInitialCiphertext()); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseBundleGet parses a BUNDLE_GET body.
func ParseBundleGet(b []byte) (*BundleGet, error) {
	m := &BundleGet{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseBundle parses a BUNDLE body: a request id and a prekeys/2 envelope of at most MaxPrekeys bytes.
func ParseBundle(b []byte) (*Bundle, error) {
	m := &Bundle{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	if n := len(m.GetPrekeys()); n == 0 || n > MaxPrekeys {
		return nil, invalid("prekeys of %d bytes", n)
	}
	return m, nil
}
