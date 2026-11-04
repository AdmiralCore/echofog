package identity_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyler-smith/go-bip39"

	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// The standard BIP-39 test phrase (Trezor vectors).
const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// Reference values produced by the v1 key manager (internal/crypto/keymanager, account 0) for the
// same phrase. They pin the "account key equals v1 identity key" rule (README §5.1, AD7).
const (
	v1IdentityPub = "060dafcd66c9be97006fc99e1fc15cfaedcd3c13e396f900be21ca5214606481"
	v1NodeID      = "c554083453a87b0aa0f7eb40ea7fa6be8dc4d3c5752ace7daae74832fdcc32a9"
	v1StorageKey  = "5cdbddc40a200d25c23e17cb288d46da838fa6ff3ed5c97bf3147164c8d8365d"
)

func mustAccount(t *testing.T) *identity.Account {
	t.Helper()
	a, err := identity.FromMnemonic(phrase, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Destroy)
	return a
}

func mustDevice(t *testing.T) *identity.Device {
	t.Helper()
	d, err := identity.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Destroy)
	return d
}

func hexOf(t *testing.T, b []byte) string {
	t.Helper()
	return hex.EncodeToString(b)
}

func TestAccountKeyEqualsV1(t *testing.T) {
	// The reference is v1's own derivation, not a copy of it.
	km, err := keymanager.NewKeyManagerWithMnemonic([]byte(phrase), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := km.GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := acc.GetIdentityKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = priv

	a := mustAccount(t)
	if a.Public().Alg() != keys.Ed25519 {
		t.Fatalf("account key tag %s", a.Public().Alg())
	}
	if !bytes.Equal(a.Public().Key(), pub) {
		t.Fatalf("account key %x, v1 %x", a.Public().Key(), pub)
	}

	// The same values are pinned as constants so a change in v1 cannot hide a change here.
	if got := hexOf(t, a.Public()); got != "01"+v1IdentityPub {
		t.Fatalf("account akey %s", got)
	}
	nodeID := a.NodeID()
	if got := hexOf(t, nodeID[:]); got != v1NodeID {
		t.Fatalf("node id %s", got)
	}
}

func TestNodeIDIsHashOfTaggedKey(t *testing.T) {
	a := mustAccount(t)
	want := sha256.Sum256(a.Public())
	if a.NodeID() != want {
		t.Fatalf("node id %x, want %x", a.NodeID(), want)
	}
	if a.Public()[0] != byte(keys.Ed25519) {
		t.Fatalf("akey does not start with the ed25519 tag: %x", a.Public()[0])
	}
}

func TestRestoreKeepsNodeIDAndMakesNewDevice(t *testing.T) {
	a1, err := identity.FromMnemonic(phrase, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a1.Destroy()
	a2, err := identity.FromMnemonic(phrase, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Destroy()
	if a1.NodeID() != a2.NodeID() {
		t.Fatal("restoring the same phrase changed the node ID")
	}

	d1, err := identity.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Destroy()
	d2, err := identity.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Destroy()
	if d1.ID() == d2.ID() || d1.Public().Equal(d2.Public()) {
		t.Fatal("two restores produced the same device")
	}
	if d1.Public().Equal(a1.Public()) {
		t.Fatal("the device key equals the account key (K1)")
	}

	a3, err := identity.FromMnemonic(phrase, "another passphrase")
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Destroy()
	if a3.NodeID() == a1.NodeID() {
		t.Fatal("the BIP-39 passphrase was ignored")
	}
}

func TestDeviceKeysAreRandom(t *testing.T) {
	d1 := mustDevice(t)
	d2 := mustDevice(t)
	if d1.ID() == d2.ID() {
		t.Fatal("device IDs repeat")
	}
	if d1.Public().Equal(d2.Public()) {
		t.Fatal("device keys repeat")
	}
	if d1.Handshake().Public().Equal(d2.Handshake().Public()) {
		t.Fatal("handshake keys repeat")
	}
	if d1.Masking().Public().Equal(d2.Masking().Public()) {
		t.Fatal("masking keys repeat")
	}
	if d1.Handshake().Public().Equal(d1.Masking().Public()) {
		t.Fatal("the handshake and masking keys are the same key")
	}
	if d1.Handshake().Public().Alg() != keys.X25519 || d1.Masking().Public().Alg() != keys.X25519 {
		t.Fatal("handshake/masking keys are not x25519")
	}
	if d1.Handshake().Public().Equal(d1.Public()) {
		t.Fatal("a masking key equals the device signing key")
	}
}

func TestSignWithAccountAndDevice(t *testing.T) {
	a := mustAccount(t)
	msg := []byte("device list")
	sig, err := a.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Verify(a.Public(), msg, sig); err != nil {
		t.Fatalf("account signature: %v", err)
	}

	d := mustDevice(t)
	dsig, err := d.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Verify(d.Public(), msg, dsig); err != nil {
		t.Fatalf("device signature: %v", err)
	}
	if err := keys.Verify(a.Public(), msg, dsig); err == nil {
		t.Fatal("a device signature verifies with the account key")
	}
}

func TestSealDeviceList(t *testing.T) {
	a := mustAccount(t)
	d := mustDevice(t)
	now := time.Unix(1_700_000_000, 0)
	notAfter := uint64(now.Add(identity.DeviceListLifetime).Unix())

	list, err := a.SealDeviceList(7, notAfter, []identity.DeviceEntry{
		{Key: d.Public(), Added: now, Primary: true, Label: "phone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := objects.VerifyDeviceList(list, now)
	if err != nil {
		t.Fatal(err)
	}
	if dl.NodeID != a.NodeID() {
		t.Fatal("the list is not signed by the account key")
	}
	if len(dl.Devices()) != 1 || !dl.Contains(d.Public()) {
		t.Fatalf("devices %v", dl.Devices())
	}
	if dl.Devices()[0].GetFlags()&objects.DevicePrimary == 0 {
		t.Fatal("the only device is not primary")
	}
	if dl.Devices()[0].GetLabel() != "phone" {
		t.Fatalf("label %q", dl.Devices()[0].GetLabel())
	}

	single, err := a.SealSingleDeviceList(d, 8, now)
	if err != nil {
		t.Fatal(err)
	}
	dl2, err := objects.VerifyDeviceList(single, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(dl2.Devices()) != 1 || !dl2.Contains(d.Public()) ||
		dl2.Devices()[0].GetFlags()&objects.DevicePrimary == 0 {
		t.Fatal("the single-device list is wrong")
	}

	// A device list names device signing keys; an X25519 key is refused by the object rules.
	if _, err := a.SealDeviceList(9, notAfter, []identity.DeviceEntry{{Key: d.Handshake().Public()}}); err == nil {
		t.Fatal("an x25519 key was accepted as a device key")
	}
}

func TestExitIdentity(t *testing.T) {
	main := mustAccount(t)
	e, err := identity.NewExitIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Destroy()
	if e.Account.NodeID() == main.NodeID() {
		t.Fatal("the exit identity reuses the messenger account")
	}
	if e.Device.Public().Equal(main.Public()) {
		t.Fatal("the exit device reuses the account key")
	}

	now := time.Unix(1_700_000_000, 0)
	list, err := e.DeviceList(1, now)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := objects.VerifyDeviceList(list, now)
	if err != nil {
		t.Fatal(err)
	}
	if dl.NodeID != e.Account.NodeID() {
		t.Fatal("the exit device list is signed by the wrong key")
	}
	if len(dl.Devices()) != 1 || !dl.Contains(e.Device.Public()) {
		t.Fatal("the exit identity must list exactly one device")
	}

	e2, err := identity.NewExitIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Destroy()
	if e2.Account.NodeID() == e.Account.NodeID() || e2.Device.ID() == e.Device.ID() {
		t.Fatal("two exit identities share a key")
	}
}

func TestThrowawayIdentity(t *testing.T) {
	main := mustAccount(t)
	a, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Destroy()
	b, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Destroy()
	if a.Account.NodeID() == b.Account.NodeID() || a.Device.ID() == b.Device.ID() {
		t.Fatal("two throwaway identities share a key")
	}
	if a.Account.NodeID() == main.NodeID() || a.Device.Public().Equal(main.Public()) {
		t.Fatal("a throwaway identity is tied to the messenger identity")
	}
	if a.Device.Handshake().Public().Equal(main.Public()) {
		t.Fatal("the throwaway reused the real handshake key")
	}

	now := time.Unix(1_700_000_000, 0)
	list, err := a.DeviceList(1, now)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := objects.VerifyDeviceList(list, now)
	if err != nil {
		t.Fatal(err)
	}
	if dl.NodeID != a.Account.NodeID() || len(dl.Devices()) != 1 || !dl.Contains(a.Device.Public()) {
		t.Fatal("the throwaway device list is wrong")
	}
}

func TestStorageKeyEqualsV1(t *testing.T) {
	km, err := keymanager.NewKeyManagerWithMnemonic([]byte(phrase), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := km.GetAccountWithOptions(0, keymanager.Options{StorageKeyInfo: identity.StorageKeyLabel})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := acc.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	buf, err := enc.Open()
	if err != nil {
		t.Fatal(err)
	}
	v1Key := append([]byte(nil), buf.Bytes()...)
	buf.Destroy()

	a := mustAccount(t)
	var got []byte
	if err := a.StorageKey().Use(func(b []byte) error {
		got = append([]byte(nil), b...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, v1Key) {
		t.Fatalf("storage key %x, v1 %x", got, v1Key)
	}
	if hexOf(t, got) != v1StorageKey {
		t.Fatalf("storage key %s", hexOf(t, got))
	}
}

type fakeProtector struct{}

func (fakeProtector) Protect(secret, entropy []byte) ([]byte, error) {
	mac := hmac.New(sha256.New, entropy)
	mac.Write(secret)
	return append(mac.Sum(nil), secret...), nil
}

func (fakeProtector) Unprotect(blob, entropy []byte) ([]byte, error) {
	if len(blob) < sha256.Size {
		return nil, errors.New("blob is too short")
	}
	mac := hmac.New(sha256.New, entropy)
	mac.Write(blob[sha256.Size:])
	if !hmac.Equal(mac.Sum(nil), blob[:sha256.Size]) {
		return nil, errors.New("wrong entropy")
	}
	return append([]byte(nil), blob[sha256.Size:]...), nil
}

func TestWrapStorageKey(t *testing.T) {
	a := mustAccount(t)
	entropy := []byte("profile-1")

	blob, err := a.WrapStorageKey(fakeProtector{}, entropy)
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.UnwrapStorageKey(fakeProtector{}, blob, entropy)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Destroy()
	var got, want []byte
	if err := key.Use(func(b []byte) error { got = append([]byte(nil), b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.StorageKey().Use(func(b []byte) error { want = append([]byte(nil), b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the unwrapped key differs from the account's storage key")
	}

	if _, err := identity.UnwrapStorageKey(fakeProtector{}, blob, []byte("profile-2")); err == nil {
		t.Fatal("a blob opened with the wrong entropy")
	}
	if _, err := a.WrapStorageKey(nil, entropy); !errors.Is(err, identity.ErrNoProtector) {
		t.Fatalf("nil protector: %v", err)
	}
	if _, err := identity.UnwrapStorageKey(nil, blob, entropy); !errors.Is(err, identity.ErrNoProtector) {
		t.Fatalf("nil protector: %v", err)
	}
}

func TestSystemProtector(t *testing.T) {
	p := identity.SystemProtector()
	if p == nil {
		t.Skip("no operating-system protector on this platform")
	}
	a := mustAccount(t)
	blob, err := a.WrapStorageKey(p, []byte("profile"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.UnwrapStorageKey(p, blob, []byte("profile"))
	if err != nil {
		t.Fatal(err)
	}
	key.Destroy()
	if _, err := identity.UnwrapStorageKey(p, blob, []byte("other profile")); err == nil {
		t.Fatal("DPAPI opened a blob sealed for another entropy")
	}
}

func TestMnemonic(t *testing.T) {
	for _, words := range []int{12, 24} {
		phrase, err := identity.NewMnemonic(words)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(strings.Fields(phrase)); n != words {
			t.Fatalf("%d words, want %d", n, words)
		}
		if !bip39.IsMnemonicValid(phrase) {
			t.Fatalf("generated phrase is not valid BIP-39: %q", phrase)
		}
	}
	if _, err := identity.NewMnemonic(15); err == nil {
		t.Fatal("15 words accepted")
	}

	for name, bad := range map[string]string{
		"eleven words": "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		"unknown word": "zebra abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		"checksum":     strings.Repeat("abandon ", 11) + "abandon",
	} {
		if _, err := identity.FromMnemonic(bad, ""); !errors.Is(err, identity.ErrPhrase) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A phrase typed with capitals and extra spaces is the same phrase.
	typed := "  ABANDON abandon\nabandon abandon abandon abandon abandon abandon abandon abandon abandon   About "
	a, err := identity.FromMnemonic(typed, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Destroy()
	want := mustAccount(t)
	if a.NodeID() != want.NodeID() {
		t.Fatal("normalising the phrase changed the identity")
	}
}

func TestKeyPairFromPrivate(t *testing.T) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		t.Fatal(err)
	}
	kp, err := identity.KeyPairFromPrivate(priv)
	if err != nil {
		t.Fatal(err)
	}
	defer kp.Destroy()

	ecdhKey, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kp.Public().Key(), ecdhKey.PublicKey().Bytes()) {
		t.Fatal("the public key does not match the private key")
	}
	if err := kp.WithPrivate(func(b []byte) error {
		if !bytes.Equal(b, priv) {
			t.Fatal("private bytes changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.KeyPairFromPrivate(priv[:31]); err == nil {
		t.Fatal("a short private key was accepted")
	}
}

func TestDestroyedIdentity(t *testing.T) {
	a, err := identity.FromMnemonic(phrase, "")
	if err != nil {
		t.Fatal(err)
	}
	a.Destroy()
	if _, err := a.Sign([]byte("x")); !errors.Is(err, secret.ErrDestroyed) {
		t.Fatalf("signing after destroy: %v", err)
	}

	d, err := identity.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	d.Destroy()
	if _, err := d.Sign([]byte("x")); !errors.Is(err, secret.ErrDestroyed) {
		t.Fatalf("device signing after destroy: %v", err)
	}
}
