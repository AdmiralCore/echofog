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
const testdata = "../../testdata"

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
	MaskSK        string `json:"mask_sk"`
	MaskPub       string `json:"mask_pub"`
	ClientSKC     string `json:"client_sk_c"`
	ClientShareC  string `json:"client_share_C"`
	DialedAddress string `json:"dialed_address"`
	Z             string `json:"z"`
	KTok          string `json:"k_tok"`
	KTokOther     string `json:"k_tok_at_198.51.100.7:443"`
	CH0           string `json:"ch0"`
	AAD           string `json:"aad"`
	TS            uint32 `json:"ts"`
	Plaintext     string `json:"plaintext"`
	Token         string `json:"token"`
	HelloWithTok  string `json:"client_hello_with_token"`
	Hybrid        struct {
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
	if got := TokenKey(z, maskPub, C, "198.51.100.7:443"); !bytes.Equal(got, unhex(t, v.KTokOther)) {
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
	withToken := unhex(t, v.HelloWithTok)
	if !bytes.Equal(withToken[randomOffset:randomOffset+32], token) {
		t.Fatalf("hello random = %x, want the token %x", withToken[randomOffset:randomOffset+32], token)
	}
}

// TestTokenOpen checks the success path and the address binding of wire.md §3.2: the same token
// does not open at another address of the same node.
func TestTokenOpen(t *testing.T) {
	v := loadReality2Vector(t)
	sk := maskKey(t, unhex(t, v.MaskSK))
	maskPub := unhex(t, v.MaskPub)
	C := unhex(t, v.ClientShareC)
	ch0 := unhex(t, v.CH0)
	aad := HelloAAD(ch0)
	now := time.Unix(int64(v.TS), 0)

	z, err := sk.ECDH(mustPublic(t, C))
	if err != nil {
		t.Fatal(err)
	}
	key := TokenKey(z, maskPub, C, v.DialedAddress)
	token, err := SealToken(key, aad, v.TS)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := OpenToken(key, aad, token, now)
	if err != nil {
		t.Fatalf("open token: %v", err)
	}
	if ts != v.TS {
		t.Fatalf("token timestamp %d, want %d", ts, v.TS)
	}
	other := TokenKey(z, maskPub, C, "198.51.100.7:443")
	if _, err := OpenToken(other, aad, token, now); err == nil {
		t.Fatal("token opened under another address")
	}
}

// TestParseHello checks the parser on a well-formed hello: token from random, zeroed CH0, C from
// the X25519 share, and C from the hybrid share when there is no X25519 entry.
func TestParseHello(t *testing.T) {
	v := loadReality2Vector(t)
	C := unhex(t, v.ClientShareC)
	token := unhex(t, v.Token)

	msg := syntheticHello(t, token, keyShareX25519(C))
	hello, err := ParseHello(msg)
	if err != nil {
		t.Fatalf("parse hello: %v", err)
	}
	if !bytes.Equal(hello.Token, token) {
		t.Fatalf("token = %x, want %x", hello.Token, token)
	}
	if !bytes.Equal(hello.C, C) {
		t.Fatalf("C = %x, want %x", hello.C, C)
	}
	if !bytes.Equal(hello.CH0[randomOffset:randomOffset+32], make([]byte, 32)) {
		t.Fatal("CH0 random is not zeroed")
	}
	if !bytes.Equal(hello.CH0[:randomOffset], msg[:randomOffset]) ||
		!bytes.Equal(hello.CH0[randomOffset+32:], msg[randomOffset+32:]) {
		t.Fatal("CH0 changed bytes outside the random")
	}

	// Hybrid only: C is the last 32 bytes of the X25519MLKEM768 share.
	hybridC := unhex(t, v.Hybrid.C)
	hello, err = ParseHello(syntheticHello(t, token, keyShareHybrid(hybridC)))
	if err != nil {
		t.Fatalf("parse hybrid hello: %v", err)
	}
	if !bytes.Equal(hello.C, hybridC) {
		t.Fatalf("hybrid C = %x, want %x", hello.C, hybridC)
	}

	// Both shares: the X25519 entry wins (wire.md §3.1).
	hello, err = ParseHello(syntheticHello(t, token, append(keyShareX25519(C), keyShareHybrid(hybridC)...)))
	if err != nil {
		t.Fatalf("parse both-shares hello: %v", err)
	}
	if !bytes.Equal(hello.C, C) {
		t.Fatalf("C with both shares = %x, want the X25519 entry %x", hello.C, C)
	}
}

// TestTokenRejects checks the token format rules of wire.md §3.3 step 2 alone, without a server.
func TestTokenRejects(t *testing.T) {
	v := loadReality2Vector(t)
	kTok := unhex(t, v.KTok)
	aad := HelloAAD(unhex(t, v.CH0))
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
		{"wrong aad", good, HelloAAD(syntheticHello(t, nil, nil)), now},
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
	base := syntheticHello(t, nil, keyShareX25519(make([]byte, 32)))
	cases := map[string][]byte{
		"empty":          nil,
		"short":          base[:10],
		"not a hello":    append([]byte{2}, base[1:]...),
		"truncated body": base[:len(base)-1],
		"bad handshake":  append([]byte{1, 0xff, 0xff, 0xff}, base[4:]...),
		"no extensions":  syntheticHello(t, nil, nil),
		"zero length":    {1, 0, 0, 0},
		"one byte":       {1},
		"random garbage": bytes.Repeat([]byte{0x5a}, 64),
		"short share":    syntheticHello(t, nil, []byte{0, 0, 0x00, 0x1d, 0x00, 0x10, 1, 2, 3}),
		"bad share len":  syntheticHello(t, nil, []byte{0, 0, 0x00, 0x1d, 0xff, 0xff, 1, 2, 3}),
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHello(msg); err == nil {
				t.Fatal("malformed hello parsed")
			}
		})
	}
}

func mustPublic(t testing.TB, b []byte) *ecdh.PublicKey {
	t.Helper()
	p, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// keyShareX25519 returns the shares part of a key_share extension body for one X25519 share.
func keyShareX25519(C []byte) []byte {
	shares := []byte{0x00, 0x1d, 0x00, 0x20}
	shares = append(shares, C...)
	return withSharesLen(shares)
}

// keyShareHybrid returns the shares part of a key_share extension body for one X25519MLKEM768
// share whose X25519 public key is C.
func keyShareHybrid(C []byte) []byte {
	shares := []byte{0x11, 0xec, 0x04, 0xc0}
	shares = append(shares, bytes.Repeat([]byte{0x11}, mlkemEncapKeyLen)...)
	shares = append(shares, C...)
	return withSharesLen(shares)
}

func withSharesLen(shares []byte) []byte {
	return append([]byte{byte(len(shares) >> 8), byte(len(shares))}, shares...)
}

// syntheticHello builds a ClientHello handshake message that carries random in ClientHello.random
// and the given key_share extension body, so the token parser can be exercised without a TLS
// stack. asShares, when non-nil, is already a complete key_share extension body.
func syntheticHello(t testing.TB, random, asShares []byte) []byte {
	t.Helper()
	if random == nil {
		random = make([]byte, 32)
	}
	var exts []byte
	if asShares != nil {
		exts = append(exts, 0x00, 0x33, byte(len(asShares)>>8), byte(len(asShares)))
		exts = append(exts, asShares...)
	}
	body := []byte{0x03, 0x03}
	body = append(body, random...)
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
