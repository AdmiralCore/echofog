package x3dh

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type MockStorage struct {
	data map[string][]byte
}

func (m *MockStorage) FindValuesSeek(prefix []byte, startKey []byte, limit int) ([]storage.Entry, error) {
	return nil, nil
}

func (m *MockStorage) Close() {}

func NewMockStorage() *MockStorage {
	return &MockStorage{
		data: make(map[string][]byte),
	}
}

func (m *MockStorage) Set(key, value []byte) error {
	m.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (m *MockStorage) Get(key []byte) ([]byte, error) {
	if val, ok := m.data[string(key)]; ok {
		return append([]byte(nil), val...), nil
	}
	return nil, errors.New("key not found in mock storage")
}

func (m *MockStorage) Delete(key []byte) error {
	delete(m.data, string(key))
	return nil
}

func (m *MockStorage) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	var out []storage.Entry
	for k, v := range m.data {
		if bytes.HasPrefix([]byte(k), prefix) {
			out = append(out, storage.Entry{Key: []byte(k), Value: append([]byte(nil), v...)})
		}
	}
	return out, nil
}

func (m *MockStorage) Exists(key []byte) bool {
	_, ok := m.data[string(key)]
	return ok
}

type party struct {
	acc    *keymanager.AccountManager
	st     *MockStorage
	bundle *keymanager.PublicBundle // published, with one-time prekeys
	engine *X3DHEngine
}

func newParty(t *testing.T, opks int) party {
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
	if err := keymanager.VerifyBundle(published); err != nil {
		t.Fatal(err)
	}
	return party{acc: acc, st: st, bundle: published, engine: NewX3DHEngine(own, st, acc)}
}

// start: a opens a session with b and b accepts it with the first message.
func start(t *testing.T, a, b party) (*X3DHSession, *X3DHSession) {
	t.Helper()
	sa, init, err := a.engine.X3DHInit(*b.bundle)
	if err != nil {
		t.Fatal(err)
	}
	init.SecureMessage, err = sa.RatchetEncrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	sb, plain, err := b.engine.Accept(init, *a.bundle)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "hello" {
		t.Fatalf("got %q", plain)
	}
	return sa, sb
}

func send(t *testing.T, s *X3DHSession, text string) types.EncryptedMessage {
	t.Helper()
	m, err := s.RatchetEncrypt([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func expect(t *testing.T, s *X3DHSession, m types.EncryptedMessage, text string) {
	t.Helper()
	got, err := s.RatchetDecrypt(m)
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	if string(got) != text {
		t.Fatalf("got %q, want %q", got, text)
	}
}

func TestX3DHEncryption(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 3), newParty(t, 3)
	sa, sb := start(t, alice, bob)

	expect(t, sb, send(t, sa, "Hello from EchoFog"), "Hello from EchoFog")
	expect(t, sa, send(t, sb, "Hi!"), "Hi!")

	m1, m2 := send(t, sb, "What you do?"), send(t, sb, "I downloaded EchoFog")
	expect(t, sa, m2, "I downloaded EchoFog")
	expect(t, sa, m1, "What you do?")
}

func TestSessionPersistence(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, sb := start(t, alice, bob)

	key, err := alice.acc.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	sm := NewX3DHSessionManager(alice.st, key)
	m := send(t, sb, "Hello!")
	if err := sm.Save(sa.RemotePeerID, sa); err != nil {
		t.Fatal(err)
	}
	// nothing secret is readable in the database
	raw, _ := alice.st.Get(sm.sessionKey(sa.RemotePeerID))
	if bytes.Contains(raw, sa.AD) {
		t.Fatal("the stored session is not sealed")
	}
	loaded, err := sm.Load(sa.RemotePeerID)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, loaded, m, "Hello!")

	// another account's key cannot open it
	otherKey, _ := bob.acc.StorageKey()
	if _, err := NewX3DHSessionManager(alice.st, otherKey).Load(sa.RemotePeerID); err == nil {
		t.Fatal("a session opened with a foreign key")
	}
}

// Mallory starts a session with Bob with her own keys while claiming to be Alice: Bob takes the
// identity from Alice's signed bundle and refuses.
func TestInitFromAnotherIdentityIsRejected(t *testing.T) {
	defer memguard.Purge()
	alice, bob, mallory := newParty(t, 1), newParty(t, 2), newParty(t, 1)

	sm, init, err := mallory.engine.X3DHInit(*bob.bundle)
	if err != nil {
		t.Fatal(err)
	}
	init.SecureMessage = send(t, sm, "hi, it's Alice")
	if _, _, err := bob.engine.Accept(init, *alice.bundle); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("want ErrIdentityMismatch, got %v", err)
	}

	// claiming Alice's identity key in the packet does not help: Mallory lacks Alice's private key
	init.IdentityKey = alice.bundle.IdentityKey
	if _, _, err := bob.engine.Accept(init, *alice.bundle); err == nil {
		t.Fatal("Bob accepted a session from somebody without Alice's key")
	}
}

