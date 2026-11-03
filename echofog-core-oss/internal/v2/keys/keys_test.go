package keys

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRFC8032Key(t *testing.T) {
	s, err := NewEd25519Signer(unhex(t, "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"))
	if err != nil {
		t.Fatal(err)
	}
	pub := s.Public()
	if hex.EncodeToString(pub) != "01d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a" {
		t.Fatalf("akey %x", pub)
	}
	if pub.Text() != "ed25519:11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo" {
		t.Fatalf("text %s", pub.Text())
	}
	id := pub.ID()
	if hex.EncodeToString(id[:]) != "bcd1d56b5845f21e54ce5b764fc1d5520cc2c462e08fade2668dae9353621237" {
		t.Fatalf("id %x", id)
	}
	sig := s.Sign(nil)
	if hex.EncodeToString(sig) != "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b" {
		t.Fatal("RFC 8032 TEST 1 signature")
	}
	if err := Verify(pub, nil, sig); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, []byte{0}, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong message: %v", err)
	}
}

func TestParse(t *testing.T) {
	ok := [][]byte{New(Ed25519, make([]byte, 32)), New(X25519, make([]byte, 32)), New(MLKEM768, make([]byte, 1184))}
	for _, k := range ok {
		if _, err := Parse(k); err != nil {
			t.Errorf("%s: %v", Alg(k[0]), err)
		}
	}
	for name, c := range map[string]struct {
		b   []byte
		err error
	}{
		"empty":           {nil, ErrMalformed},
		"short ed25519":   {New(Ed25519, make([]byte, 31)), ErrMalformed},
		"long x25519":     {New(X25519, make([]byte, 33)), ErrMalformed},
		"reserved 0x00":   {New(0, make([]byte, 32)), ErrUnknownAlg},
		"reserved mldsa":  {New(MLDSA65, make([]byte, 1952)), ErrUnknownAlg},
		"reserved hybrid": {New(Ed25519MLDSA65, make([]byte, 1984)), ErrUnknownAlg},
		"experimental":    {New(0xf3, make([]byte, 32)), ErrUnknownAlg},
	} {
		if _, err := Parse(c.b); !errors.Is(err, c.err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseKind(New(X25519, make([]byte, 32)), Ed25519); !errors.Is(err, ErrWrongKind) {
		t.Errorf("ParseKind: %v", err)
	}
	if Ed25519.String() != "ed25519" || Alg(0x99).String() != "alg(0x99)" || !Ed25519.IsSignature() || X25519.IsSignature() {
		t.Error("names")
	}
}

func TestText(t *testing.T) {
	k := New(X25519, bytes.Repeat([]byte{0xfb}, 32))
	back, err := ParseText(k.Text())
	if err != nil || !back.Equal(k) {
		t.Fatalf("round trip %q: %v", k.Text(), err)
	}
	for _, s := range []string{"", "ed25519", "rsa:AAAA", "ed25519:AAAA", "ed25519:11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo=",
		"ed25519:11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo"} {
		if _, err := ParseText(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// Small-order points of edwards25519 (encodings of the 8 torsion points and a non-canonical one).
var smallOrder = []string{
	"0100000000000000000000000000000000000000000000000000000000000000", // identity
	"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // order 2
	"0000000000000000000000000000000000000000000000000000000000000000", // order 4
	"0000000000000000000000000000000000000000000000000000000000000080", // order 4
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a", // order 8
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa", // order 8
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", // order 8
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85", // order 8
	"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // non-canonical (y = p + 1)
}

func TestSmallOrderRejected(t *testing.T) {
	for _, h := range smallOrder {
		pub := unhex(t, h)
		if err := CheckEd25519Public(pub); err == nil {
			t.Errorf("%s accepted", h)
		}
		// An all-zero signature verifies against some small-order keys in cofactored or lax
		// implementations; Verify must refuse before trying.
		if err := Verify(New(Ed25519, pub), []byte("m"), make([]byte, 64)); err == nil {
			t.Errorf("%s: Verify accepted", h)
		}
	}
	good, _, _ := ed25519.GenerateKey(nil)
	if err := CheckEd25519Public(good); err != nil {
		t.Fatal(err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(New(Ed25519, make([]byte, 32))))
	f.Add([]byte{0x03})
	f.Fuzz(func(t *testing.T, b []byte) {
		k, err := Parse(b)
		if err != nil {
			return
		}
		if !bytes.Equal(k, b) || len(k) != 1+k.Alg().KeyLen() {
			t.Fatal("Parse changed the key")
		}
		back, err := ParseText(k.Text())
		if err != nil || !back.Equal(k) {
			t.Fatalf("text round trip: %v", err)
		}
		if k.Alg() == Ed25519 {
			_ = CheckEd25519Public(k.Key())
			_ = Verify(k, b, make([]byte, 64))
		}
	})
}
