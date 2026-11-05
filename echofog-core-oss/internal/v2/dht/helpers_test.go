package dht

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// testSelf is a device ID that does not collide with any card built by buildCard.
var testSelf = DeviceID{0x5e, 0x1f}

// buildCard signs a valid card for a deterministic device and account. Two calls with the same
// seed are two cards of the same device, which is what the update rules are about.
func buildCard(t *testing.T, seed, seq uint64, now time.Time, scope objects.Scope, flags uint32, addrs ...*pb.Addr) *objects.Card {
	t.Helper()

	var devSeed [32]byte
	binary.BigEndian.PutUint64(devSeed[0:8], seed)
	binary.BigEndian.PutUint64(devSeed[8:16], seed^0x9e3779b97f4a7c15)
	dev, err := keys.NewEd25519Signer(devSeed[:])
	if err != nil {
		t.Fatalf("device signer: %v", err)
	}
	var accSeed [32]byte
	binary.BigEndian.PutUint64(accSeed[0:8], seed^0x0123456789abcdef)
	binary.BigEndian.PutUint64(accSeed[8:16], seed^0xfedcba9876543210)
	acc, err := keys.NewEd25519Signer(accSeed[:])
	if err != nil {
		t.Fatalf("account signer: %v", err)
	}

	notAfter := uint64(now.Add(6 * 24 * time.Hour).Unix())
	list, err := objects.SealDeviceList(acc, 1, notAfter, &pb.DeviceList{
		Devices: []*pb.Device{{Key: dev.Public(), Added: uint64(now.Unix()), Flags: objects.DevicePrimary}},
	})
	if err != nil {
		t.Fatalf("device list: %v", err)
	}

	hs := make([]byte, 32)
	binary.BigEndian.PutUint64(hs, seed)
	body := &pb.NodeCard{
		ProtoVer: objects.MinProtocolVersion,
		HsKey:    keys.New(keys.X25519, hs),
		Account:  acc.Public(),
		Devices:  list,
		Scope:    uint32(scope),
		Role:     objects.RoleUser,
		Addrs:    addrs,
		Flags:    flags,
	}
	raw, err := objects.SealCard(dev, seq, notAfter, body)
	if err != nil {
		t.Fatalf("seal card: %v", err)
	}
	card, err := objects.VerifyCard(raw, now)
	if err != nil {
		t.Fatalf("verify card: %v", err)
	}
	return card
}

// testAddr is an address of a suite with no card rules beyond host and port, so tests can use
// plain IPv4 letters. The real suites are covered by the objects package.
func testAddr(host string, port uint32) *pb.Addr {
	return &pb.Addr{Suite: 0x77, Host: host, Port: port}
}

// cardInBucket grinds deterministic device keys until one lands in the requested bucket. The
// caller supplies a counter so tests can build many distinct devices of one bucket.
func cardInBucket(t *testing.T, self DeviceID, bucket int, counter uint64, now time.Time, addrs ...*pb.Addr) *objects.Card {
	t.Helper()
	for i := 0; i < 1<<16; i++ {
		seed := counter<<16 | uint64(i)
		c := buildCard(t, seed, 1, now, objects.ScopePublic, 0, addrs...)
		if bucketIndex(self, DeviceID(c.DeviceID)) == bucket {
			return c
		}
	}
	t.Fatalf("no device found in bucket %d", bucket)
	return nil
}

// bucketIndex is the number of leading bits self and other share; the table defines it.

func cardAddr(c *objects.Card) string {
	a := c.Addrs()
	if len(a) == 0 {
		return ""
	}
	return a[0].GetHost()
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}

// fakePinger answers with a fixed error and records every check.
type fakePinger struct {
	mu    sync.Mutex
	err   error
	calls []*objects.Card
	done  chan struct{}
}

func (p *fakePinger) Ping(_ context.Context, card *objects.Card) error {
	p.mu.Lock()
	p.calls = append(p.calls, card)
	p.mu.Unlock()
	if p.done != nil {
		select {
		case p.done <- struct{}{}:
		default:
		}
	}
	return p.err
}

func (p *fakePinger) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

// memKV is the smallest store.KV a table needs; it keeps the key order of Badger.
type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (k *memKV) Get(key []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[string(key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (k *memKV) Put(key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[string(key)] = append([]byte(nil), value...)
	return nil
}

func (k *memKV) PutTTL(key, value []byte, _ time.Duration) error { return k.Put(key, value) }

func (k *memKV) Delete(key []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, string(key))
	return nil
}

func (k *memKV) Has(key []byte) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.m[string(key)]
	return ok, nil
}

func (k *memKV) Scan(prefix []byte, limit int) ([]store.Entry, error) {
	return k.ScanFrom(prefix, nil, limit)
}

func (k *memKV) ScanFrom(prefix, start []byte, limit int) ([]store.Entry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := make([]string, 0, len(k.m))
	for key := range k.m {
		if bytes.HasPrefix([]byte(key), prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]store.Entry, 0, len(keys))
	for _, key := range keys {
		if len(start) > 0 && key <= string(start) {
			continue
		}
		out = append(out, store.Entry{Key: []byte(key), Value: append([]byte(nil), k.m[key]...)})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (k *memKV) count(prefix []byte) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for key := range k.m {
		if bytes.HasPrefix([]byte(key), prefix) {
			n++
		}
	}
	return n
}
