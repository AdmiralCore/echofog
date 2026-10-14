package groups

import (
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/nacl/box"
)

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
	if _, err := io.ReadFull(rand.Reader, rawKey); err != nil {
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
	if _, err := io.ReadFull(rand.Reader, rawKey); err != nil {
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

	var recipCurvePub [32]byte
	copy(recipCurvePub[:], recipientPub[:]) // Map identity public key to Curve25519

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
