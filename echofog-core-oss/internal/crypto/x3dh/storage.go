package x3dh

import (
	"encoding/hex"
	"fmt"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// X3DHSessionManager keeps ratchet sessions in the database, sealed with the account's storage key.
// It also remembers which one-time prekeys of peers we used and which init packets we accepted.
type X3DHSessionManager struct {
	db  storage.Storage
	key *memguard.Enclave
}

func NewX3DHSessionManager(db storage.Storage, key *memguard.Enclave) *X3DHSessionManager {
	return &X3DHSessionManager{db: db, key: key}
}

// The peer is blinded in the keys: the database does not show whom we have sessions with.
func (sm *X3DHSessionManager) sessionKey(peerID types.PeerID) []byte {
	return []byte("sess:" + storage.BlindID(sm.db, hex.EncodeToString(peerID[:])))
}

func (sm *X3DHSessionManager) Save(peerID types.PeerID, session *X3DHSession) error {
	plainBytes, err := session.ToBytes()
	if err != nil {
		return fmt.Errorf("serialize error: %w", err)
	}
	defer memguard.WipeBytes(plainBytes)

	name := sm.sessionKey(peerID)
	sealed, err := keymanager.SealValue(sm.key, name, plainBytes)
	if err != nil {
		return fmt.Errorf("seal session: %w", err)
	}
	return sm.db.Set(name, sealed)
}

// Load returns the session with peerID. A session that cannot be opened (stored by an older
// version, or damaged) counts as missing.
func (sm *X3DHSessionManager) Load(peerID types.PeerID) (*X3DHSession, error) {
	name := sm.sessionKey(peerID)
	sealed, err := sm.db.Get(name)
	if err != nil {
		return nil, err
	}
	plain, err := keymanager.OpenValue(sm.key, name, sealed)
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(plain)
	return FromBytes(plain)
}

func (sm *X3DHSessionManager) Delete(id types.PeerID) error {
	return sm.db.Delete(sm.sessionKey(id))
}

func (sm *X3DHSessionManager) usedOPKKey(peer types.PeerID, opk [32]byte) []byte {
	return []byte("opkused:" + storage.BlindID(sm.db, hex.EncodeToString(peer[:])+hex.EncodeToString(opk[:])))
}

// MarkOPKUsed remembers that we started a session with this one-time prekey of peer: the peer
// deleted it, so a bundle from cache must not offer it again.
func (sm *X3DHSessionManager) MarkOPKUsed(peer types.PeerID, opk [32]byte) {
	_ = sm.db.Set(sm.usedOPKKey(peer, opk), []byte{1})
}

// UnusedOPKs filters out the one-time prekeys of peer that we already used.
func (sm *X3DHSessionManager) UnusedOPKs(peer types.PeerID, opks [][32]byte) [][32]byte {
	out := make([][32]byte, 0, len(opks))
	for _, k := range opks {
		if !sm.db.Exists(sm.usedOPKKey(peer, k)) {
			out = append(out, k)
		}
	}
	return out
}

func initKey(ek [32]byte) []byte { return append([]byte("x3dh-init:"), ek[:]...) }

// InitSeen tells whether an init packet with this ephemeral key was accepted before.
func (sm *X3DHSessionManager) InitSeen(ek [32]byte) bool { return sm.db.Exists(initKey(ek)) }

// MarkInitSeen records an accepted init packet, so a replay of it cannot start a session again.
func (sm *X3DHSessionManager) MarkInitSeen(ek [32]byte) { _ = sm.db.Set(initKey(ek), []byte{1}) }
