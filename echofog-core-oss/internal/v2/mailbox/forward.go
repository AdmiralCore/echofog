package mailbox

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// ErrAuth reports a FETCH or ACK whose auth signature does not verify for this session, queue and
// request. The name of the queue's auth key is never echoed.
var ErrAuth = errors.New("mailbox: bad fetch auth")

// ErrPowInvalid reports a proof of work that does not meet the queue's threshold.
var ErrPowInvalid = errors.New("mailbox: invalid proof of work")

// AuthInput builds the signed bytes of FETCH and ACK (services.md Â§5.2):
//
//	"echofog/mailbox2/fetch-auth" â€– sid(16) â€– u8(op) â€– queue(16) â€– u64(req)
//
// Every part has a fixed length, so the concatenation is unambiguous.
func AuthInput(op byte, sid [16]byte, queue []byte, req uint64) []byte {
	out := make([]byte, 0, len(AuthLabel)+16+1+len(queue)+8)
	out = append(out, AuthLabel...)
	out = append(out, sid[:]...)
	out = append(out, op)
	out = append(out, queue...)
	return binary.BigEndian.AppendUint64(out, req)
}

// VerifyAuth checks auth over the input above with the queue's Ed25519 auth key. authKey may be a
// raw 32-byte Ed25519 public key or its 33-byte akey form (the spec names an "Ed25519 auth_key"
// without fixing the encoding; both appear in v2 objects).
func VerifyAuth(authKey, auth []byte, op byte, sid [16]byte, queue []byte, req uint64) error {
	pub, err := normalizeAuthKey(authKey)
	if err != nil {
		return ErrAuth
	}
	if len(auth) != ed25519.SignatureSize {
		return ErrAuth
	}
	if err := keys.Verify(pub, AuthInput(op, sid, queue, req), auth); err != nil {
		return ErrAuth
	}
	return nil
}

func normalizeAuthKey(b []byte) (keys.AKey, error) {
	var pub keys.AKey
	switch len(b) {
	case ed25519.PublicKeySize:
		pub = keys.New(keys.Ed25519, b)
	case ed25519.PublicKeySize + 1:
		var err error
		pub, err = keys.ParseKind(b, keys.Ed25519)
		if err != nil {
			return nil, ErrAuth
		}
	default:
		return nil, ErrAuth
	}
	if err := keys.CheckEd25519Public(pub.Key()); err != nil {
		return nil, ErrAuth
	}
	return pub, nil
}

// PowHash is SHA-256("echofog/mailbox2/pow" â€– queue(16) â€– SHA-256(blob) â€– u64(pow)) as required
// for a deposit to a public queue (services.md Â§5.2).
func PowHash(queue, blob []byte, pow uint64) [32]byte {
	blobHash := sha256.Sum256(blob)
	h := sha256.New()
	h.Write([]byte(PowLabel))
	h.Write(queue)
	h.Write(blobHash[:])
	h.Write(binary.BigEndian.AppendUint64(nil, pow))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// LeadingZeroBits counts the zero bits from the most significant bit of h.
func LeadingZeroBits(h []byte) int {
	n := 0
	for _, b := range h {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<uint(bit)) != 0 {
				return n
			}
			n++
		}
	}
	return n
}

// ValidPow reports whether pow gives at least bits leading zero bits. A threshold above MaxPowBits
// is never satisfiable by a queue this build accepts.
func ValidPow(queue, blob []byte, pow uint64, bits uint32) bool {
	if bits > MaxPowBits {
		return false
	}
	h := PowHash(queue, blob, pow)
	return LeadingZeroBits(h[:]) >= int(bits)
}

// ---- forwarded deposit sealing (services.md Â§5.3) -------------------------------------------

