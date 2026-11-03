package objects

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// DHT keys of the record kinds (registries.md §9, dht.md §2).

// DHTDay returns floor(unix / 86400).
func DHTDay(t time.Time) uint32 { return uint32(unix(t) / day) }

// DHTKey returns SHA-256("echofog/dht2/key" ‖ u32(day) ‖ u8 len(label) ‖ label ‖ signer_id), the key
// of a signer-keyed record. signerID is SHA-256 of the envelope signer's akey.
func DHTKey(label string, signerID [32]byte, day uint32) [32]byte {
	b := make([]byte, 0, 16+4+1+len(label)+32)
	b = append(b, "echofog/dht2/key"...)
	b = binary.BigEndian.AppendUint32(b, day)
	b = append(b, byte(len(label)))
	b = append(b, label...)
	b = append(b, signerID[:]...)
	return sha256.Sum256(b)
}

// ReleaseDHTKey returns SHA-256("echofog/dht2/release" ‖ u32(day)).
func ReleaseDHTKey(day uint32) [32]byte {
	b := binary.BigEndian.AppendUint32([]byte("echofog/dht2/release"), day)
	return sha256.Sum256(b)
}
