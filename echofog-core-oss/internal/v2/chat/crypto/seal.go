package chatcrypto

import "errors"

// ErrOpen is every failure to open a sealed envelope; one error, one text (AD20).
var ErrOpen = errors.New("chat/2: envelope not opened")

// SealEnvelope seals inner for the recipient (services.md 4.2): a fresh ephemeral X25519 key, the
// key k = HKDF-SHA256(X25519(eph, ik), info = "echofog/chat2/seal" || eph || ik) and
// ChaCha20-Poly1305(k, nonce 0, aad = to || eph, inner). to is the recipient's node ID, ik the
// X25519 identity key of the recipient's prekeys/2. The result is the serialised SealedEnvelope.
func SealEnvelope(to, ik [32]byte, inner []byte) ([]byte, error) {
	return nil, errors.New("not implemented")
}

// OpenEnvelope opens an envelope addressed to the node ID self with the device's identity key.
func (c *Crypto) OpenEnvelope(self [32]byte, env []byte) ([]byte, error) {
	return nil, errors.New("not implemented")
}

// sealWith is SealEnvelope with a given ephemeral key (the vector).
func sealWith(ephSK []byte, to, ik [32]byte, inner []byte) ([]byte, error) {
	return nil, errors.New("not implemented")
}

// openWith opens an envelope with an identity key.
func openWith(ikSK []byte, ikPub, self [32]byte, env []byte) ([]byte, error) {
	return nil, errors.New("not implemented")
}
