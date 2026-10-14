package dpi

import (
	"bytes"
	"testing"
)

// FuzzOpenForeignPacket: every datagram on the node's UDP port goes through open() before QUIC
// sees it. Noise and scanner packets must be dropped, not panic, not decode to something.
func FuzzOpenForeignPacket(f *testing.F) {
	var key [KeyLen]byte
	copy(key[:], NodeKey("", [32]byte{9}))
	salt := bytes.Repeat([]byte{1}, SaltLen)
	wire := make([]byte, 128)
	n, _ := seal(&key, []byte("payload"), wire, salt)
	f.Add(wire[:n])
	f.Add(wire[:Overhead])
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xC0}, 1200)) // a QUIC Initial in the clear
	f.Fuzz(func(t *testing.T, packet []byte) {
		out := make([]byte, len(packet)+1)
		m, ok := open(&key, packet, out)
		if ok && m != len(packet)-Overhead {
			t.Fatalf("length %d for a packet of %d", m, len(packet))
		}
	})
}

// FuzzSealOpen: everything sealed with a key opens with it, to the same bytes, and only with it.
func FuzzSealOpen(f *testing.F) {
	f.Add([]byte("hello"), []byte{1, 2, 3, 4, 5, 6, 7, 8}, byte(1), byte(2))
	f.Add([]byte{}, make([]byte, SaltLen), byte(0), byte(0))
	f.Fuzz(func(t *testing.T, payload, salt []byte, id1, id2 byte) {
		if len(salt) < SaltLen || len(payload) > 2000 {
			t.Skip()
		}
		var k1, k2 [KeyLen]byte
		copy(k1[:], NodeKey("", [32]byte{id1}))
		copy(k2[:], NodeKey("", [32]byte{id2}))
		wire := make([]byte, len(payload)+Overhead)
		n, err := seal(&k1, payload, wire, salt[:SaltLen])
		if err != nil || n != len(wire) {
			t.Fatalf("seal: %v n=%d", err, n)
		}
		out := make([]byte, len(payload)+1)
		m, ok := open(&k1, wire, out)
		if len(payload) == 0 {
			return // an empty payload is not a valid packet
		}
		if !ok || !bytes.Equal(out[:m], payload) {
			t.Fatalf("did not open with its own key")
		}
		if id1 != id2 {
			if _, ok := open(&k2, wire, out); ok {
				t.Fatalf("opened with another node's key")
			}
		}
	})
}

func FuzzIsSTUN(f *testing.F) {
	f.Add(make([]byte, 20))
	f.Add([]byte{0, 1, 0, 0, 0x21, 0x12, 0xa4, 0x42})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if isSTUN(data) && len(data) < 20 {
			t.Fatalf("a message of %d bytes is not STUN", len(data))
		}
	})
}
