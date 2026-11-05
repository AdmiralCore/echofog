package cards

import (
	"context"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// testNow is the fixed instant every test starts from.
var testNow = time.Unix(1_800_000_000, 0)

// testPhrase is a valid BIP-39 phrase; the account key is irrelevant to card rules, only its
// shape matters.
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

// mapKV is an in-memory KV with an optional write failure.
type mapKV struct {
	mu     sync.Mutex
	m      map[string][]byte
	putErr error
	getErr error
	puts   int
}

func newMapKV() *mapKV { return &mapKV{m: make(map[string][]byte)} }

func (k *mapKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.getErr != nil {
		return nil, false, k.getErr
	}
	v, ok := k.m[keyString(key)]
	return append([]byte(nil), v...), ok, nil
}

func (k *mapKV) Put(_ context.Context, key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.putErr != nil {
		return k.putErr
	}
	k.puts++
	k.m[keyString(key)] = append([]byte(nil), value...)
	return nil
}

func (k *mapKV) set(key, value []byte) {
	k.mu.Lock()
	k.m[keyString(key)] = append([]byte(nil), value...)
	k.mu.Unlock()
}

func keyString(b []byte) string { return string(b) }

type material struct {
	account *identity.Account
	device  *identity.Device
	devices []byte
}

func newMaterial(t *testing.T) *material {
	t.Helper()
	account, err := identity.FromMnemonic(testPhrase, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	device, err := identity.NewDevice()
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	devices, err := account.SealSingleDeviceList(device, 1, testNow)
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	t.Cleanup(func() {
		account.Destroy()
		device.Destroy()
	})
	return &material{account: account, device: device, devices: devices}
}

func newMask(t *testing.T) keys.AKey {
	t.Helper()
	kp, err := identity.NewKeyPair()
	if err != nil {
		t.Fatalf("masking key: %v", err)
	}
	t.Cleanup(kp.Destroy)
	return kp.Public()
}

// baseConfig is a public user node with one REALITY listener.
func baseConfig(t *testing.T, m *material, kv KV, clk Clock) Config {
	t.Helper()
	return Config{
		Account:  m.account.Public(),
		Device:   m.device,
		HsKey:    m.device.Handshake().Public(),
		L1Suites: []uint32{1, 2},
		Role:     objects.RoleUser,
		Listeners: []Listener{{
			Suite: objects.SuiteReality2QUIC,
			Host:  "203.0.113.10",
			Port:  443,
			SNI:   "example.com",
			Mask:  newMask(t),
		}},
		Devices: m.devices,
		Release: "0.3.0",
		KV:      kv,
		Clock:   clk,
	}
}

// verifyBytes verifies a card and returns it.
func verifyBytes(t *testing.T, env []byte, now time.Time) *objects.Card {
	t.Helper()
	c, err := objects.VerifyCard(env, now)
	if err != nil {
		t.Fatalf("card does not verify: %v", err)
	}
	return c
}

func identityKeyPair(t *testing.T) (keys.AKey, error) {
	t.Helper()
	kp, err := identity.NewKeyPair()
	if err != nil {
		return nil, err
	}
	t.Cleanup(kp.Destroy)
	return kp.Public(), nil
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}