// forwardKey derives the sealing key from the ephemeral private key and M's handshake public key:
//
//	k = HKDF-SHA256(ikm = X25519(eph_sk, hs_M), salt = empty,
//	                info = "echofog/mailbox2/forward" â€– eph(32) â€– hs_M(32), L = 32)
func forwardKey(ephPrivate, hsPublic []byte) ([]byte, error) {
	priv, err := ecdh.X25519().NewPrivateKey(ephPrivate)
	if err != nil {
		return nil, fmt.Errorf("mailbox: forward ephemeral key: %w", err)
	}
	pub, err := ecdh.X25519().NewPublicKey(hsPublic)
	if err != nil {
		return nil, fmt.Errorf("mailbox: mailbox handshake key: %w", err)
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("mailbox: forward ECDH: %w", err)
	}
	return deriveForwardKey(shared, priv.PublicKey().Bytes(), hsPublic)
}

// deriveForwardKey is the KDF of services.md §5.3 for an already computed shared secret. The
// ephemeral public key in the info string is the sender's, at both ends.
func deriveForwardKey(shared, ephPublic, hsPublic []byte) ([]byte, error) {
	info := make([]byte, 0, len(ForwardLabel)+64)
	info = append(info, ForwardLabel...)
	info = append(info, ephPublic...)
	info = append(info, hsPublic...)
	key, err := hkdf.Key(sha256.New, shared, nil, string(info), chacha20poly1305.KeySize)
	if err != nil {
		return nil, fmt.Errorf("mailbox: forward KDF: %w", err)
	}
	return key, nil
}

func forwardEphPublic(ephPrivate []byte) ([]byte, error) {
	priv, err := ecdh.X25519().NewPrivateKey(ephPrivate)
	if err != nil {
		return nil, fmt.Errorf("mailbox: forward ephemeral key: %w", err)
	}
	return priv.PublicKey().Bytes(), nil
}

// SealForward builds the sealed DEPOSIT body of FORWARD for M's handshake public key: the 32-byte
// ephemeral public key followed by ChaCha20-Poly1305(k, nonce 0, aad = eph) over deposit.
func SealForward(ephPrivate, hsPublic, deposit []byte) ([]byte, error) {
	key, err := forwardKey(ephPrivate, hsPublic)
	if err != nil {
		return nil, err
	}
	eph, err := forwardEphPublic(ephPrivate)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("mailbox: forward AEAD: %w", err)
	}
	out := make([]byte, 0, len(eph)+len(deposit)+aead.Overhead())
	out = append(out, eph...)
	return aead.Seal(out, make([]byte, chacha20poly1305.NonceSize), deposit, eph), nil
}

// OpenForward opens a FORWARD sealed with SealForward to this node's handshake key.
func OpenForward(hs *identity.KeyPair, sealed []byte) ([]byte, error) {
	if hs == nil || len(sealed) < 32+chacha20poly1305.Overhead {
		return nil, ErrBadFrame
	}
	eph := sealed[:32]
	aeadKey, err := func() ([]byte, error) {
		var priv []byte
		if err := hs.WithPrivate(func(b []byte) error {
			priv = append([]byte(nil), b...)
			return nil
		}); err != nil {
			return nil, err
		}
		defer zero(priv)
		hsPub := hs.Public().Key()
		hsPrivate, err := ecdh.X25519().NewPrivateKey(priv)
		if err != nil {
			return nil, err
		}
		ephPublic, err := ecdh.X25519().NewPublicKey(eph)
		if err != nil {
			return nil, ErrBadFrame
		}
		shared, err := hsPrivate.ECDH(ephPublic)
		if err != nil {
			return nil, ErrBadFrame
		}
		return deriveForwardKey(shared, eph, hsPub)
	}()
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(aeadKey)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, make([]byte, chacha20poly1305.NonceSize), sealed[32:], eph)
	if err != nil {
		return nil, ErrBadFrame
	}
	return plain, nil
}

// zero wipes a private key copy. secret.Wipe would do the same; the dependency is not worth it for
// one local slice.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
