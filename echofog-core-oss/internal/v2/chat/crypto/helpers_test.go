package chatcrypto

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// testClock is the injected node clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: time.Now().Truncate(time.Second)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// user is one account with one device, a store and a Crypto.
type user struct {
	t     *testing.T
	clock *testClock
	tw    *identity.Throwaway
	key   *secret.Value
	dir   string
	st    *store.Store
	ids   *store.IDs
	c     *Crypto
	dl    *objects.DeviceList
}

func newUser(t *testing.T, clock *testClock) *user {
	t.Helper()
	// memguard locks pages and the quota of a Windows process is small: the buffers leaked by one
	// test are released after it (the v1 tests do the same), or a later test hangs in Purge.
	t.Cleanup(memguard.Purge)
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	raw, err := tw.DeviceList(1, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	dl, err := objects.VerifyDeviceList(raw, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	u := &user{t: t, clock: clock, tw: tw, key: secret.New(k), dir: t.TempDir(), dl: dl}
	u.open()
	t.Cleanup(func() { u.close(); u.key.Destroy() })
	return u
}

// open opens the store and the Crypto; a second call after close reopens them (a restart).
func (u *user) open() {
	u.t.Helper()
	st, err := store.Open(store.Options{Dir: filepath.Join(u.dir, "v2"), Key: u.key})
	if err != nil {
		u.t.Fatal(err)
	}
	ids, err := store.NewIDs(u.key)
	if err != nil {
		u.t.Fatal(err)
	}
	c, err := New(Config{Device: u.tw.Device, Store: st.Store, IDs: ids, Now: u.clock.Now})
	if err != nil {
		u.t.Fatalf("New: %v", err)
	}
	u.st, u.ids, u.c = st, ids, c
}

func (u *user) close() {
	if u.c != nil {
		u.c.Close()
		u.c = nil
	}
	if u.ids != nil {
		u.ids.Close()
		u.ids = nil
	}
	if u.st != nil {
		_ = u.st.Close()
		u.st = nil
	}
}

func (u *user) restart() { u.close(); u.open() }

func (u *user) device() [32]byte { return u.tw.Device.ID() }
func (u *user) node() [32]byte   { return u.tw.Account.NodeID() }

// bundle returns the user's Bundle() verified as a peer would.
func (u *user) bundle() *objects.Prekeys {
	u.t.Helper()
	raw, err := u.c.Bundle()
	if err != nil {
		u.t.Fatalf("Bundle: %v", err)
	}
	p, err := objects.VerifyPrekeys(raw, u.clock.Now())
	if err != nil {
		u.t.Fatalf("a bundle does not verify: %v", err)
	}
	return p
}

// pair makes a and b and a session a -> b with the first message "hello"; it returns b's plaintext.
func pair(t *testing.T) (a, b *user, init []byte, clock *testClock) {
	t.Helper()
	clock = newClock()
	a, b = newUser(t, clock), newUser(t, clock)
	var err error
	init, err = a.c.Start(b.bundle(), b.dl, []byte("hello"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return a, b, init, clock
}

func accept(t *testing.T, to, from *user, init []byte) ([]byte, error) {
	t.Helper()
	raw, err := from.c.Prekeys()
	if err != nil {
		t.Fatal(err)
	}
	p, err := objects.VerifyPrekeys(raw, from.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return to.c.Accept(p, from.dl, init)
}

// ---- helpers for hand-made senders ------------------------------------------------------------

// memStorage is a storage.Storage in memory for the raw v1 engine of a hand-made sender.
type memStorage struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMem() *memStorage { return &memStorage{m: map[string][]byte{}} }

func (s *memStorage) Set(k, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(k)] = append([]byte(nil), v...)
	return nil
}

func (s *memStorage) Get(k []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[string(k)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), v...), nil
}

func (s *memStorage) Delete(k []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(k))
	return nil
}

func (s *memStorage) FindValues(prefix []byte, limit int) ([]storage.Entry, error) { return nil, nil }
func (s *memStorage) FindValuesSeek(prefix, start []byte, limit int) ([]storage.Entry, error) {
	return nil, nil
}
func (s *memStorage) Exists(k []byte) bool { _, err := s.Get(k); return err == nil }
func (s *memStorage) Close()               {}

func publicBundle(p *objects.Prekeys) keymanager.PublicBundle {
	var b keymanager.PublicBundle
	b.PeerID = p.DeviceID
	ik, _ := keys.ParseKind(p.Body.GetIk(), keys.X25519)
	copy(b.IdentityKey[:], ik.Key())
	copy(b.SignedPreKey[:], p.Body.GetSpk())
	for _, o := range p.OPKs() {
		var k [32]byte
		copy(k[:], o)
		b.OneTimeKeys = append(b.OneTimeKeys, k)
	}
	b.KEMPreKey = append([]byte(nil), p.Body.GetKem()...)
	b.CreatedAt = int64(p.Env.Seq)
	return b
}

func hexBytes(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// vector reads one object of docs/protocol/v2/vectors/kdf.json.
func vector(t testing.TB, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("../../../../../docs/protocol/v2/vectors/kdf.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(all[name], into); err != nil {
		t.Fatal(err)
	}
}

func repeat(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }
