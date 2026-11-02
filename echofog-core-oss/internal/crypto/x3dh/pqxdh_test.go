package x3dh

import (
	"crypto/mlkem"
	"errors"
	"strings"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// classicView is a bundle as v0.2.0 sees it, or as it comes out when somebody strips the KEM prekey:
// its classic signature still holds.
func classicView(b *keymanager.PublicBundle) *keymanager.PublicBundle {
	c := *b
	c.Version, c.KEMPreKey, c.KEMSignature = 0, nil, [64]byte{}
	return &c
}

// oldParty is a node of v0.2.0: a classic bundle, and no KEM prekey ever made.
func oldParty(t *testing.T) party {
	t.Helper()
	p := newParty(t, 1)
	for k := range p.st.data {
		if strings.HasPrefix(k, "kem:") {
			delete(p.st.data, k)
		}
	}
	p.bundle = classicView(p.bundle)
	if p.acc.KEMSince() != 0 {
		t.Fatal("an old node has no KEM prekey")
	}
	return p
}

// opens: b accepts a's init carrying "hello" (a's card is cardA).
func opens(t *testing.T, b party, init types.X3DHInit, cardA *keymanager.PublicBundle) *X3DHSession {
	t.Helper()
	sb, plain, err := b.engine.Accept(init, *cardA)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "hello" {
		t.Fatalf("got %q", plain)
	}
	return sb
}

func initTo(t *testing.T, a party, bundleB *keymanager.PublicBundle) (*X3DHSession, types.X3DHInit) {
	t.Helper()
	sa, init, err := a.engine.X3DHInit(*bundleB)
	if err != nil {
		t.Fatal(err)
	}
	init.SecureMessage = send(t, sa, "hello")
	return sa, init
}

func talk(t *testing.T, sa, sb *X3DHSession) {
	t.Helper()
	expect(t, sb, send(t, sa, "a1"), "a1")
	expect(t, sa, send(t, sb, "b1"), "b1")
	expect(t, sb, send(t, sa, "a2"), "a2")
}

func TestPQXDHRoundTrip(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, init := initTo(t, alice, bob.bundle)

	if len(init.KEMCiphertext) != mlkem.CiphertextSize768 || init.KEMKeyID == nil ||
		*init.KEMKeyID != keymanager.KEMKeyID(bob.bundle.KEMPreKey) {
		t.Fatal("the init must carry the KEM ciphertext for Bob's KEM prekey")
	}
	if init.BundleCreatedAt != bob.bundle.CreatedAt {
		t.Fatal("the init names the bundle it used")
	}
	sb := opens(t, bob, init, alice.bundle)
	if !sa.PQ || !sb.PQ {
		t.Fatal("both sessions are PQXDH")
	}
	talk(t, sa, sb)

	raw, err := sb.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromBytes(raw)
	if err != nil || !back.PQ {
		t.Fatalf("the PQ mark must survive storage: %v", err)
	}

	// the KEM prekey is a last-resort key: a second session with it works too (the one-time
	// prekey of the bundle is used up, so this one goes without)
	carol := newParty(t, 0)
	noOPK := *bob.bundle
	noOPK.OneTimeKeys = nil
	sc, init2 := initTo(t, carol, &noOPK)
	talk(t, sc, opens(t, bob, init2, carol.bundle))
}

// The KEM secret is part of the session key: a PQXDH secret differs from the classic one over the
// same X25519 keys.
func TestPQXDHSecretDependsOnTheKEM(t *testing.T) {
	defer memguard.Purge()
	ikA, _ := fixedKey(1)
	ekA, _ := fixedKey(2)
	_, ikBpub := fixedKey(3)
	_, spkBpub := fixedKey(4)
	p := X3DHParameters{MyIdentityPriv: ikA, MyEphemeralPriv: ekA, RemoteIdentityPub: ikBpub, RemoteSignedPrePub: spkBpub}
	classic, err := CalculateX3DH(p)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{openHex(t, classic): true}
	for _, kem := range []*KEMInput{
		{SharedSecret: make([]byte, 32), Ciphertext: []byte{1}},
		{SharedSecret: append(make([]byte, 31), 1), Ciphertext: []byte{1}},
		{SharedSecret: make([]byte, 32), Ciphertext: []byte{2}},
		{SharedSecret: make([]byte, 32), KeyID: [32]byte{1}, Ciphertext: []byte{1}},
	} {
		p.KEM = kem
		k, err := CalculateX3DH(p)
		if err != nil {
			t.Fatal(err)
		}
		h := openHex(t, k)
		if seen[h] {
			t.Fatal("the KEM secret, key id and ciphertext must all change the session key")
		}
		seen[h] = true
	}
	p.KEM = &KEMInput{SharedSecret: make([]byte, 16)}
	if _, err := CalculateX3DH(p); !errors.Is(err, ErrBadKEMInit) {
		t.Fatalf("a KEM secret of the wrong size: %v", err)
	}
}

// A new node and a node of v0.2.0 still talk, classic, in both directions.
func TestClassicWithV020Peer(t *testing.T) {
	defer memguard.Purge()
	alice, old := newParty(t, 1), oldParty(t)

	// new -> old: the old bundle has no KEM prekey, so the init is classic
	sa, init := initTo(t, alice, old.bundle)
	if len(init.KEMCiphertext) != 0 || init.KEMKeyID != nil {
		t.Fatal("no KEM part for a bundle without a KEM prekey")
	}
	sb := opens(t, old, init, alice.bundle)
	if sa.PQ || sb.PQ {
		t.Fatal("a session with v0.2.0 is classic")
	}
	talk(t, sa, sb)

	// old -> new: v0.2.0 reads only the classic part of the new bundle and sends an init without
	// the KEM fields and without the bundle time; the new node accepts it as classic
	bob := newParty(t, 1)
	so, init2 := initTo(t, old, classicView(bob.bundle))
	init2.BundleCreatedAt = 0
	sbob := opens(t, bob, init2, old.bundle)
	if so.PQ || sbob.PQ {
		t.Fatal("a session from v0.2.0 is classic")
	}
	talk(t, so, sbob)
}

// Somebody strips the KEM prekey from Bob's bundle (its classic signature still holds). Alice then
// starts classic X3DH, but her init names the bundle, and Bob signed every bundle since his first
// KEM prekey with one: he refuses. A bundle older than his first KEM prekey is still fine.
func TestStrippedKEMPreKeyIsDetected(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	stripped := classicView(bob.bundle)
	if err := keymanager.VerifyBundle(stripped); err != nil {
		t.Fatal(err)
	}
	_, init := initTo(t, alice, stripped)
	if init.KEMCiphertext != nil {
		t.Fatal("no KEM prekey, no KEM part")
	}
	if _, _, err := bob.engine.Accept(init, *alice.bundle); !errors.Is(err, ErrPQDowngrade) {
		t.Fatalf("want ErrPQDowngrade, got %v", err)
	}

	// a bundle Bob signed before he had a KEM prekey (an upgrade since): classic is right
	_, init = initTo(t, alice, stripped)
	init.BundleCreatedAt = bob.acc.KEMSince() - 1
	opens(t, bob, init, alice.bundle)
}

func TestTamperedKEMInitIsRejected(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 0), newParty(t, 0)
	for name, c := range map[string]struct {
		tamper func(*types.X3DHInit)
		want   error
	}{
		"ciphertext byte": {func(i *types.X3DHInit) { i.KEMCiphertext[500] ^= 1 }, ErrInitRejected},
		"ciphertext cut":  {func(i *types.X3DHInit) { i.KEMCiphertext = i.KEMCiphertext[:1000] }, ErrBadKEMInit},
		"key id":          {func(i *types.X3DHInit) { i.KEMKeyID[0] ^= 1 }, ErrKEMKeyNotFound},
		"ciphertext gone": {func(i *types.X3DHInit) { i.KEMCiphertext = nil }, ErrBadKEMInit},
		"key id gone":     {func(i *types.X3DHInit) { i.KEMKeyID = nil }, ErrBadKEMInit},
		"KEM part gone": {func(i *types.X3DHInit) {
			i.KEMCiphertext, i.KEMKeyID = nil, nil
		}, ErrPQDowngrade},
		// made to look like an init of v0.2.0: the classic key is not the PQXDH one
		"KEM part and bundle time gone": {func(i *types.X3DHInit) {
			i.KEMCiphertext, i.KEMKeyID, i.BundleCreatedAt = nil, nil, 0
		}, ErrInitRejected},
		"ciphertext of another session": {func(i *types.X3DHInit) {
			_, other := initTo(t, newParty(t, 0), bob.bundle)
			i.KEMCiphertext = other.KEMCiphertext
		}, ErrInitRejected},
	} {
		_, init := initTo(t, alice, bob.bundle)
		id := *init.KEMKeyID
		init.KEMKeyID = &id
		init.KEMCiphertext = append([]byte(nil), init.KEMCiphertext...)
		c.tamper(&init)
		if _, _, err := bob.engine.Accept(init, *alice.bundle); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	// and the untouched init still opens
	_, init := initTo(t, alice, bob.bundle)
	opens(t, bob, init, alice.bundle)
}

// A bundle whose KEM prekey is no ML-KEM-768 key cannot start a session (VerifyBundle refuses it
// before; this is the engine's own check).
func TestBundleWithBrokenKEMKeyStartsNothing(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 0), newParty(t, 0)
	bad := *bob.bundle
	bad.KEMPreKey = make([]byte, 10)
	if _, _, err := alice.engine.X3DHInit(bad); !errors.Is(err, ErrBadKEMKey) {
		t.Fatalf("want ErrBadKEMKey, got %v", err)
	}
}
