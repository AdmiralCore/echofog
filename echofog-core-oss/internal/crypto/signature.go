package crypto

import (
	"crypto/ed25519"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func VerifySignature(pubKey types.PeerPublicKey, payload []byte, sig []byte) bool {
	if len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pubKey[:], payload, sig)
}
