package chatcrypto

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"golang.org/x/crypto/chacha20poly1305"
	"io"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// SealLabel is the HKDF info prefix of the sealing key (services.md 4.2).
const SealLabel = "echofog/chat2/seal"

// ErrOpen is every failure to open a sealed envelope; one error, one text (AD20).
var ErrOpen = errors.New("chat/2: envelope not opened")

// SealEnvelope seals inner for the recipient (services.md 4.2): a fresh ephemeral X25519 key, the
// key k = HKDF-SHA256(X25519(eph, ik), info = "echofog/chat2/seal" || eph || ik) and
// ChaCha20-Poly1305(k, nonce 0, aad = to || eph, inner). to is the recipient's node ID, ik the
// X25519 identity key of the recipient's prekeys/2. The result is the serialised SealedEnvelope.
//
// The key is used for one box, so the all-zero nonce is never repeated under it.
func SealEnvelope(to, ik [32]byte, inner []byte) ([]byte, error) {
	sk := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, sk); err != nil {
		return nil, err
	}
	return sealWith(sk, to, ik, inner)
}

// sealWith is SealEnvelope with a given ephemeral key (the vector).
func sealWith(ephSK []byte, to, ik [32]byte, inner []byte) ([]byte, error) {
	eph, err := curve25519.X25519(ephSK, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(ephSK, ik[:]) // errors on a low-order recipient key
	if err != nil {
		return nil, err
	}
	aead, err := sealAEAD(shared, eph, ik[:])
	if err != nil {
		return nil, err
	}
	box := aead.Seal(nil, make([]byte, chacha20poly1305.NonceSize), inner, append(append([]byte(nil), to[:]...), eph...))
	return objects.Marshal(&chatpb.SealedEnvelope{To: to[:], Eph: eph, Box: box})
}

func sealAEAD(shared, eph, ik []byte) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}, error) {
	info := append(append([]byte(SealLabel), eph...), ik...)
	k := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, info), k); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(k)
}

// OpenEnvelope opens an envelope addressed to the node ID self with the device's identity key.
// Any failure is ErrOpen.
func (c *Crypto) OpenEnvelope(self [32]byte, env []byte) ([]byte, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, errClosed
	}
	var inner []byte
	err := c.ikSecret.Use(func(sk []byte) error {
		var err error
		inner, err = openWith(sk, c.ikPub, self, env)
		return err
	})
	if err != nil {
		return nil, ErrOpen
	}
	return inner, nil
}

// openWith opens an envelope with an identity key.
func openWith(ikSK []byte, ikPub, self [32]byte, env []byte) ([]byte, error) {
	m, err := chatpb.ParseSealedEnvelope(env)
	if err != nil || !equal(m.GetTo(), self[:]) {
		return nil, ErrOpen
	}
	shared, err := curve25519.X25519(ikSK, m.GetEph())
	if err != nil {
		return nil, ErrOpen
	}
	aead, err := sealAEAD(shared, m.GetEph(), ikPub[:])
	if err != nil {
		return nil, ErrOpen
	}
	inner, err := aead.Open(nil, make([]byte, chacha20poly1305.NonceSize), m.GetBox(), append(append([]byte(nil), m.GetTo()...), m.GetEph()...))
	if err != nil {
		return nil, ErrOpen
	}
	return inner, nil
}
