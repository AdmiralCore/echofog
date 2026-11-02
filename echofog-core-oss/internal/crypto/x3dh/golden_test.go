package x3dh

import (
	"encoding/hex"
	"testing"

	"github.com/awnumar/memguard"
	"golang.org/x/crypto/curve25519"
)

// fixedKey is a private key made of one repeated byte, and its public key.
func fixedKey(b byte) (*memguard.Enclave, [32]byte) {
	var priv, pub [32]byte
	for i := range priv {
		priv[i] = b
	}
	curve25519.ScalarBaseMult(&pub, &priv)
	return memguard.NewEnclave(priv[:]), pub
}

// The classic X3DH secret must stay byte for byte what v0.2.0 computes, or sessions between old and
// new nodes break. The values were taken from the code before PQXDH was added.
func TestClassicX3DHMatchesV020(t *testing.T) {
	defer memguard.Purge()
	for _, c := range []struct {
		withOPK bool
		want    string
	}{
		{true, "3a0fa2d647c841031bc2f4556c183e5595d4134563ce2a7e8b6e8b4b043986f1"},
		{false, "0f27b30783e222642c00eabb7ffe87616a0c721240497339665692d76377d4d1"},
	} {
		ikA, ikApub := fixedKey(1)
		ekA, ekApub := fixedKey(2)
		ikB, ikBpub := fixedKey(3)
		spkB, spkBpub := fixedKey(4)
		opkB, opkBpub := fixedKey(5)
		p := X3DHParameters{
			MyIdentityPriv: ikA, MyEphemeralPriv: ekA,
			RemoteIdentityPub: ikBpub, RemoteSignedPrePub: spkBpub,
		}
		if c.withOPK {
			p.RemoteOneTimePub = &opkBpub
		} else {
			opkB = nil
		}
		root, err := CalculateX3DH(p)
		if err != nil {
			t.Fatal(err)
		}
		got := openHex(t, root)
		if got != c.want {
			t.Fatalf("classic X3DH secret (one-time prekey: %v) changed: %s", c.withOPK, got)
		}
		back, err := CalculateX3DH_Receiver(ikB, spkB, opkB, ikApub, ekApub)
		if err != nil {
			t.Fatal(err)
		}
		if openHex(t, back) != got {
			t.Fatal("both sides must agree")
		}
	}
}

func openHex(t *testing.T, e *memguard.Enclave) string {
	t.Helper()
	l, err := e.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Destroy()
	return hex.EncodeToString(l.Bytes())
}
