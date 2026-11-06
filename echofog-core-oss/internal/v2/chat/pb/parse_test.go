package chatpb

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"google.golang.org/protobuf/proto"
)

func rep(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func samples(t testing.TB) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/v1_samples.json")
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]string
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for k, v := range in {
		b, err := hex.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

func goodMessage() *SecureMessage {
	return &SecureMessage{Header: &RatchetHeader{RatchetKey: rep(0x21, 32), Pn: 7, N: 300}, Ciphertext: rep(0xc3, 61)}
}

func goodInit() *X3DHInit {
	return &X3DHInit{EphemeralKey: rep(1, 32), IdentityKey: rep(2, 32), OneTimeKeyId: rep(3, 32), InitialCiphertext: goodMessage(),
		KemCiphertext: rep(4, KEMCiphertextLen), KemKeyId: rep(5, 32)}
}

func mustMarshal(t testing.TB, m proto.Message) []byte {
	t.Helper()
	b, err := objects.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The copies encode to the bytes v1 produced (the messages keep their field numbers).
func TestBytesEqualV1(t *testing.T) {
	t.Parallel()
	s := samples(t)
	withTime := goodInit()
	withTime.BundleCreatedAt = 1760000000
	noOPK := goodInit()
	noOPK.OneTimeKeyId = nil
	cases := []struct {
		name string
		msg  proto.Message
		new  func() proto.Message
	}{
		{"secure_message", goodMessage(), func() proto.Message { return &SecureMessage{} }},
		{"x3dh_init", withTime, func() proto.Message { return &X3DHInit{} }},
		{"x3dh_init_no_opk", noOPK, func() proto.Message { return &X3DHInit{} }},
		{"x3dh_init_classic", &X3DHInit{EphemeralKey: rep(1, 32), IdentityKey: rep(2, 32), InitialCiphertext: goodMessage(), SenderBundle: rep(9, 40), BundleCreatedAt: 5}, func() proto.Message { return &X3DHInit{} }},
	}
	for _, tc := range cases {
		want, ok := s[tc.name]
		if !ok {
			t.Fatalf("no v1 sample %q", tc.name)
		}
		if got := mustMarshal(t, tc.msg); !bytes.Equal(got, want) {
			t.Errorf("%s: v2 encoding differs from v1:\n got %x\nwant %x", tc.name, got, want)
		}
		m := tc.new()
		if err := proto.Unmarshal(want, m); err != nil {
			t.Fatalf("%s: v1 bytes do not decode: %v", tc.name, err)
		}
		if got := mustMarshal(t, m); !bytes.Equal(got, want) {
			t.Errorf("%s: decode and encode of v1 bytes changed them", tc.name)
		}
	}
}

func TestParseSecureMessage(t *testing.T) {
	t.Parallel()
	mod := func(f func(m *SecureMessage)) []byte { m := goodMessage(); f(m); return mustMarshal(t, m) }
	cases := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"v1 sample", samples(t)["secure_message"], true},
		{"minimal ciphertext", mod(func(m *SecureMessage) { m.Ciphertext = rep(1, MinCiphertext) }), true},
		{"maximal ciphertext", mod(func(m *SecureMessage) { m.Ciphertext = rep(1, MaxCiphertext) }), true},
		{"empty", nil, false},
		{"no header", mod(func(m *SecureMessage) { m.Header = nil }), false},
		{"short ratchet key", mod(func(m *SecureMessage) { m.Header.RatchetKey = rep(1, 31) }), false},
		{"long ratchet key", mod(func(m *SecureMessage) { m.Header.RatchetKey = rep(1, 33) }), false},
		{"short ciphertext", mod(func(m *SecureMessage) { m.Ciphertext = rep(1, MinCiphertext-1) }), false},
		{"long ciphertext", mod(func(m *SecureMessage) { m.Ciphertext = rep(1, MaxCiphertext+1) }), false},
		{"non-minimal varint", append(mustMarshal(t, goodMessage()), 0x18, 0x80, 0x00), false},
		{"truncated", mustMarshal(t, goodMessage())[:20], false},
	}
	for _, tc := range cases {
		m, err := ParseSecureMessage(tc.in)
		if tc.ok && (err != nil || m == nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", tc.name, err)
		}
	}
}

func TestParseX3DHInit(t *testing.T) {
	t.Parallel()
	mod := func(f func(i *X3DHInit)) []byte { i := goodInit(); f(i); return mustMarshal(t, i) }
	swapped := func() []byte { // field 2 before field 1
		b := mustMarshal(t, goodInit())
		rest := b[2+32+2+32:]
		out := append([]byte{0x12, 32}, rep(2, 32)...)
		out = append(out, 0x0a, 32)
		out = append(out, rep(1, 32)...)
		return append(out, rest...)
	}
	cases := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"pqxdh with one-time prekey", mustMarshal(t, goodInit()), true},
		{"pqxdh from a DHT copy (no one-time prekey)", mod(func(i *X3DHInit) { i.OneTimeKeyId = nil }), true},
		{"v1 pqxdh sample without bundle time", samples(t)["x3dh_init_no_opk"], true},
		{"classic init is refused", samples(t)["x3dh_init_classic"], false},
		{"classic without sender bundle is refused", mod(func(i *X3DHInit) { i.KemCiphertext, i.KemKeyId = nil, nil }), false},
		{"kem ciphertext without key id", mod(func(i *X3DHInit) { i.KemKeyId = nil }), false},
		{"kem key id without ciphertext", mod(func(i *X3DHInit) { i.KemCiphertext = nil }), false},
		{"short kem ciphertext", mod(func(i *X3DHInit) { i.KemCiphertext = rep(4, KEMCiphertextLen-1) }), false},
		{"long kem ciphertext", mod(func(i *X3DHInit) { i.KemCiphertext = rep(4, KEMCiphertextLen+1) }), false},
		{"short kem key id", mod(func(i *X3DHInit) { i.KemKeyId = rep(5, 31) }), false},
		{"short ephemeral key", mod(func(i *X3DHInit) { i.EphemeralKey = rep(1, 31) }), false},
		{"no identity key", mod(func(i *X3DHInit) { i.IdentityKey = nil }), false},
		{"odd one-time prekey id", mod(func(i *X3DHInit) { i.OneTimeKeyId = rep(3, 31) }), false},
		{"no first message", mod(func(i *X3DHInit) { i.InitialCiphertext = nil }), false},
		{"bad first message", mod(func(i *X3DHInit) { i.InitialCiphertext.Header.RatchetKey = rep(1, 5) }), false},
		{"sender bundle set", mod(func(i *X3DHInit) { i.SenderBundle = rep(9, 40) }), false},
		{"bundle time set", mod(func(i *X3DHInit) { i.BundleCreatedAt = 1 }), false},
		{"empty", nil, false},
		{"non-canonical field order", swapped(), false},
	}
	for _, tc := range cases {
		m, err := ParseX3DHInit(tc.in)
		if tc.ok && (err != nil || m == nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", tc.name, err)
		}
	}
}

func TestParseBundleFrames(t *testing.T) {
	t.Parallel()
	if g, err := ParseBundleGet(mustMarshal(t, &BundleGet{Req: 9})); err != nil || g.Req != 9 {
		t.Fatalf("BUNDLE_GET: %v %v", g, err)
	}
	if _, err := ParseBundleGet(append(mustMarshal(t, &BundleGet{Req: 9}), 0x10, 0x80, 0x00)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-canonical BUNDLE_GET accepted: %v", err)
	}
	if b, err := ParseBundle(mustMarshal(t, &Bundle{Req: 2, Prekeys: rep(7, MaxPrekeys)})); err != nil || b.Req != 2 {
		t.Fatalf("BUNDLE: %v %v", b, err)
	}
	for name, in := range map[string][]byte{
		"empty prekeys": mustMarshal(t, &Bundle{Req: 2}),
		"too long":      mustMarshal(t, &Bundle{Req: 2, Prekeys: rep(7, MaxPrekeys+1)}),
		"garbage":       {0xff, 0xff},
	} {
		if _, err := ParseBundle(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("BUNDLE %s: want ErrInvalid, got %v", name, err)
		}
	}
}
