package reality2

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"
)

// tcpHello builds a ClientHello of reality2-tcp (wire.md §4.1): the 32-byte token is the
// legacy_session_id, the random stays random. withToken false builds CH0 (session id zeroed).
func tcpHello(t testing.TB, random, sid, shares []byte) []byte {
	t.Helper()
	exts := append([]byte{0x00, 0x33, byte(len(shares) >> 8), byte(len(shares))}, shares...)
	body := []byte{0x03, 0x03}
	body = append(body, random...)
	body = append(body, byte(len(sid)))
	body = append(body, sid...)
	body = append(body, 0x00, 0x02, 0x13, 0x01, 0x01, 0x00)
	body = append(body, byte(len(exts)>>8), byte(len(exts)))
	body = append(body, exts...)
	msg := []byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(msg, body...)
}

func TestParseHelloTCPTokenInSessionID(t *testing.T) {
	random := make([]byte, 32)
	_, _ = rand.Read(random)
	sid := bytes.Repeat([]byte{0xAB}, 32)
	c, _ := ecdh.X25519().GenerateKey(rand.Reader)
	shares := keyShareX25519(c.PublicKey().Bytes())

	h, err := ParseHelloTCP(tcpHello(t, random, sid, shares))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.Token, sid) {
		t.Fatalf("token %x, want the session id %x", h.Token, sid)
	}
	if !bytes.Equal(h.C, c.PublicKey().Bytes()) {
		t.Fatal("C is not the X25519 share")
	}
	// CH0 zeroes the session id and keeps the random.
	want := tcpHello(t, random, make([]byte, 32), shares)
	if !bytes.Equal(h.CH0, want) {
		t.Fatalf("CH0 differs from the hello with a zero session id")
	}
	// The QUIC parser still reads the random, not the session id.
	q, err := ParseHello(tcpHello(t, random, sid, shares))
	if err != nil || !bytes.Equal(q.Token, random) {
		t.Fatalf("ParseHello changed: %v", err)
	}
}

func TestParseHelloTCPNeedsA32ByteSessionID(t *testing.T) {
	random := make([]byte, 32)
	c, _ := ecdh.X25519().GenerateKey(rand.Reader)
	shares := keyShareX25519(c.PublicKey().Bytes())
	for _, n := range []int{0, 1, 31, 33} {
		if _, err := ParseHelloTCP(tcpHello(t, random, make([]byte, n), shares)); err == nil {
			t.Fatalf("session id of %d bytes accepted", n)
		}
	}
}

func TestVerifierTCPToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mask := mustMask(t)
	replay := NewReplay(ReplayConfig{Now: func() time.Time { return now }})
	v, err := NewVerifier([]MaskKey{{Private: mask}}, []string{"a.test:443"}, replay, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	c, _ := ecdh.X25519().GenerateKey(rand.Reader)
	C := c.PublicKey().Bytes()
	random := make([]byte, 32)
	_, _ = rand.Read(random)
	shares := keyShareX25519(C)
	ch0 := tcpHello(t, random, make([]byte, 32), shares)
	z, _ := c.ECDH(mask.PublicKey())
	tok, err := SealToken(TokenKey(z, mask.PublicKey().Bytes(), C, "a.test:443"), HelloAAD(ch0), uint32(now.Unix()))
	if err != nil {
		t.Fatal(err)
	}
	hello, err := ParseHelloTCP(tcpHello(t, random, tok, shares))
	if err != nil {
		t.Fatal(err)
	}
	if !v.Verify(hello) {
		t.Fatal("valid token rejected")
	}
	if v.Verify(hello) {
		t.Fatal("replayed token accepted")
	}

	// A token for another address of the node does not open.
	v2, _ := NewVerifier([]MaskKey{{Private: mask}}, []string{"b.test:443"}, NewReplay(ReplayConfig{Now: func() time.Time { return now }}), func() time.Time { return now })
	if v2.Verify(hello) {
		t.Fatal("token valid for a.test accepted for b.test")
	}
	// The hello with another random is a different ClientHello: the binding breaks.
	other := make([]byte, 32)
	moved, _ := ParseHelloTCP(tcpHello(t, other, tok, shares))
	v3, _ := NewVerifier([]MaskKey{{Private: mask}}, []string{"a.test:443"}, NewReplay(ReplayConfig{Now: func() time.Time { return now }}), func() time.Time { return now })
	if v3.Verify(moved) {
		t.Fatal("token moved into another ClientHello accepted")
	}
}

func TestNewVerifierRejectsBadInput(t *testing.T) {
	replay := NewReplay(ReplayConfig{})
	if _, err := NewVerifier(nil, []string{"a:1"}, replay, nil); err == nil {
		t.Fatal("no masks accepted")
	}
	if _, err := NewVerifier([]MaskKey{{Private: mustMask(t)}}, nil, replay, nil); err == nil {
		t.Fatal("no addresses accepted")
	}
	if _, err := NewVerifier([]MaskKey{{Private: mustMask(t)}}, []string{"a:1"}, nil, nil); err == nil {
		t.Fatal("no replay cache accepted")
	}
}

func FuzzParseHelloTCP(f *testing.F) {
	c, _ := ecdh.X25519().GenerateKey(rand.Reader)
	shares := keyShareX25519(c.PublicKey().Bytes())
	f.Add(tcpHello(f, make([]byte, 32), make([]byte, 32), shares))
	f.Add(tcpHello(f, make([]byte, 32), nil, shares))
	f.Add([]byte{1, 0, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, msg []byte) {
		h, err := ParseHelloTCP(msg)
		if err != nil {
			return
		}
		if len(h.Token) != 32 || len(h.C) != 32 || len(h.CH0) > len(msg) {
			t.Fatalf("malformed result: token %d C %d CH0 %d of %d", len(h.Token), len(h.C), len(h.CH0), len(msg))
		}
	})
}
