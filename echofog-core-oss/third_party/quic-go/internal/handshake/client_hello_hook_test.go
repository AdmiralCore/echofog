package handshake

import (
	"bytes"
	"crypto/ecdh"
	"io"
	"testing"
)

// TestCH0RandZeroesRandom pins the pre-pass reader: the first 32 bytes it serves are zeros
// (CH0's random), everything after comes from the source and is recorded for the replay.
func TestCH0RandZeroesRandom(t *testing.T) {
	src := bytes.NewReader(bytes.Repeat([]byte{0x11}, 64))
	r := newCH0Rand(src)
	buf := make([]byte, 32)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, make([]byte, 32)) {
		t.Fatalf("first 32 bytes %x, want zeros", buf)
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, bytes.Repeat([]byte{0x11}, 32)) {
		t.Fatalf("second read %x, want the source bytes", buf)
	}
	if len(r.recorded) != 64 {
		t.Fatalf("recorded %d bytes, want 64", len(r.recorded))
	}
}

// TestTokenFirstRandReplays checks that the real pass sees the hook's bytes in front of the
// recorded stream, and switches to the fallback only after it is exhausted.
func TestTokenFirstRandReplays(t *testing.T) {
	recorded := append(make([]byte, 32), bytes.Repeat([]byte{0x22}, 16)...)
	var token [32]byte
	for i := range token {
		token[i] = byte(0x80 + i)
	}
	r := &tokenFirstRand{token: token, recorded: recorded, fallback: bytes.NewReader([]byte{0x33})}
	got := make([]byte, 32+16+1)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:32], token[:]) {
		t.Fatalf("token part %x, want %x", got[:32], token)
	}
	if !bytes.Equal(got[32:48], bytes.Repeat([]byte{0x22}, 16)) {
		t.Fatalf("replayed part %x, want the recorded bytes", got[32:48])
	}
	if got[48] != 0x33 {
		t.Fatalf("fallback byte %x, want 0x33", got[48])
	}
}

// TestRecoverX25519Key checks that the private key of the hello's X25519 share is found among
// the 32-byte reads of the randomness stream, which is what patch 4 hands to the hook.
func TestRecoverX25519Key(t *testing.T) {
	priv, err := ecdh.X25519().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	ch0 := helloWithX25519Share(t, priv.PublicKey().Bytes())
	reads := [][]byte{
		make([]byte, 32), // the zeroed random
		priv.Bytes(),
	}
	got, err := recoverX25519Key(ch0, reads)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv.Bytes()) {
		t.Fatalf("recovered key %x, want %x", got, priv.Bytes())
	}

	if _, err := recoverX25519Key(ch0, [][]byte{make([]byte, 32)}); err == nil {
		t.Fatal("recovered a key that is not in the stream")
	}
	if _, err := recoverX25519Key(helloWithX25519Share(t, make([]byte, 32))[:20], reads); err == nil {
		t.Fatal("recovered a key from a truncated hello")
	}
}

// TestHelloX25519ShareShort checks review finding 8: a 38-byte hello (header, version and
// random, no session id) must not panic the parser.
func TestHelloX25519ShareShort(t *testing.T) {
	msg := make([]byte, 38)
	msg[0] = 1
	msg[3] = 34 // body length: version + random, nothing after the random
	if C, ok := helloX25519Share(msg); ok || C != nil {
		t.Fatalf("a hello without a key_share returned %x", C)
	}
	if C, ok := helloX25519Share(msg[:37]); ok || C != nil {
		t.Fatalf("a truncated hello returned %x", C)
	}
}

// helloWithX25519Share builds a minimal ClientHello handshake message carrying C as its X25519
// key share.
func helloWithX25519Share(t testing.TB, C []byte) []byte {
	t.Helper()
	shares := []byte{0x00, 0x1d, 0x00, 0x20}
	shares = append(shares, C...)
	body := append([]byte{byte(len(shares) >> 8), byte(len(shares))}, shares...)
	ext := append([]byte{0x00, 0x33, byte(len(body) >> 8), byte(len(body))}, body...)
	hello := []byte{0x03, 0x03}
	hello = append(hello, make([]byte, 32)...)
	hello = append(hello, 0x00, 0x00, 0x02, 0x13, 0x01, 0x01, 0x00)
	hello = append(hello, byte(len(ext)>>8), byte(len(ext)))
	hello = append(hello, ext...)
	msg := []byte{1, byte(len(hello) >> 16), byte(len(hello) >> 8), byte(len(hello))}
	return append(msg, hello...)
}
