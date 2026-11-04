package handshake

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// testIdentity is a node with an account key, one device key and a handshake key, all derived
// from a small number so tests are reproducible.
type testIdentity struct {
	account ed25519.PrivateKey
	device  ed25519.PrivateKey
	hs      *ecdh.PrivateKey
}

func testSeed(label string, n int) []byte {
	h := sha256.Sum256([]byte("echofog handshake test: " + label + string(rune('0'+n))))
	return h[:]
}

func newTestIdentity(t testing.TB, n int) *testIdentity {
	t.Helper()
	hs, err := ecdh.X25519().NewPrivateKey(testSeed("hs", n))
	if err != nil {
		t.Fatal(err)
	}
	return &testIdentity{
		account: ed25519.NewKeyFromSeed(testSeed("account", n)),
		device:  ed25519.NewKeyFromSeed(testSeed("device", n)),
		hs:      hs,
	}
}

func akeyEd(pub ed25519.PublicKey) []byte { return append([]byte{AlgEd25519}, pub...) }

func (k *testIdentity) deviceID() [32]byte {
	return sha256.Sum256(akeyEd(k.device.Public().(ed25519.PublicKey)))
}

func (k *testIdentity) nodeID() [32]byte {
	return sha256.Sum256(akeyEd(k.account.Public().(ed25519.PublicKey)))
}

func testEnvelope(label string, signer ed25519.PrivateKey, seq, notAfter uint64, body []byte) []byte {
	var b []byte
	b = append(b, 0x02, byte(len(label)))
	b = append(b, label...)
	b = append(b, AlgEd25519)
	b = append(b, signer.Public().(ed25519.PublicKey)...)
	b = binary.BigEndian.AppendUint64(b, seq)
	b = binary.BigEndian.AppendUint64(b, notAfter)
	b = appendQUICVarint(b, uint64(len(body)))
	b = append(b, body...)
	return append(b, ed25519.Sign(signer, b)...)
}

func (k *testIdentity) deviceList(t testing.TB, now uint64) []byte {
	t.Helper()
	dev := new(pbWriter).bytes(1, akeyEd(k.device.Public().(ed25519.PublicKey))).uint(2, now).uint(3, 1).b
	body := new(pbWriter).bytes(1, dev).b
	return testEnvelope(LabelDevices, k.account, now, now+30*86400, body)
}

// CardOptions changes a test card.
type CardOptions struct {
	HsKey    []byte   // default: the identity's hs public key
	Suites   []uint16 // default: [1, 2]
	Scope    uint32
	Seq      uint64 // default: fixedTime
	PrevHs   []byte
	InviteID []byte
}

func (k *testIdentity) card(t testing.TB, o CardOptions) []byte {
	return k.cardWithDevices(t, k.deviceList(t, fixedTime), o)
}

func (k *testIdentity) cardWithDevices(t testing.TB, devices []byte, o CardOptions) []byte {
	t.Helper()
	hs := o.HsKey
	if hs == nil {
		hs = k.hs.PublicKey().Bytes()
	}
	suites := o.Suites
	if suites == nil {
		suites = []uint16{SuiteXK, SuiteXX}
	}
	var s []uint64
	for _, x := range suites {
		s = append(s, uint64(x))
	}
	w := new(pbWriter).uint(1, 200).bytes(2, append([]byte{AlgX25519}, hs...)).packed(3, s...)
	w.uint(6, 0).uint(7, 0)
	w.bytes(13, akeyEd(k.account.Public().(ed25519.PublicKey)))
	if o.PrevHs != nil {
		w.bytes(15, new(pbWriter).bytes(1, append([]byte{AlgX25519}, o.PrevHs...)).uint(2, fixedTime+6*86400).b)
	}
	w.uint(16, uint64(o.Scope)).bytes(17, o.InviteID).bytes(18, devices)
	seq := o.Seq
	if seq == 0 {
		seq = fixedTime
	}
	return testEnvelope(LabelCard, k.device, seq, fixedTime+6*86400, w.b)
}
