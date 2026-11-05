package access_test

import (
	"crypto/ecdh"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// testNow is the fixed instant every test starts from.
var testNow = time.Unix(1_800_000_000, 0)

// testPhrase is a valid BIP-39 phrase; the account key is irrelevant to access rules.
const testPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// fakeClock is the injected clock (AD13).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: testNow} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// node is one test identity with a card builder. The server node of a rig is built separately so
// that its store can be reopened with the same keys.
type node struct {
	account *identity.Account
	device  *identity.Device
	devices []byte
	builder *cards.CardBuilder
	clock   *fakeClock
}

func newNode(t *testing.T, clk *fakeClock, vis cards.Visibility, suite uint32, host string, port uint32, mask []byte) *node {
	t.Helper()

	account, err := identity.FromMnemonic(testPhrase, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	device, err := identity.NewDevice()
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	devices, err := account.SealSingleDeviceList(device, 1, clk.Now())
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	t.Cleanup(func() {
		account.Destroy()
		device.Destroy()
	})
	return &node{
		account: account, device: device, devices: devices, clock: clk,
		builder: buildCards(t, account, device, devices, clk, vis, suite, host, port, mask),
	}
}

func buildCards(t *testing.T, account *identity.Account, device *identity.Device, devices []byte, clk *fakeClock, vis cards.Visibility, suite uint32, host string, port uint32, mask []byte) *cards.CardBuilder {
	t.Helper()
	l := cards.Listener{Suite: suite, Host: host, Port: port, Mask: mask}
	if suite == objects.SuiteReality2QUIC {
		l.SNI = "example.com"
	}
	b, err := cards.New(cards.Config{
		Account:    account.Public(),
		Device:     device,
		HsKey:      device.Handshake().Public(),
		Role:       objects.RoleUser,
		Listeners:  []cards.Listener{l},
		Devices:    devices,
		Release:    "0.3.0",
		Visibility: vis,
		Clock:      clk,
	})
	if err != nil {
		t.Fatalf("card builder: %v", err)
	}
	return b
}

func (n *node) peerCard(t *testing.T) []byte {
	t.Helper()
	c, err := n.builder.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("peer card: %v", err)
	}
	return c.Envelope
}

func (n *node) static(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	var key *ecdh.PrivateKey
	err := n.device.Handshake().WithPrivate(func(b []byte) error {
		var err error
		key, err = ecdh.X25519().NewPrivateKey(b)
		return err
	})
	if err != nil || key == nil {
		t.Fatalf("static key: %v", err)
	}
	return key
}

func (n *node) ownCards(t *testing.T) handshake.OwnCards {
	t.Helper()
	oc, err := n.builder.OwnCards(t.Context())
	if err != nil {
		t.Fatalf("own cards: %v", err)
	}
	return oc
}

// rig is one hidden server node with its store and access manager.
type rig struct {
	dir   string
	key   *secret.Value
	st    *store.Store
	clock *fakeClock

	account *identity.Account
	device  *identity.Device
	devices []byte

	srv     *node
	builder *cards.CardBuilder
	kv      store.KV
	mgr     *access.Manager
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{dir: t.TempDir(), key: testKey(), clock: newFakeClock()}

	account, err := identity.FromMnemonic(testPhrase, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	device, err := identity.NewDevice()
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	devices, err := account.SealSingleDeviceList(device, 1, r.clock.Now())
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	r.account, r.device, r.devices = account, device, devices
	t.Cleanup(func() {
		account.Destroy()
		device.Destroy()
		r.key.Destroy()
	})

	r.open(t)
	t.Cleanup(func() {
		r.mgr.Close()
		if r.st != nil {
			_ = r.st.Close()
		}
	})
	return r
}

func testKey() *secret.Value {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return secret.New(key)
}

func (r *rig) open(t *testing.T) {
	t.Helper()
	if r.st != nil {
		_ = r.st.Close()
	}
	st, err := store.Open(store.Options{Dir: r.dir, Key: r.key, MemCapMB: 0})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	r.st = st
	r.kv = st.Store(access.Namespace)

	// The hidden node's contacts listener is on a different address than the invitation one: an
	// invitation card must never name it (objects.md §10.3).
	r.builder = buildCards(t, r.account, r.device, r.devices, r.clock, cards.Hidden,
		objects.SuiteNoise3, "198.51.100.20", 5555, r.device.Masking().Public())
	r.srv = &node{account: r.account, device: r.device, devices: r.devices, builder: r.builder, clock: r.clock}
	r.mgr = r.newManager(t)
}

func (r *rig) newManager(t *testing.T) *access.Manager {
	t.Helper()
	return r.managerWith(t, r.kv, []cards.Listener{{
		Suite: objects.SuiteNoise3,
		Host:  "203.0.113.10",
		Port:  4242,
	}}, nil)
}

// managerWith opens a manager over an explicit KV and listener set (used to simulate a listener
// disappearing or an injected store failure).
func (r *rig) managerWith(t *testing.T, kv store.KV, listeners []cards.Listener, rand io.Reader) *access.Manager {
	t.Helper()
	m, err := access.New(access.Config{
		KV:          kv,
		Cards:       r.builder,
		Clock:       r.clock,
		ContactsKey: r.device.Masking(),
		Listeners:   listeners,
		Rand:        rand,
	})
	if err != nil {
		t.Fatalf("access.New: %v", err)
	}
	return m
}

// failingKV injects one Put failure, to prove that a state change is applied in memory even when
// it cannot be persisted.
type failingKV struct {
	store.KV
	mu        sync.Mutex
	failAt    int
	puts      int
	delFailAt int
	deletes   int
	err       error
}

func (f *failingKV) Put(key, value []byte) error {
	f.mu.Lock()
	f.puts++
	fail := f.failAt > 0 && f.puts >= f.failAt
	f.mu.Unlock()
	if fail {
		return f.err
	}
	return f.KV.Put(key, value)
}

func (f *failingKV) Delete(key []byte) error {
	f.mu.Lock()
	f.deletes++
	fail := f.delFailAt > 0 && f.deletes >= f.delFailAt
	f.mu.Unlock()
	if fail {
		return f.err
	}
	return f.KV.Delete(key)
}

func (f *failingKV) arm(n int) {
	f.mu.Lock()
	f.failAt, f.puts = n, 0
	f.mu.Unlock()
}

func (f *failingKV) armDelete(n int) {
	f.mu.Lock()
	f.delFailAt, f.deletes = n, 0
	f.mu.Unlock()
}

// fixedRand is a deterministic randomness source: every byte is b.
type fixedRand struct{ b byte }

func (r fixedRand) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// reopen closes the store and opens the manager again over the same directory.
func (r *rig) reopen(t *testing.T) {
	t.Helper()
	r.mgr.Close()
	r.open(t)
}

func (r *rig) clientNode(t *testing.T) *node {
	t.Helper()
	kp, err := identity.NewKeyPair()
	if err != nil {
		t.Fatalf("mask: %v", err)
	}
	t.Cleanup(kp.Destroy)
	return newNode(t, r.clock, cards.Visible, objects.SuiteReality2QUIC, "203.0.113.99", 443, kp.Public())
}

// linkCard decodes and verifies a link without a DHT lookup (objects.md §10).
func linkCard(t *testing.T, link string) *objects.Card {
	t.Helper()
	c, err := linkCardAt(t, link, testNow)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	return c
}

// linkCardAt is linkCard with an explicit verification time.
func linkCardAt(t *testing.T, link string, now time.Time) (*objects.Card, error) {
	t.Helper()
	l, err := objects.ParseLink(link)
	if err != nil {
		return nil, err
	}
	return l.Verify(now)
}

// cardMasks returns the masking public keys a card actually references, in address order.
func cardMasks(t *testing.T, c *objects.Card) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(c.Body.GetAddrs()))
	for _, a := range c.Body.GetAddrs() {
		i := int(a.GetMask())
		if i <= 0 || i > len(c.Body.GetMaskKeys()) {
			t.Fatalf("address mask index %d", i)
		}
		out = append(out, c.Body.GetMaskKeys()[i-1].GetKey())
	}
	return out
}

// hasMask reports whether a card references the public key.
func hasMask(c *objects.Card, pub []byte) bool {
	for _, m := range c.Body.GetMaskKeys() {
		if string(m.GetKey()) == string(pub) {
			return true
		}
	}
	return false
}
