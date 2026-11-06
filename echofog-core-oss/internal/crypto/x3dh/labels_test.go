package x3dh

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// chat2Labels are the v2 labels of services §4.0. T7.0 does not use them in v1; the tests only
// prove that the KDF labels and the identity key source are parameters now (T7.1 passes these).
var chat2Labels = Labels{
	Session:    "echofog/chat2/pqxdh",
	PQXDH:      "echofog/chat2/pqxdh",
	InitChains: "echofog/chat2/ratchet-init",
	Root:       "echofog/chat2/ratchet-root",
}

// The v1 labels are wire values: sessions of v1 nodes are built with them, so they must not move.
func TestV1LabelsAreUnchanged(t *testing.T) {
	want := Labels{
		Session:    "FOG_X3DH_V2",
		PQXDH:      "FOG_PQXDH_X25519_SHA-256_ML-KEM-768_V1",
		InitChains: "FOG_RATCHET_INIT_V2",
		Root:       "FOG_RATCHET_ROOT_V2",
	}
	if V1Labels != want {
		t.Fatalf("the v1 labels changed: %+v", V1Labels)
	}
	if (Labels{}).resolved() != want {
		t.Fatal("an unset Labels must mean v1")
	}
}

// The label goes into the HKDF info exactly: the same DH inputs under the v2 label give another
// key, and that key is what HKDF with the v2 info computes.
func TestLabelsChangeTheSessionKey(t *testing.T) {
	defer memguard.Purge()

	ikA, _ := fixedKey(1)
	ekA, _ := fixedKey(2)
	_, ikBpub := fixedKey(3)
	_, spkBpub := fixedKey(4)
	p := X3DHParameters{
		MyIdentityPriv: ikA, MyEphemeralPriv: ekA,
		RemoteIdentityPub: ikBpub, RemoteSignedPrePub: spkBpub,
	}

	v1Key, err := CalculateX3DH(p)
	if err != nil {
		t.Fatal(err)
	}
	v2Key, err := CalculateX3DHWithLabels(p, chat2Labels)
	if err != nil {
		t.Fatal(err)
	}
	if openHex(t, v1Key) == openHex(t, v2Key) {
		t.Fatal("the KDF labels do not reach the session key")
	}
	if got, want := openHex(t, v2Key), independentX3DHSecret(t, p, chat2Labels.Session); got != want {
		t.Fatalf("v2 session key: got %s want %s", got, want)
	}
}

// independentX3DHSecret computes the classic X3DH key of the specification by hand, for a label.
func independentX3DHSecret(t *testing.T, p X3DHParameters, info string) string {
	t.Helper()
	dh := func(enc *memguard.Enclave, pub [32]byte) []byte {
		t.Helper()
		locked, err := enc.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer locked.Destroy()
		out, err := curve25519.X25519(locked.Bytes(), pub[:])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ikm := bytes.Repeat([]byte{0xFF}, 32)
	ikm = append(ikm, dh(p.MyIdentityPriv, p.RemoteSignedPrePub)...)
	ikm = append(ikm, dh(p.MyEphemeralPriv, p.RemoteIdentityPub)...)
	ikm = append(ikm, dh(p.MyEphemeralPriv, p.RemoteSignedPrePub)...)
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, make([]byte, 32), []byte(info)), key); err != nil {
		t.Fatal(err)
	}
	defer memguard.WipeBytes(key)
	return hex.EncodeToString(key)
}

// labeledParty is a node whose engine is built with a Config, the way v2 will build it.
type labeledParty struct {
	acc       *keymanager.AccountManager
	st        *MockStorage
	own       *keymanager.PublicBundle
	published *keymanager.PublicBundle
	engine    *X3DHEngine
}

func newLabeledParty(t *testing.T, cfg Config, opks int) labeledParty {
	t.Helper()
	st := NewMockStorage()
	root := make([]byte, 64)
	if _, err := rand.Read(root); err != nil {
		t.Fatal(err)
	}
	acc, err := keymanager.NewKeyManager(root, st).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	own, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	published := own
	if opks > 0 {
		if published, err = acc.CreateBundleWithOPKs(opks); err != nil {
			t.Fatal(err)
		}
	}
	return labeledParty{
		acc: acc, st: st, own: own, published: published,
		engine: NewX3DHEngineWithConfig(own, st, acc, cfg),
	}
}

