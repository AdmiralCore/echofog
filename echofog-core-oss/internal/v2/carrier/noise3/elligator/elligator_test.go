package elligator

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const vectorPath = "../../../testdata/elligator2.json"

type vectors struct {
	TOrder8 string `json:"T_order8"`
	Samples []struct {
		SeedBase  string `json:"seed_base"`
		Attempt   int    `json:"attempt"`
		SK        string `json:"sk"`
		SK0Low3   byte   `json:"sk0_low3"`
		Tweak     string `json:"tweak"`
		CleanPub  string `json:"clean_pub"`
		DirtyPub  string `json:"dirty_pub"`
		Repr      string `json:"repr"`
		Forward   string `json:"forward_repr"`
		NoReprKey int    `json:"keys_without_representative_before"`
	} `json:"samples"`
	ForwardOnly []struct {
		Repr string `json:"repr"`
		U    string `json:"u"`
	} `json:"forward_only"`
	NoRepresentative struct {
		SK       string `json:"sk"`
		DirtyPub string `json:"dirty_pub"`
	} `json:"no_representative"`
	RFCCrosscheck struct {
		Cases []struct {
			U0BE string `json:"u0_be"`
			QXBE string `json:"Q.x_be"`
		} `json:"cases"`
	} `json:"rfc9380_crosscheck"`
	ECDHCheck struct {
		PeerSK           string `json:"peer_sk"`
		PeerSKDirty      string `json:"x25519_peer_sk_sample0_dirty_pub"`
		PeerSKClean      string `json:"x25519_peer_sk_sample0_clean_pub"`
		SampleSKPeerPub  string `json:"x25519_sample0_sk_peer_pub"`
	} `json:"ecdh_check"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return v
}

func hx(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func x25519(t *testing.T, sk, pub []byte) []byte {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(sk)
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	z, err := k.ECDH(mustPublic(t, pub))
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	return z
}

func mustPublic(t *testing.T, pub []byte) *ecdh.PublicKey {
	t.Helper()
	p, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	return p
}

func TestSamples(t *testing.T) {
	v := loadVectors(t)
	for i, s := range v.Samples {
		sk := hx(t, s.SK)
		tweak := hx(t, s.Tweak)[0]
		dirty, clean, err := DirtyPublic(sk)
		if err != nil {
			t.Fatalf("sample %d: DirtyPublic: %v", i, err)
		}
		if got := hex.EncodeToString(dirty); got != s.DirtyPub {
			t.Errorf("sample %d: dirty = %s, want %s", i, got, s.DirtyPub)
		}
		if got := hex.EncodeToString(clean); got != s.CleanPub {
			t.Errorf("sample %d: clean = %s, want %s", i, got, s.CleanPub)
		}
		repr, ok := Reverse(dirty, tweak)
		if !ok {
			t.Fatalf("sample %d: Reverse reported no representative", i)
		}
		if got := hex.EncodeToString(repr); got != s.Repr {
			t.Errorf("sample %d: repr = %s, want %s", i, got, s.Repr)
		}
		if got := hex.EncodeToString(Forward(repr)); got != s.Forward {
			t.Errorf("sample %d: Forward(repr) = %s, want %s", i, got, s.Forward)
		}
		if s.SK0Low3 != sk[0]&7 {
			t.Errorf("sample %d: sk low bits = %d, want %d", i, sk[0]&7, s.SK0Low3)
		}
	}
}

func TestForwardOnly(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.ForwardOnly {
		got := Forward(hx(t, c.Repr))
		if hex.EncodeToString(got) != c.U {
			t.Errorf("case %d: Forward = %s, want %s", i, hex.EncodeToString(got), c.U)
		}
	}
}

func TestNoRepresentative(t *testing.T) {
	v := loadVectors(t)
	dirty := hx(t, v.NoRepresentative.DirtyPub)
	if repr, ok := Reverse(dirty, 0); ok {
		t.Fatalf("Reverse found a representative %x for a non-square point", repr)
	}
}

func TestRFCCrosscheck(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.RFCCrosscheck.Cases {
		u0 := reverseBytes(hx(t, c.U0BE))
		got := Map(u0)
		if hex.EncodeToString(reverseBytes(got)) != c.QXBE {
			t.Errorf("case %d: Map = %s, want %s", i, hex.EncodeToString(reverseBytes(got)), c.QXBE)
		}
	}
}

func TestECDH(t *testing.T) {
	v := loadVectors(t)
	peer := hx(t, v.ECDHCheck.PeerSK)
	s := v.Samples[0]
	dirty := hx(t, s.DirtyPub)
	clean := hx(t, s.CleanPub)
	sk := hx(t, s.SK)
	zDirty := x25519(t, peer, dirty)
	zClean := x25519(t, peer, clean)
	zInit := x25519(t, sk, x25519Pub(t, peer))
	if !bytes.Equal(zDirty, zClean) || !bytes.Equal(zClean, zInit) {
		t.Errorf("ECDH differs: %x %x %x", zDirty, zClean, zInit)
	}
	if hex.EncodeToString(zDirty) != v.ECDHCheck.PeerSKDirty ||
		hex.EncodeToString(zInit) != v.ECDHCheck.SampleSKPeerPub {
		t.Errorf("ECDH does not match the vector")
	}
}

func TestGenerate(t *testing.T) {
	for i := 0; i < 200; i++ {
		sk, repr, err := Generate(rand.Reader)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		dirty, clean, err := DirtyPublic(sk)
		if err != nil {
			t.Fatalf("DirtyPublic: %v", err)
		}
		if !bytes.Equal(Forward(repr), dirty) {
			t.Fatalf("iteration %d: forward(repr) != dirty public key", i)
		}
		z1 := x25519(t, sk, x25519Pub(t, clean))
		z2 := x25519(t, sk, x25519Pub(t, dirty))
		if !bytes.Equal(z1, z2) {
			t.Fatalf("iteration %d: torsion changed the shared secret", i)
		}
	}
}

func x25519Pub(t *testing.T, sk []byte) []byte {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(sk)
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	return k.PublicKey().Bytes()
}

func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

func TestTestdataCopyMatchesSpec(t *testing.T) {
	spec := filepath.Join("..", "..", "..", "..", "..", "docs", "protocol", "v2", "vectors", "elligator2.json")
	if _, err := os.Stat(spec); err != nil {
		t.Skip("spec vectors not present (OSS copy)")
	}
	a, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(a, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))) {
		t.Error("testdata copy differs from docs/protocol/v2/vectors")
	}
}
