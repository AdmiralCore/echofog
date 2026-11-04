package identity

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"

	"github.com/echofog/echofog-core/internal/v2/secret"
)

// hardened is the SLIP-0010 hardened-child flag.
const hardened uint32 = 0x80000000

// slip10Master returns the master private key and chain code of SLIP-0010 Ed25519.
func slip10Master(seed []byte) (key, chain []byte) {
	h := hmac.New(sha512.New, []byte("ed25519 seed"))
	h.Write(seed)
	I := h.Sum(nil)
	key = append([]byte(nil), I[:32]...)
	chain = append([]byte(nil), I[32:]...)
	secret.Wipe(I)
	return key, chain
}

// slip10Path derives the hardened children of path from a key and chain code and wipes every
// intermediate node.
func slip10Path(key, chain []byte, path ...uint32) ([]byte, []byte) {
	for _, index := range path {
		nextKey, nextChain := slip10Child(key, chain, index)
		secret.Wipe(key)
		secret.Wipe(chain)
		key, chain = nextKey, nextChain
	}
	return key, chain
}

// slip10Child derives one hardened child (SLIP-0010, Ed25519 has only hardened derivation).
func slip10Child(key, chain []byte, index uint32) ([]byte, []byte) {
	data := make([]byte, 1+len(key)+4)
	copy(data[1:], key)
	binary.BigEndian.PutUint32(data[1+len(key):], index|hardened)

	h := hmac.New(sha512.New, chain)
	h.Write(data)
	I := h.Sum(nil)

	outKey := append([]byte(nil), I[:32]...)
	outChain := append([]byte(nil), I[32:]...)
	secret.Wipe(I)
	secret.Wipe(data)
	return outKey, outChain
}
