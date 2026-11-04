// Package noise implements the Noise Protocol Framework (revision 34) pieces that EchoFog protocol
// v2 needs (docs/protocol/v2/handshake.md §2): BLAKE2s-256, ChaCha20-Poly1305, X25519 and the HFS
// tokens e1/ekem1 with ML-KEM-768, for the patterns XKhfs and XXhfs.
//
// Pure Go, standard library plus golang.org/x/crypto; no cgo (platforms.md §1.1).
package noise

import (
	"crypto/cipher"
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"hash"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// HashLen is HASHLEN of BLAKE2s-256.
	HashLen = 32
	// BlockLen is BLOCKLEN of BLAKE2s.
	BlockLen = 64
	// KeyLen is the ChaCha20-Poly1305 key size.
	KeyLen = 32
	// TagLen is the Poly1305 tag size.
	TagLen = 16
	// MaxMessageLen is the largest Noise message (handshake.md §4, §11).
	MaxMessageLen = 16384
)

// Protocol names of the two v2 suites (registries.md §6.2, Decision D3).
const (
	NameXKhfs = "EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s"
	NameXXhfs = "EchoFog/2/Noise_XXhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s"
)

var (
	// ErrDecrypt is returned when an AEAD open fails.
	ErrDecrypt = errors.New("noise: decryption failed")
	// ErrNonceExhausted is returned when the nonce reached 2^64-1 (Noise §5.1).
	ErrNonceExhausted = errors.New("noise: nonce exhausted")
)

func newHash() hash.Hash {
	h, err := blake2s.New256(nil)
	if err != nil {
		panic(err) // unreachable: no key
	}
	return h
}

