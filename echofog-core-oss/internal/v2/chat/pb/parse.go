package chatpb

import "errors"

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
// services.md 4.4 (a classic X3DH init has no KEM part and is refused).
var ErrInvalid = errors.New("chat/pb: invalid message")

// ParseSecureMessage parses a canonical SecureMessage with a 32-byte ratchet key and a ciphertext of
// MinCiphertext..MaxCiphertext bytes.
func ParseSecureMessage(b []byte) (*SecureMessage, error) { return nil, errors.New("not implemented") }

// ParseX3DHInit parses a canonical PQXDH init: ephemeral and identity keys of 32 bytes, an optional
// one-time prekey id of 32 bytes, the KEM ciphertext (1088 bytes) and its key id (32 bytes), and
// the first message. A classic init (no KEM part) is refused (services.md 4.4); sender_bundle and
// bundle_created_at must be absent.
func ParseX3DHInit(b []byte) (*X3DHInit, error) { return nil, errors.New("not implemented") }

// ParseBundleGet parses a BUNDLE_GET body.
func ParseBundleGet(b []byte) (*BundleGet, error) { return nil, errors.New("not implemented") }

// ParseBundle parses a BUNDLE body: a request id and a prekeys/2 envelope of at most MaxPrekeys bytes.
func ParseBundle(b []byte) (*Bundle, error) { return nil, errors.New("not implemented") }
