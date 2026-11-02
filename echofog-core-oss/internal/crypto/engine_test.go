package crypto_test

import (
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// source hands out bundles like the swarm does, and remembers which ones it was told to drop.
type source struct {
	bundles map[types.PeerID]*keymanager.PublicBundle
	forgot  []types.PeerID
}

func (s *source) GetBundle(id types.PeerID) (*keymanager.PublicBundle, error) {
	b, ok := s.bundles[id]
	if !ok {
		return nil, errors.New("no bundle")
	}
	c := *b
	return &c, nil
}

func (s *source) ForgetBundle(id types.PeerID) { s.forgot = append(s.forgot, id) }

type node struct {
	id     types.PeerID
	acc    *keymanager.AccountManager
	engine *crypto.CryptoEngine
}

func newNode(t *testing.T, src *source) node {
	t.Helper()
	st := testutil.NewTestingBadgerStorage(t)
	t.Cleanup(st.CloseAndClear)
	root := make([]byte, 64)
	_, _ = rand.Read(root)
	acc, err := keymanager.NewKeyManager(root, st.St).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	_, pub, err := acc.GetIdentityKey()
	if err != nil {
		t.Fatal(err)
	}
	id := types.PeerPubKeyToID(types.PeerPublicKey(pub))
	x, err := crypto.NewX3DHInitializer(acc, st.St, nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return node{id: id, acc: acc, engine: crypto.NewCryptoEngine(id, acc, x.GetEngine(), x.GetSessionManager(), src, log)}
}

func (n node) bundle(t *testing.T) *keymanager.PublicBundle {
	t.Helper()
	b, err := n.acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func stripped(b *keymanager.PublicBundle) *keymanager.PublicBundle {
	c := *b
	c.Version, c.KEMPreKey, c.KEMSignature = 0, nil, [64]byte{}
	return &c
}

// deliver hands an init from a to b, as the messenger does with a's card.
func deliver(t *testing.T, a, b node, msg types.EncryptedMessage, init *types.X3DHInit) {
	t.Helper()
	if init == nil {
		t.Fatal("a new session sends an init")
	}
	init.SecureMessage = msg
	plain, err := b.engine.HandleInitPacket(a.id, *init, false, a.bundle(t))
	if err != nil || string(plain) != "hi" {
		t.Fatalf("%q %v", plain, err)
	}
}

// Once Alice saw Bob's bundle with a KEM prekey, a copy of it (or an older one) without the KEM
// prekey is refused and dropped from the cache instead of starting a classic session. A newer
// classic bundle is a real downgrade of Bob's app and is taken.
func TestEngineRefusesABundleThatLostItsKEMPreKey(t *testing.T) {
	defer memguard.Purge()
	src := &source{bundles: map[types.PeerID]*keymanager.PublicBundle{}}
	alice, bob := newNode(t, src), newNode(t, src)
	pq := bob.bundle(t)
	src.bundles[bob.id] = pq

	msg, init, err := alice.engine.EncryptMessage(bob.id, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(init.KEMCiphertext) == 0 {
		t.Fatal("the first session with a PQ bundle is PQXDH")
	}
	deliver(t, alice, bob, msg, init)

	// the session goes; the next one is offered the same bundle stripped of its KEM prekey
	_ = alice.engine.DeleteSession(bob.id)
	src.bundles[bob.id] = stripped(pq)
	if _, _, err := alice.engine.EncryptMessage(bob.id, []byte("hi")); !errors.Is(err, crypto.ErrPQDowngrade) {
		t.Fatalf("want ErrPQDowngrade, got %v", err)
	}
	if len(src.forgot) != 1 || src.forgot[0] != bob.id {
		t.Fatal("the stripped bundle must be dropped from the cache")
	}
	older := stripped(pq)
	older.CreatedAt--
	src.bundles[bob.id] = older
	if _, _, err := alice.engine.EncryptMessage(bob.id, []byte("hi")); !errors.Is(err, crypto.ErrPQDowngrade) {
		t.Fatalf("an older classic bundle: want ErrPQDowngrade, got %v", err)
	}

	// a fresh PQ bundle works again
	src.bundles[bob.id] = bob.bundle(t)
	msg, init, err = alice.engine.EncryptMessage(bob.id, []byte("hi"))
	if err != nil || len(init.KEMCiphertext) == 0 {
		t.Fatalf("a fresh PQ bundle: %v", err)
	}
	deliver(t, alice, bob, msg, init)

	// Bob went back to an app without PQXDH: his newer bundles are classic, and taken
	_ = alice.engine.DeleteSession(bob.id)
	newer := stripped(bob.bundle(t))
	newer.CreatedAt += 10
	src.bundles[bob.id] = newer
	if _, init, err := alice.engine.EncryptMessage(bob.id, []byte("hi")); err != nil || len(init.KEMCiphertext) != 0 {
		t.Fatalf("a newer classic bundle: %v", err)
	}
}

// Bob learns from Alice's signed card that she is on PQXDH: a stripped bundle of hers is then
// refused when he starts a session himself.
func TestResponderLearnsTheInitiatorIsPQ(t *testing.T) {
	defer memguard.Purge()
	src := &source{bundles: map[types.PeerID]*keymanager.PublicBundle{}}
	alice, bob := newNode(t, src), newNode(t, src)
	src.bundles[bob.id] = bob.bundle(t)
	msg, init, err := alice.engine.EncryptMessage(bob.id, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	init.SecureMessage = msg
	card := alice.bundle(t)
	if _, err := bob.engine.HandleInitPacket(alice.id, *init, false, card); err != nil {
		t.Fatal(err)
	}
	_ = bob.engine.DeleteSession(alice.id)
	src.bundles[alice.id] = stripped(card)
	if _, _, err := bob.engine.EncryptMessage(alice.id, []byte("hi")); !errors.Is(err, crypto.ErrPQDowngrade) {
		t.Fatalf("want ErrPQDowngrade, got %v", err)
	}
}

// A peer that never offered a KEM prekey (v0.2.0) gets classic sessions as before.
func TestEngineClassicWithAPeerThatNeverHadPQ(t *testing.T) {
	defer memguard.Purge()
	src := &source{bundles: map[types.PeerID]*keymanager.PublicBundle{}}
	alice, bob := newNode(t, src), newNode(t, src)
	src.bundles[bob.id] = stripped(bob.bundle(t))
	if _, init, err := alice.engine.EncryptMessage(bob.id, []byte("hi")); err != nil || len(init.KEMCiphertext) != 0 {
		t.Fatalf("classic with a classic bundle: %v", err)
	}
	if len(src.forgot) != 0 {
		t.Fatal("nothing to drop")
	}
}
