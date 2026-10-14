package mimicry

import "testing"

// FuzzVerifyStealthToken: the first bytes a stranger sends after the TLS handshake. Anything that
// is not a token made with the node's key must be refused, quickly and without panicking.
func FuzzVerifyStealthToken(f *testing.F) {
	key := NodeStealthKey("", [32]byte{1})
	aead, err := DeriveAEAD(key)
	if err != nil {
		f.Fatal(err)
	}
	good, err := GenerateStealthToken(aead)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add(good[:len(good)-1])
	f.Add(make([]byte, StealthTokenLen))
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, token []byte) {
		err := VerifyStealthToken(aead, token)
		if err == nil && len(token) != StealthTokenLen {
			t.Fatalf("accepted a token of %d bytes", len(token))
		}
	})
}

// Whatever key and wrong token: refusing is the only outcome without the key.
func FuzzStealthTokenWrongKey(f *testing.F) {
	f.Add([]byte("a"), []byte("b"))
	f.Fuzz(func(t *testing.T, k1, k2 []byte) {
		if string(k1) == string(k2) {
			return
		}
		a1, err1 := DeriveAEAD(StealthKeyFromPassword(string(k1)))
		a2, err2 := DeriveAEAD(StealthKeyFromPassword(string(k2)))
		if err1 != nil || err2 != nil {
			t.Skip()
		}
		token, err := GenerateStealthToken(a1)
		if err != nil {
			t.Skip()
		}
		if VerifyStealthToken(a2, token) == nil {
			t.Fatalf("a token verified under another key")
		}
	})
}