// Hash is HASH(data...) = BLAKE2s-256 over the concatenation.
func Hash(parts ...[]byte) []byte {
	h := newHash()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// HMAC is HMAC-HASH(key, data...) with BLAKE2s-256 (RFC 2104).
func HMAC(key []byte, parts ...[]byte) []byte {
	m := hmac.New(newHash, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// HKDF2 is the Noise HKDF with two outputs (Noise §4.3).
func HKDF2(ck, ikm []byte) (o1, o2 []byte) {
	t := HMAC(ck, ikm)
	o1 = HMAC(t, []byte{0x01})
	o2 = HMAC(t, o1, []byte{0x02})
	wipe(t)
	return o1, o2
}

// HKDF3 is the Noise HKDF with three outputs (Noise §4.3).
func HKDF3(ck, ikm []byte) (o1, o2, o3 []byte) {
	t := HMAC(ck, ikm)
	o1 = HMAC(t, []byte{0x01})
	o2 = HMAC(t, o1, []byte{0x02})
	o3 = HMAC(t, o2, []byte{0x03})
	wipe(t)
	return o1, o2, o3
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// CipherState is the Noise CipherState (Noise §5.1).
type CipherState struct {
	k      [KeyLen]byte
	hasKey bool
	n      uint64
	aead   cipher.AEAD
}

// InitializeKey sets k and resets n to zero.
func (c *CipherState) InitializeKey(k []byte) {
	if len(k) != KeyLen {
		panic("noise: bad key length")
	}
	copy(c.k[:], k)
	c.hasKey = true
	c.n = 0
	a, err := chacha20poly1305.New(c.k[:])
	if err != nil {
		panic(err) // unreachable: key length checked
	}
	c.aead = a
}

// HasKey reports whether a key is set.
func (c *CipherState) HasKey() bool { return c.hasKey }

// Key returns a copy of the key (nil if none). Used for vectors and for the sealed transport.
func (c *CipherState) Key() []byte {
	if !c.hasKey {
		return nil
	}
	return append([]byte(nil), c.k[:]...)
}

// Nonce returns the next nonce value.
func (c *CipherState) Nonce() uint64 { return c.n }

func nonce(n uint64) []byte {
	var b [12]byte
	binary.LittleEndian.PutUint64(b[4:], n)
	return b[:]
}

// EncryptWithAd appends the encryption of pt to out. Without a key it appends pt unchanged.
func (c *CipherState) EncryptWithAd(out, ad, pt []byte) ([]byte, error) {
	if !c.hasKey {
		return append(out, pt...), nil
	}
	if c.n == ^uint64(0) {
		return nil, ErrNonceExhausted
	}
	out = c.aead.Seal(out, nonce(c.n), pt, ad)
	c.n++
	return out, nil
}

// DecryptWithAd appends the decryption of ct to out. On failure the nonce is not advanced.
func (c *CipherState) DecryptWithAd(out, ad, ct []byte) ([]byte, error) {
	if !c.hasKey {
		return append(out, ct...), nil
	}
	if c.n == ^uint64(0) {
		return nil, ErrNonceExhausted
	}
	out, err := c.aead.Open(out, nonce(c.n), ct, ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	c.n++
	return out, nil
}

// Erase clears the key.
func (c *CipherState) Erase() {
	wipe(c.k[:])
	c.hasKey = false
	c.aead = nil
	c.n = 0
}

// SymmetricState is the Noise SymmetricState (Noise §5.2).
type SymmetricState struct {
	cs CipherState
	ck [HashLen]byte
	h  [HashLen]byte
}

// NewSymmetricState runs InitializeSymmetric(protocolName).
func NewSymmetricState(protocolName string) *SymmetricState {
	s := &SymmetricState{}
	if len(protocolName) <= HashLen {
		copy(s.h[:], protocolName)
	} else {
		copy(s.h[:], Hash([]byte(protocolName)))
	}
	s.ck = s.h
	return s
}

// H returns a copy of the handshake hash.
func (s *SymmetricState) H() []byte { return append([]byte(nil), s.h[:]...) }

// CK returns a copy of the chaining key.
func (s *SymmetricState) CK() []byte { return append([]byte(nil), s.ck[:]...) }

// Cipher exposes the inner CipherState (read-only use: key and nonce for traces).
func (s *SymmetricState) Cipher() *CipherState { return &s.cs }

// MixKey runs MixKey(ikm).
func (s *SymmetricState) MixKey(ikm []byte) {
	ck, k := HKDF2(s.ck[:], ikm)
	copy(s.ck[:], ck)
	s.cs.InitializeKey(k[:KeyLen])
	wipe(ck)
	wipe(k)
}

// MixHash runs MixHash(data).
func (s *SymmetricState) MixHash(data []byte) {
	copy(s.h[:], Hash(s.h[:], data))
}

// EncryptAndHash appends EncryptAndHash(pt) to out.
func (s *SymmetricState) EncryptAndHash(out, pt []byte) ([]byte, error) {
	start := len(out)
	out, err := s.cs.EncryptWithAd(out, s.h[:], pt)
	if err != nil {
		return nil, err
	}
	s.MixHash(out[start:])
	return out, nil
}

// DecryptAndHash returns DecryptAndHash(ct).
func (s *SymmetricState) DecryptAndHash(ct []byte) ([]byte, error) {
	pt, err := s.cs.DecryptWithAd(nil, s.h[:], ct)
	if err != nil {
		return nil, err
	}
	s.MixHash(ct)
	return pt, nil
}

// Split returns the two transport CipherStates (initiator→responder, responder→initiator).
func (s *SymmetricState) Split() (c1, c2 *CipherState) {
	k1, k2 := HKDF2(s.ck[:], nil)
	c1, c2 = &CipherState{}, &CipherState{}
	c1.InitializeKey(k1[:KeyLen])
	c2.InitializeKey(k2[:KeyLen])
	wipe(k1)
	wipe(k2)
	return c1, c2
}

// Erase clears ck, h and the cipher key.
func (s *SymmetricState) Erase() {
	wipe(s.ck[:])
	wipe(s.h[:])
	s.cs.Erase()
}
