package groups

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"io"
	"sync"
	"time"

	"filippo.io/edwards25519"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/nacl/box"
)

// keyRand is where epoch keys come from; tests swap it to make a rotation fail.
var (
	defaultKeyRand io.Reader = rand.Reader
	keyRand                  = defaultKeyRand
)

// This in-memory engine predates group encryption and is not what protects messages: every node
// made its own random "epoch" on a ban, so no two members ever held the same key. Group messages
// are sealed with the epochs and sender chains of package senderkeys (pkg/api/node/group_keys.go),
// which admins make, sign and hand to the members. What is left here is the X25519 sealing of a
// key to a member's identity, kept for tests and for sealing outside a session.

// GroupEpochKey represents the symmetric cipher key used to encrypt group messages for a specific epoch.
type GroupEpochKey struct {
	GroupID   string `json:"group_id"`
	Epoch     uint64 `json:"epoch"`
	Key       []byte `json:"key"` // 32 bytes AES-256 / ChaCha20
	CreatedAt int64  `json:"created_at"`
}

// KeyRotationEngine handles Cryptographic Eviction upon member banning or key expiration.
type KeyRotationEngine struct {
	mu        sync.RWMutex
	epochKeys map[string]*GroupEpochKey // GroupID -> current key
}

// NewKeyRotationEngine creates a new key rotation manager.
func NewKeyRotationEngine() *KeyRotationEngine {
	return &KeyRotationEngine{
		epochKeys: make(map[string]*GroupEpochKey),
	}
}

// GenerateInitialKey creates Epoch 1 encryption key for a new group.
func (e *KeyRotationEngine) GenerateInitialKey(groupID string) (*GroupEpochKey, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rawKey := make([]byte, 32)
	if _, err := io.ReadFull(keyRand, rawKey); err != nil {
		return nil, err
	}

	epochKey := &GroupEpochKey{
		GroupID:   groupID,
		Epoch:     1,
		Key:       rawKey,
		CreatedAt: time.Now().UnixMilli(),
	}

	e.epochKeys[groupID] = epochKey
	return epochKey, nil
}

// RotateKey increments epoch and generates a fresh key upon ban or kick (Cryptographic Eviction).
func (e *KeyRotationEngine) RotateKey(groupID string) (*GroupEpochKey, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	current, exists := e.epochKeys[groupID]
	var nextEpoch uint64 = 1
	if exists {
		nextEpoch = current.Epoch + 1
	}

	rawKey := make([]byte, 32)
	if _, err := io.ReadFull(keyRand, rawKey); err != nil {
		return nil, err
	}

	newEpochKey := &GroupEpochKey{
		GroupID:   groupID,
		Epoch:     nextEpoch,
		Key:       rawKey,
		CreatedAt: time.Now().UnixMilli(),
	}

	e.epochKeys[groupID] = newEpochKey
	return newEpochKey, nil
}

// GetCurrentKey returns the active epoch key for a group.
func (e *KeyRotationEngine) GetCurrentKey(groupID string) (*GroupEpochKey, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	k, exists := e.epochKeys[groupID]
	return k, exists
}

// FilterAuthorizedRecipients strips banned members from the key distribution list.
// Banned peers receive ZERO keys and cannot decrypt any subsequent messages.
func FilterAuthorizedRecipients(members []MemberProfile) []MemberProfile {
	var authorized []MemberProfile
	for _, m := range members {
		if !m.IsBanned {
			authorized = append(authorized, m)
		}
	}
	return authorized
}

// EncryptKeyForMember seals the epoch key using an ephemeral NaCl box for the member's public key.
func EncryptKeyForMember(rawKey []byte, recipientPub types.IdentityPublicKey) ([]byte, error) {
	if len(rawKey) != 32 {
		return nil, errors.New("rawKey must be 32 bytes")
	}

	// Generate ephemeral Curve25519 keypair
	ephemPub, ephemPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	recipCurvePub, err := curvePublicKey(recipientPub)
	if err != nil {
		return nil, err
	}

	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, err
	}

	// box.Seal appends encrypted bytes to out
	encrypted := box.Seal(nil, rawKey, &nonce, &recipCurvePub, ephemPriv)

	// Format: [24 bytes Nonce] [32 bytes EphemPub] [EncryptedBytes]
	payload := make([]byte, 24+32+len(encrypted))
	copy(payload[0:24], nonce[:])
	copy(payload[24:56], ephemPub[:])
	copy(payload[56:], encrypted)

	return payload, nil
}

// DecryptKeyForMember opens a payload made by EncryptKeyForMember with the member's identity key.
func DecryptKeyForMember(payload []byte, recipientPriv ed25519.PrivateKey) ([]byte, error) {
	if len(payload) < 24+32+box.Overhead {
		return nil, errors.New("sealed key too short")
	}
	if len(recipientPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key size")
	}
	var nonce [24]byte
	var ephemPub [32]byte
	copy(nonce[:], payload[0:24])
	copy(ephemPub[:], payload[24:56])
	curvePriv := curvePrivateKey(recipientPriv)
	rawKey, ok := box.Open(nil, payload[56:], &nonce, &ephemPub, &curvePriv)
	if !ok {
		return nil, errors.New("sealed key cannot be opened with this identity")
	}
	return rawKey, nil
}

// curvePublicKey turns an Ed25519 identity key into the X25519 key of the same pair (the
// birational map from the Edwards to the Montgomery curve).
func curvePublicKey(pub types.IdentityPublicKey) ([32]byte, error) {
	var out [32]byte
	p, err := new(edwards25519.Point).SetBytes(pub[:])
	if err != nil {
		return out, errors.New("identity key is not a valid Ed25519 point")
	}
	copy(out[:], p.BytesMontgomery())
	return out, nil
}

// curvePrivateKey derives the X25519 scalar of an Ed25519 key the way Ed25519 derives its own:
// the clamped first half of SHA-512 of the seed.
func curvePrivateKey(priv ed25519.PrivateKey) [32]byte {
	h := sha512.Sum512(priv.Seed())
	var out [32]byte
	copy(out[:], h[:32])
	out[0] &= 248
	out[31] &= 127
	out[31] |= 64
	return out
}
