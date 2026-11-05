package reality2

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The vectors are copied from docs/protocol/v2/vectors into internal/v2/testdata so the module
// tests stand alone (the OSS copy ships without docs). TestVectorCopiesMatchSpec keeps them in
// sync; the vector itself is the source of truth for the token derivation of wire.md §3.2.
const testdata = "../testdata"

func loadJSON(t testing.TB, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestVectorCopiesMatchSpec(t *testing.T) {
	spec := filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors")
	if _, err := os.Stat(spec); err != nil {
		t.Skip("spec vectors not present (OSS copy)")
	}
	a, err := os.ReadFile(filepath.Join(spec, "reality2.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(testdata, "reality2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(a, []byte("\r\n"), []byte("\n")),
		bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))) {
		t.Error("reality2.json: testdata copy differs from docs/protocol/v2/vectors")
	}
}

type reality2Vector struct {
	MaskSK              string `json:"mask_sk"`
	MaskPub             string `json:"mask_pub"`
	ClientSKC           string `json:"client_sk_c"`
	ClientShareC        string `json:"client_share_C"`
	DialedAddress       string `json:"dialed_address"`
	Z                   string `json:"z"`
	HKDFInfo            string `json:"hkdf_info"`
	KTok                string `json:"k_tok"`
	KTokOther           string `json:"k_tok_at_198.51.100.7:443"`
	CH0                 string `json:"ch0"`
	AAD                 string `json:"aad"`
	TS                  uint32 `json:"ts"`
	Plaintext           string `json:"plaintext"`
	Token               string `json:"token"`
	ClientHelloWithTok  string `json:"client_hello_with_token"`
	HybridShareExample  struct {
		Note        string `json:"note"`
		ShareSHA256 string `json:"share_sha256"`
		C           string `json:"C"`
	} `json:"hybrid_share_example"`
}

func loadReality2Vector(t testing.TB) reality2Vector {
	t.Helper()
	var v reality2Vector
	loadJSON(t, "reality2.json", &v)
	return v
}

func maskKey(t testing.TB, sk []byte) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(sk)
	if err != nil {
		t.Fatalf("mask key: %v", err)
	}
	return k
}