func TestOneTimePrekeyIsUsedOnce(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)

	sa, init, err := alice.engine.X3DHInit(*bob.bundle)
	if err != nil {
		t.Fatal(err)
	}
	if init.OneTimeKeyID == nil {
		t.Fatal("the bundle offered a one-time prekey")
	}
	init.SecureMessage = send(t, sa, "first")
	if _, _, err := bob.engine.Accept(init, *alice.bundle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bob.engine.Accept(init, *alice.bundle); !errors.Is(err, ErrOPKNotFound) {
		t.Fatalf("a replayed init was accepted again: %v", err)
	}
}

// A repeat of the init packet (its receipt was lost) can still be read with the session it made.
func TestInitMessageCanBeReadAgain(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, init, err := alice.engine.X3DHInit(*bob.bundle)
	if err != nil {
		t.Fatal(err)
	}
	init.SecureMessage = send(t, sa, "first")
	sb, _, err := bob.engine.Accept(init, *alice.bundle)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sb.ReadInitAgain(init.SecureMessage); err != nil || string(got) != "first" {
		t.Fatalf("%q %v", got, err)
	}
	// after the first ratchet step the key is gone
	expect(t, sa, send(t, sb, "reply"), "reply")
	expect(t, sb, send(t, sa, "next"), "next")
	if _, err := sb.ReadInitAgain(init.SecureMessage); err == nil {
		t.Fatal("the init message key outlived the first ratchet step")
	}
}

func TestTamperedHeaderIsRejectedAndChangesNothing(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, sb := start(t, alice, bob)

	m := send(t, sa, "real")
	for _, tamper := range []func(*types.EncryptedMessage){
		func(m *types.EncryptedMessage) { m.Header.PN = 7 },
		func(m *types.EncryptedMessage) { m.Header.N = 5 },
		func(m *types.EncryptedMessage) { m.Header.DH[0] ^= 1 },
		func(m *types.EncryptedMessage) { m.Ciphertext[len(m.Ciphertext)-1] ^= 1 },
	} {
		bad := m
		bad.Ciphertext = append([]byte(nil), m.Ciphertext...)
		tamper(&bad)
		if _, err := sb.RatchetDecrypt(bad); err == nil {
			t.Fatal("a tampered message was accepted")
		}
	}
	if len(sb.SkippedKeys) != 0 {
		t.Fatal("a rejected message left skipped keys behind")
	}
	expect(t, sb, m, "real")
	if _, err := sb.RatchetDecrypt(m); err == nil {
		t.Fatal("the same message was accepted twice")
	}
}

// Messages lost before a ratchet step arrive later and are read: PN counts the sender's previous
// sending chain.
func TestLateMessagesAcrossARatchetStep(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, sb := start(t, alice, bob)

	a1, a2 := send(t, sa, "a1"), send(t, sa, "a2")
	expect(t, sa, send(t, sb, "b0"), "b0")
	a3 := send(t, sa, "a3") // new chain
	expect(t, sb, a3, "a3")
	expect(t, sb, a2, "a2")
	expect(t, sb, a1, "a1")
}

// One side sending far more than MaxSkip messages in a row does not break the session.
func TestLongMonologue(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, sb := start(t, alice, bob)
	expect(t, sa, send(t, sb, "b"), "b")
	for i := 0; i < MaxSkip+200; i++ {
		expect(t, sb, send(t, sa, fmt.Sprint(i)), fmt.Sprint(i))
	}
	expect(t, sa, send(t, sb, "still here"), "still here")
	expect(t, sb, send(t, sa, "me too"), "me too")
}

func TestSkippedKeysAreBounded(t *testing.T) {
	defer memguard.Purge()
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, sb := start(t, alice, bob)

	for step := 0; step < MaxSkippedChains+3; step++ {
		var last types.EncryptedMessage
		for i := 0; i < MaxSkip; i++ {
			last = send(t, sa, "x")
		}
		expect(t, sb, last, "x") // skips MaxSkip-1 keys
		expect(t, sa, send(t, sb, "ok"), "ok")
	}
	total := 0
	for _, k := range sb.SkippedKeys {
		total += len(k)
	}
	if total > MaxSkippedTotal || len(sb.SkippedKeys) > MaxSkippedChains {
		t.Fatalf("%d skipped keys in %d chains", total, len(sb.SkippedKeys))
	}

	// a gap above MaxSkip is refused
	for i := 0; i <= MaxSkip+1; i++ {
		send(t, sa, "y")
	}
	if _, err := sb.RatchetDecrypt(send(t, sa, "far")); !errors.Is(err, ErrTooManySkipped) {
		t.Fatalf("want ErrTooManySkipped, got %v", err)
	}
}

func TestStoredSessionOfOldFormatIsDropped(t *testing.T) {
	alice, bob := newParty(t, 1), newParty(t, 1)
	sa, _ := start(t, alice, bob)
	sa.AD = nil
	raw, err := sa.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromBytes(raw); !errors.Is(err, ErrSessionFormat) {
		t.Fatalf("want ErrSessionFormat, got %v", err)
	}
}
