package call

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// NewInviteSecret creates a call invite with 256 bits of entropy. The invite is the room secret:
// it travels only inside end-to-end encrypted messages and never appears on the call wire.
func NewInviteSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// DeriveRoom splits an invite into the public room ID used in signaling and the media key.
// Knowing the room ID (e.g. from a relay or an event log) does not reveal the key.
func DeriveRoom(invite string) (roomID string, key []byte) {
	id := sha256.Sum256([]byte("echofog-call-room-id\x00" + invite))
	k := sha256.Sum256([]byte("echofog-call-room-key\x00" + invite))
	return "room_" + hex.EncodeToString(id[:12]), k[:]
}