// Both sides of a session must use the same labels: with the chat/2 labels they talk, while a v1
// peer cannot open the init (another session key).
func TestCustomLabelsEndToEnd(t *testing.T) {
	defer memguard.Purge()

	a := newLabeledParty(t, Config{Labels: chat2Labels}, 2)
	b := newLabeledParty(t, Config{Labels: chat2Labels}, 0)

	sa, init, err := a.engine.X3DHInit(*b.published)
	if err != nil {
		t.Fatal(err)
	}
	init.SecureMessage, err = sa.RatchetEncrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	// the same account and bundle with the v1 labels: the session key differs, so the init does
	// not open (and the one-time prekey is not used up by a failed attempt)
	v1b := NewX3DHEngine(b.own, b.st, b.acc)
	if _, _, err := v1b.Accept(init, *a.published); !errors.Is(err, ErrInitRejected) {
		t.Fatalf("a v1 peer opened a chat/2 init: %v", err)
	}

	sb, plain, err := b.engine.Accept(init, *a.published)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "hello" {
		t.Fatalf("got %q", plain)
	}
	talk(t, sa, sb)
}

// staticIdentity is an identity key source that is not the account derivation: v2 will keep a
// random key per device this way.
type staticIdentity struct {
	enc *memguard.Enclave
	pub []byte
}

func (s staticIdentity) GetEncryptionKey() (*memguard.Enclave, []byte, error) {
	return s.enc, s.pub, nil
}

func TestIdentityKeySourceIsParameter(t *testing.T) {
	defer memguard.Purge()

	a := newLabeledParty(t, Config{}, 0)

	want := bytes.Repeat([]byte{9}, 32)
	var pub [32]byte
	copy(pub[:], want)
	curve25519.ScalarBaseMult(&pub, &pub)
	enc := memguard.NewEnclave(append([]byte(nil), want...))
	eng := NewX3DHEngineWithConfig(a.own, a.st, a.acc, Config{Identity: staticIdentity{enc: enc, pub: pub[:]}})

	got, err := eng.identityKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened(t, got), want) {
		t.Fatal("the engine ignored the identity key source")
	}

	// the default is still the account derivation of v1
	accKey, _, err := a.acc.GetEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	def, err := a.engine.identityKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened(t, def), opened(t, accKey)) {
		t.Fatal("a default engine must derive the identity key from the account")
	}
}

// A stored session keeps the labels it was made with; sessions stored before labels existed are v1.
func TestSessionLabelsSurviveTheStore(t *testing.T) {
	defer memguard.Purge()

	shared := memguard.NewEnclave(bytes.Repeat([]byte{3}, 32))
	eph, pub := fixedKey(4)
	s, err := NewX3DHSessionWithLabels(shared, eph, types.PeerID{1}, pub, true, chat2Labels)
	if err != nil {
		t.Fatal(err)
	}
	s.AD = bytes.Repeat([]byte{1}, 64) // a session the engine finished starting has AD
	raw, err := s.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.labels != chat2Labels {
		t.Fatalf("labels lost in the store: %+v", back.labels)
	}

	// A session written by v1: bytes produced by the code of v0.2.0 (commit 79aac72, before the
	// labels existed), not by the DTO of this build. It carries no labels and must read as v1.
	hexRec, err := os.ReadFile("testdata/session_v1_79aac72.hex")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := hex.DecodeString(strings.TrimSpace(string(hexRec)))
	if err != nil {
		t.Fatal(err)
	}
	old, err := FromBytes(rec)
	if err != nil {
		t.Fatalf("a session stored by v1 cannot be read: %v", err)
	}
	if old.labels != V1Labels {
		t.Fatalf("a session without labels is not v1: %+v", old.labels)
	}
	if !old.IsInitiator || old.RemotePeerID != (types.PeerID{1}) || old.RemoteRatchetPub != pub || !bytes.Equal(old.AD, bytes.Repeat([]byte{1}, 64)) {
		t.Fatalf("the stored v1 session was read wrongly: %+v", old)
	}
	if _, err := old.RatchetEncrypt([]byte("still works")); err != nil {
		t.Fatalf("the stored v1 session does not work: %v", err)
	}
}

func opened(t *testing.T, e *memguard.Enclave) []byte {
	t.Helper()
	l, err := e.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Destroy()
	return append([]byte(nil), l.Bytes()...)
}
