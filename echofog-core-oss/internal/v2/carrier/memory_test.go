package carrier

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

type fakeKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newFakeKV() *fakeKV { return &fakeKV{m: make(map[string][]byte)} }

func (k *fakeKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[keyBytes(key)]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

func (k *fakeKV) Put(_ context.Context, key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[keyBytes(key)] = append([]byte(nil), value...)
	return nil
}

func keyBytes(b []byte) string { return string(b) }

func TestAddressKey(t *testing.T) {
	cases := []struct {
		name string
		addr *pb.Addr
		want string
	}{
		{"ipv4", &pb.Addr{Suite: objects.SuiteReality2TCP, Host: "203.0.113.20", Port: 443}, "203.0.113.20:443"},
		{"hostname", &pb.Addr{Suite: objects.SuiteNoise3, Host: "Peer.Example", Port: 8443}, "peer.example:8443"},
		{"ws2_default_port", &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://Cover.Example/ws"}, "cover.example:443"},
		{"ws2_explicit_port", &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://cover.example:8443/ws"}, "cover.example:8443"},
		{"empty", &pb.Addr{Suite: objects.SuiteWS2, Url: "://bad"}, ""},
		{"no_host", &pb.Addr{Suite: objects.SuiteNoise3, Port: 443}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AddressKey(c.addr); got != c.want {
				t.Fatalf("AddressKey() = %q, want %q", got, c.want)
			}
		})
	}
}

func testMemory(t *testing.T, kv KV, addrs []netip.Addr, now func() time.Time) *Memory {
	t.Helper()
	return NewMemory(MemoryConfig{
		KV:         kv,
		LocalAddrs: func() []netip.Addr { return addrs },
		Now:        now,
		TTL:        30 * time.Minute,
	})
}

func TestMemoryRoundTrip(t *testing.T) {
	kv := newFakeKV()
	now := time.Unix(1_700_000_000, 0)
	m := testMemory(t, kv, []netip.Addr{ipLocal, ipLocal2}, func() time.Time { return now })
	ctx := context.Background()
	if err := m.Remember(ctx, "203.0.113.20:443", objects.SuiteReality2TCP); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	suite, ok, err := m.Lookup(ctx, "203.0.113.20:443")
	if err != nil || !ok || suite != objects.SuiteReality2TCP {
		t.Fatalf("Lookup = (%d, %v, %v), want (tcp, true, nil)", suite, ok, err)
	}
	if _, ok, _ := m.Lookup(ctx, "203.0.113.21:443"); ok {
		t.Fatal("Lookup returned an entry for another address")
	}
}

func TestMemoryDependsOnNetworkFingerprint(t *testing.T) {
	kv := newFakeKV()
	now := time.Unix(1_700_000_000, 0)
	ctx := context.Background()
	m1 := testMemory(t, kv, []netip.Addr{ipLocal, ipLocal2}, func() time.Time { return now })
	if err := m1.Remember(ctx, "peer:443", objects.SuiteNoise3); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// The same set in another order is the same network.
	m2 := testMemory(t, kv, []netip.Addr{ipLocal2, ipLocal}, func() time.Time { return now })
	if _, ok, _ := m2.Lookup(ctx, "peer:443"); !ok {
		t.Fatal("Lookup on the same network with reordered addresses missed")
	}
	// A different network must not reuse the entry.
	m3 := testMemory(t, kv, []netip.Addr{ipBorder}, func() time.Time { return now })
	if _, ok, _ := m3.Lookup(ctx, "peer:443"); ok {
		t.Fatal("Lookup on another network returned the entry")
	}
}

func TestMemoryWithoutLocalAddresses(t *testing.T) {
	kv := newFakeKV()
	m := testMemory(t, kv, nil, time.Now)
	ctx := context.Background()
	if err := m.Remember(ctx, "peer:443", objects.SuiteNoise3); err != nil {
		t.Fatalf("Remember without addresses: %v", err)
	}
	if _, ok, _ := m.Lookup(ctx, "peer:443"); ok {
		t.Fatal("Lookup without a network fingerprint found an entry")
	}
	kv.mu.Lock()
	n := len(kv.m)
	kv.mu.Unlock()
	if n != 0 {
		t.Fatalf("unfingerprinted memory wrote %d keys", n)
	}
}

func TestMemoryExpires(t *testing.T) {
	kv := newFakeKV()
	now := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return now }
	m := testMemory(t, kv, []netip.Addr{ipLocal}, nowFn)
	ctx := context.Background()
	if err := m.Remember(ctx, "peer:443", objects.SuiteReality2QUIC); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	now = now.Add(29 * time.Minute)
	if _, ok, _ := m.Lookup(ctx, "peer:443"); !ok {
		t.Fatal("fresh entry expired too early")
	}
	now = now.Add(2 * time.Minute)
	if _, ok, _ := m.Lookup(ctx, "peer:443"); ok {
		t.Fatal("stale entry was still returned")
	}
}

func TestMemoryErrors(t *testing.T) {
	ctx := context.Background()
	want := errors.New("kv down")
	kv := &errKV{err: want}
	m := testMemory(t, kv, []netip.Addr{ipLocal}, time.Now)
	if err := m.Remember(ctx, "peer:443", objects.SuiteNoise3); !errors.Is(err, want) {
		t.Fatalf("Remember error = %v, want %v", err, want)
	}
	if _, _, err := m.Lookup(ctx, "peer:443"); !errors.Is(err, want) {
		t.Fatalf("Lookup error = %v, want %v", err, want)
	}
}

type errKV struct{ err error }

func (k *errKV) Get(context.Context, []byte) ([]byte, bool, error) { return nil, false, k.err }
func (k *errKV) Put(context.Context, []byte, []byte) error         { return k.err }

func TestMemoryRejectsCorruptValue(t *testing.T) {
	kv := newFakeKV()
	m := testMemory(t, kv, []netip.Addr{ipLocal}, time.Now)
	ctx := context.Background()
	if err := m.Remember(ctx, "peer:443", objects.SuiteNoise3); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// Corrupt every stored value; Lookup must treat the entry as absent.
	kv.mu.Lock()
	for k := range kv.m {
		kv.m[k] = []byte{1}
	}
	kv.mu.Unlock()
	if _, ok, err := m.Lookup(ctx, "peer:443"); err != nil || ok {
		t.Fatalf("Lookup(corrupt) = (_, %v, %v), want (false, nil)", ok, err)
	}
}
