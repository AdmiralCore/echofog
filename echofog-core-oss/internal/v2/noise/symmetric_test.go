package noise

import (
	"bytes"
	"testing"
)

func TestInitializeSymmetricVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	list, ok := v["initialize_symmetric"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("initialize_symmetric: want 2 entries")
	}
	for _, e := range list {
		m := e.(map[string]any)
		name := m["protocol_name"].(string)
		ss := NewSymmetricState(name)
		if got, want := ss.H(), hexField(t, m, "h0"); !bytes.Equal(got, want) {
			t.Errorf("%s: h0 = %x, want %x", name, got, want)
		}
		if got, want := ss.CK(), hexField(t, m, "ck0"); !bytes.Equal(got, want) {
			t.Errorf("%s: ck0 = %x, want %x", name, got, want)
		}
	}
}

func TestShortProtocolNamePadded(t *testing.T) {
	// Noise §5.2: a name of at most HASHLEN bytes is zero-padded, not hashed.
	short := NewSymmetricState("abc")
	want := make([]byte, HashLen)
	copy(want, "abc")
	if !bytes.Equal(short.H(), want) || !bytes.Equal(short.CK(), want) {
		t.Fatalf("short name: h = %x", short.H())
	}
}

func TestPrologueAndPremessageHash(t *testing.T) {
	v := loadVector(t, "handshake.json")
	xk := obj(t, v, "prologue_xk")
	ss := NewSymmetricState(NameXKhfs)
	ss.MixHash(hexField(t, xk, "prologue"))
	if got, want := ss.H(), hexField(t, xk, "h_after_prologue"); !bytes.Equal(got, want) {
		t.Fatalf("XK h after prologue = %x, want %x", got, want)
	}
	ss.MixHash(hexField(t, xk, "responder_static_rs"))
	if got, want := ss.H(), hexField(t, xk, "h_after_premessage_rs"); !bytes.Equal(got, want) {
		t.Fatalf("XK h after pre-message = %x, want %x", got, want)
	}

	xx := obj(t, v, "prologue_xx_bare_address")
	ss = NewSymmetricState(NameXXhfs)
	ss.MixHash(hexField(t, xx, "prologue"))
	if got, want := ss.H(), hexField(t, xx, "h_after_prologue"); !bytes.Equal(got, want) {
		t.Fatalf("XX h after prologue = %x, want %x", got, want)
	}
}

func TestHKDFSplitVectors(t *testing.T) {
	v := loadVector(t, "kdf.json")
	s := obj(t, v, "session")
	ck := hexField(t, s, "ck")
	o1, o2 := HKDF2(ck, nil)
	if want := hexField(t, s, "split_k_i2r"); !bytes.Equal(o1, want) {
		t.Errorf("split o1 = %x, want %x", o1, want)
	}
	if want := hexField(t, s, "split_k_r2i"); !bytes.Equal(o2, want) {
		t.Errorf("split o2 = %x, want %x", o2, want)
	}
	sec, _ := HKDF2(ck, []byte("echofog/hs2/session-secret"))
	if want := hexField(t, s, "session_secret"); !bytes.Equal(sec, want) {
		t.Errorf("session secret = %x, want %x", sec, want)
	}
}

func TestHKDF3Prefix(t *testing.T) {
	ck := bytes.Repeat([]byte{7}, 32)
	a1, a2 := HKDF2(ck, []byte("x"))
	b1, b2, b3 := HKDF3(ck, []byte("x"))
	if !bytes.Equal(a1, b1) || !bytes.Equal(a2, b2) || len(b3) != 32 || bytes.Equal(b3, b2) {
		t.Fatal("HKDF3 must extend HKDF2")
	}
}

func TestCipherStateNonceAndNoKey(t *testing.T) {
	var cs CipherState
	pt := []byte("plain")
	out, err := cs.EncryptWithAd(nil, []byte("ad"), pt)
	if err != nil || !bytes.Equal(out, pt) {
		t.Fatalf("no key: encrypt must be identity, got %x %v", out, err)
	}
	cs.InitializeKey(bytes.Repeat([]byte{1}, 32))
	c0, _ := cs.EncryptWithAd(nil, nil, pt)
	c1, _ := cs.EncryptWithAd(nil, nil, pt)
	if bytes.Equal(c0, c1) || len(c0) != len(pt)+TagLen {
		t.Fatal("nonce must advance and a tag must be appended")
	}
	var rx CipherState
	rx.InitializeKey(bytes.Repeat([]byte{1}, 32))
	if p, err := rx.DecryptWithAd(nil, nil, c0); err != nil || !bytes.Equal(p, pt) {
		t.Fatalf("decrypt c0: %v", err)
	}
	// A failed decryption does not advance the nonce (Noise §5.1).
	bad := append([]byte{}, c1...)
	bad[0] ^= 1
	if _, err := rx.DecryptWithAd(nil, nil, bad); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if p, err := rx.DecryptWithAd(nil, nil, c1); err != nil || !bytes.Equal(p, pt) {
		t.Fatalf("decrypt c1 after failure: %v", err)
	}
}