// TestTokenVectors pins the whole derivation of wire.md §3.2 to docs/protocol/v2/vectors/
// reality2.json: z, k_tok, the ClientHello binding, the 32-byte token and the hello carrying it.
func TestTokenVectors(t *testing.T) {
	v := loadReality2Vector(t)
	sk := maskKey(t, unhex(t, v.MaskSK))
	maskPub := unhex(t, v.MaskPub)
	C := unhex(t, v.ClientShareC)
	ch0 := unhex(t, v.CH0)

	if got := sk.PublicKey().Bytes(); !bytes.Equal(got, maskPub) {
		t.Fatalf("mask public key %x, want %x", got, maskPub)
	}

	// The server derives z from the client's share; the client from its own private key.
	client, err := ecdh.X25519().NewPrivateKey(unhex(t, v.ClientSKC))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(client.PublicKey().Bytes(), C) {
		t.Fatalf("client share %x, want %x", client.PublicKey().Bytes(), C)
	}
	z, err := sk.ECDH(client.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(z, unhex(t, v.Z)) {
		t.Fatalf("z = %x, want %x", z, unhex(t, v.Z))
	}

	if got := TokenKey(z, maskPub, C, v.DialedAddress); !bytes.Equal(got, unhex(t, v.KTok)) {
		t.Fatalf("k_tok = %x, want %x", got, unhex(t, v.KTok))
	}
	otherZ, err := sk.ECDH(client.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if got := TokenKey(otherZ, maskPub, C, "198.51.100.7:443"); !bytes.Equal(got, unhex(t, v.KTokOther)) {
		t.Fatalf("k_tok at another address = %x, want %x", got, unhex(t, v.KTokOther))
	}

	aad := HelloAAD(ch0)
	if !bytes.Equal(aad[:], unhex(t, v.AAD)) {
		t.Fatalf("aad = %x, want %x", aad, unhex(t, v.AAD))
	}

	token, err := SealToken(unhex(t, v.KTok), aad, v.TS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(token, unhex(t, v.Token)) {
		t.Fatalf("token = %x, want %x", token, unhex(t, v.Token))
	}

	// The hello on the wire carries the token in ClientHello.random.
	withToken := unhex(t, v.ClientHelloWithTok)
	hello, err := ParseHello(withToken)
	if err != nil {
		t.Fatalf("parse hello: %v", err)
	}
	if !bytes.Equal(hello.Token, token) {
		t.Fatalf("hello token = %x, want %x", hello.Token, token)
	}
	if !bytes.Equal(hello.CH0, ch0) {
		t.Fatalf("hello CH0 differs from the vector")
	}
	if !bytes.Equal(hello.C, C) {
		t.Fatalf("hello C = %x, want %x", hello.C, C)
	}

	// And the server accepts it exactly within the timestamp window.
	now := time.Unix(int64(v.TS), 0)
	ts, err := OpenToken(unhex(t, v.KTok), aad, hello.Token, now)
	if err != nil {
		t.Fatalf("open token: %v", err)
	}
	if ts != v.TS {
		t.Fatalf("token timestamp %d, want %d", ts, v.TS)
	}

	// The same token at another address of the same node does not open (wire.md §3.2).
	otherKey := TokenKey(z, maskPub, C, "198.51.100.7:443")
	if _, err := OpenToken(otherKey, aad, hello.Token, now); err == nil {
		t.Fatal("token opened under another address")
	}
}

// TestHybridShareC checks that when a hello has no X25519 entry, C is the last 32 bytes of the
// X25519MLKEM768 share (wire.md §3.1).
func TestHybridShareC(t *testing.T) {
	v := loadReality2Vector(t)
	C := unhex(t, v.HybridShareExample.C)
	mlkem := bytes.Repeat([]byte{0x11}, 1184)
	body := []byte{0x00, 0x00} // client_shares length, filled below
	body = append(body, 0x11, 0xec, 0x04, 0xcc) // X25519MLKEM768 (0x11ec), 1232 bytes
	body = append(body, mlkem...)
	body = append(body, C...)
	body[0] = byte((len(body) - 2) >> 8)
	body[1] = byte(len(body) - 2)

	msg := syntheticHello(t, body)
	hello, err := ParseHello(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hello.C, C) {
		t.Fatalf("hybrid C = %x, want %x", hello.C, C)
	}
}

// TestTokenRejects checks the token format rules of wire.md §3.3 step 2 alone, without a server.
func TestTokenRejects(t *testing.T) {
	v := loadReality2Vector(t)
	kTok := unhex(t, v.KTok)
	ch0 := unhex(t, v.CH0)
	aad := HelloAAD(ch0)
	now := time.Unix(int64(v.TS), 0)

	good, err := SealToken(kTok, aad, v.TS)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token []byte
		aad   [32]byte
		now   time.Time
	}{
		{"short", good[:31], aad, now},
		{"long", append(append([]byte{}, good...), 0), aad, now},
		{"zero", make([]byte, TokenLen), aad, now},
		{"garbage", bytes.Repeat([]byte{0xaa}, TokenLen), aad, now},
		{"wrong aad", good, HelloAAD(syntheticHello(t, nil)), now},
		{"old ts", good, aad, now.Add(301 * time.Second)},
		{"future ts", good, aad, now.Add(-301 * time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OpenToken(kTok, tc.aad, tc.token, tc.now); err == nil {
				t.Fatal("token accepted")
			}
		})
	}
}

// TestParseHelloRejects covers the malformed ClientHellos the gate will see from strangers. None
// of them may panic, and none may produce a share.
func TestParseHelloRejects(t *testing.T) {
	base := syntheticHello(t, nil)
	cases := map[string][]byte{
		"empty":           nil,
		"short":           base[:10],
		"not a hello":     append([]byte{2}, base[1:]...),
		"truncated body":  base[:len(base)-1],
		"bad handshake":   append([]byte{1, 0xff, 0xff, 0xff}, base[4:]...),
		"no extensions":   append(append([]byte{}, base[:len(base)-2]...), 0, 0),
		"zero length":     {1, 0, 0, 0},
		"one byte":        {1},
		"random garbage":  bytes.Repeat([]byte{0x5a}, 64),
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHello(msg); err == nil {
				t.Fatal("malformed hello parsed")
			}
		})
	}
}

// syntheticHello builds a ClientHello handshake message whose extensions are key_share with the
// given body, so the token parser can be exercised without a real TLS stack.
func syntheticHello(t testing.TB, keyShareBody []byte) []byte {
	t.Helper()
	var exts []byte
	if keyShareBody != nil {
		ks := append([]byte{0x00, 0x33}, byte(len(keyShareBody)>>8), byte(len(keyShareBody)))
		ks = append(ks, keyShareBody...)
		exts = append(exts, ks...)
	}
	body := []byte{0x03, 0x03}
	body = append(body, make([]byte, 32)...) // random
	body = append(body,
		0x00,       // legacy_session_id length
		0x00, 0x02, // cipher suites length
		0x13, 0x01,
		0x01, 0x00, // compression methods
	)
	body = append(body, byte(len(exts)>>8), byte(len(exts)))
	body = append(body, exts...)
	msg := []byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(msg, body...)
}
