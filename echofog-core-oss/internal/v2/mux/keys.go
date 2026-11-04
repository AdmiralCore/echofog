package mux

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// Keys is what the mux needs from a completed handshake (handshake.md §9). The handshake
// package produces it; the mux never runs the handshake itself.
type Keys interface {
	// SID is the 16-byte session ID.
	SID() [16]byte
	// Initiator reports whether this side was the session (handshake) initiator.
	Initiator() bool
	// SessionSecret keys path binding (wire.md §8.2).
	SessionSecret() []byte
	// TranscriptHash is the final handshake hash h.
	TranscriptHash() []byte
	// TransportKeys are Split() of the handshake: k_i2r and k_r2i (32 bytes each), used for
	// sealed records and datagrams (wire.md §8.4).
	TransportKeys() (i2r, r2i []byte)
}

// Labels (registries.md §7.4).
const (
	labelSessionID = "echofog/session2/id"
	labelPathBind  = "echofog/session2/path-bind"
	labelPathMAC   = "echofog/session2/path-mac"
)

// DeriveSID computes sid = HKDF-SHA256(ikm = session_secret, salt = h, info = "echofog/session2/id", 16).
// The handshake derives it; the mux uses it only to check consistency in tests.
func DeriveSID(secret, h []byte) [16]byte {
	k, err := hkdf.Key(sha256.New, secret, h, labelSessionID, 16)
	if err != nil {
		panic(err)
	}
	return [16]byte(k)
}

// PathBindKey computes k_path = HKDF-SHA256(ikm = session_secret, salt = h,
// info = "echofog/session2/path-bind", L = 32) (wire.md §8.2).
func PathBindKey(k Keys) []byte {
	out, err := hkdf.Key(sha256.New, k.SessionSecret(), k.TranscriptHash(), labelPathBind, 32)
	if err != nil {
		panic(err)
	}
	return out
}

// PathMAC computes HMAC-SHA256(k_path, "echofog/session2/path-mac" ‖ cb(32) ‖ sid(16) ‖ u64(path_id) ‖ u8(role)),
// role = 0 if the sender is the session initiator, 1 otherwise.
func PathMAC(kPath []byte, cb [32]byte, sid [16]byte, pathID uint64, role byte) [32]byte {
	m := hmac.New(sha256.New, kPath)
	m.Write([]byte(labelPathMAC))
	m.Write(cb[:])
	m.Write(sid[:])
	m.Write(binary.BigEndian.AppendUint64(nil, pathID))
	m.Write([]byte{role})
	return [32]byte(m.Sum(nil))
}

func roleOf(initiator bool) byte {
	if initiator {
		return 0
	}
	return 1
}
