package release_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/release"
	"github.com/echofog/echofog-core/internal/v2/store"
)

var t0 = time.Unix(1_800_000_000, 0)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: t0} }

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

func signer(t testing.TB, n byte) *keys.Signer {
	t.Helper()
	seed := sha256.Sum256([]byte{'r', 'k', n})
	s, err := keys.NewEd25519Signer(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// memKV is an in-memory store.KV with an injectable Put failure.
type memKV struct {
	mu      sync.Mutex
	m       map[string][]byte
	failPut bool
}

func newKV() *memKV { return &memKV{m: map[string][]byte{}} }

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
	if k.failPut {
		return errors.New("disk full")
	}
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
	var keysSorted []string
	for key := range k.m {
		if strings.HasPrefix(key, string(prefix)) && (start == nil || key > string(start)) {
			keysSorted = append(keysSorted, key)
		}
	}
	sort.Strings(keysSorted)
	var out []store.Entry
	for _, key := range keysSorted {
		if len(out) >= limit {
			break
		}
		out = append(out, store.Entry{Key: []byte(key), Value: append([]byte(nil), k.m[key]...)})
	}
	return out, nil
}

func (k *memKV) count(prefix string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for key := range k.m {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}

type rig struct {
	t      testing.TB
	clk    *fakeClock
	kv     *memKV
	k      []*keys.Signer // the three compiled release keys
	events []release.Event
	mu     sync.Mutex
}

func newRig(t *testing.T) *rig {
	return &rig{t: t, clk: newClock(), kv: newKV(), k: []*keys.Signer{signer(t, 1), signer(t, 2), signer(t, 3)}}
}

func (r *rig) config() release.Config {
	pubs := make([]keys.AKey, len(r.k))
	for i, s := range r.k {
		pubs[i] = s.Public()
	}
	return release.Config{
		Keys: pubs, KV: r.kv, Clock: r.clk, Own: map[string]uint32{"proto": 200},
		OnEvent: func(e release.Event) {
			r.mu.Lock()
			r.events = append(r.events, e)
			r.mu.Unlock()
		},
	}
}

func (r *rig) open() *release.Manager {
	r.t.Helper()
	m, err := release.Open(r.config())
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	return m
}

func (r *rig) got() []release.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]release.Event(nil), r.events...)
}

// seal builds a manifest released `age` before the rig's now, signed by the first signer and
// co-signed by the rest.
func (r *rig) seal(version string, age time.Duration, mut func(*pb.Release), signers ...*keys.Signer) []byte {
	r.t.Helper()
	rel := &pb.Release{Version: version, ReleasedAt: uint64(r.clk.Now().Add(-age).Unix())}
	if mut != nil {
		mut(rel)
	}
	raw, err := objects.SealRelease(signers[0], rel.ReleasedAt+400*86400, rel, signers[1:]...)
	if err != nil {
		r.t.Fatalf("SealRelease: %v", err)
	}
	return raw
}

func proto(min, rec uint32) func(*pb.Release) {
	return func(r *pb.Release) {
		r.Policies = []*pb.Policy{{Component: "proto", MinSupported: min, Recommended: rec}}
	}
}

func TestOneSignatureOfThreeIsRejected(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, proto(200, 200), r.k[0])
	if ok, err := m.Offer(raw); ok || err == nil {
		t.Fatalf("one signature: applied=%v err=%v", ok, err)
	}
	if m.Latest() != nil || m.Last() != nil {
		t.Fatal("a manifest with one signature became current")
	}
	if err := m.Check(raw, r.clk.Now()); err == nil {
		t.Fatal("Check accepts one signature")
	}
	// The same signer twice is still one key.
	dup := r.seal("0.3.1", time.Hour, proto(200, 200), r.k[0], r.k[0])
	if ok, err := m.Offer(dup); ok || err == nil {
		t.Fatalf("one key twice: applied=%v err=%v", ok, err)
	}
	// A key outside the set does not count either.
	stranger := signer(t, 99)
	out := r.seal("0.3.1", time.Hour, proto(200, 200), r.k[0], stranger)
	if ok, err := m.Offer(out); ok || err == nil {
		t.Fatalf("a stranger's cosignature: applied=%v err=%v", ok, err)
	}
	if r.kv.count("") != 0 {
		t.Fatal("a refused manifest was persisted")
	}
}

func TestTwoSignaturesApply(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, proto(200, 200), r.k[0], r.k[1])
	ok, err := m.Offer(raw)
	if err != nil || !ok {
		t.Fatalf("Offer: %v, %v", ok, err)
	}
	if !bytes.Equal(m.Latest(), raw) {
		t.Fatal("Latest is not the applied object")
	}
	if p := m.Policy("proto"); p == nil || p.GetMinSupported() != 200 {
		t.Fatalf("policy %v", p)
	}
	// Offering the same object again is not an error and applies nothing.
	if ok, err := m.Offer(raw); ok || err != nil {
		t.Fatalf("repeat: %v, %v", ok, err)
	}
	if len(r.got()) != 0 {
		t.Fatalf("events for an up-to-date node: %+v", r.got())
	}
}

func TestRevokedKeyDoesNotCount(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	k4 := signer(t, 4)
	rot := r.seal("0.3.1", 2*time.Hour, func(rel *pb.Release) {
		rel.NextKeys = [][]byte{k4.Public()}
		rel.RevokedKeys = [][]byte{r.k[2].Public()}
	}, r.k[0], r.k[1])
	if ok, err := m.Offer(rot); err != nil || !ok {
		t.Fatalf("rotation: %v, %v", ok, err)
	}
	if len(m.Keys()) != 3 {
		t.Fatalf("key set %d keys", len(m.Keys()))
	}
	// The revoked key plus a good one: one valid signature only.
	bad := r.seal("0.3.2", time.Hour, nil, r.k[0], r.k[2])
	if ok, err := m.Offer(bad); ok || err == nil {
		t.Fatalf("a revoked key counted: %v, %v", ok, err)
	}
	// A new key plus an old one is valid.
	good := r.seal("0.3.2", time.Hour, nil, k4, r.k[0])
	if ok, err := m.Offer(good); err != nil || !ok {
		t.Fatalf("the new key: %v, %v", ok, err)
	}
}

func TestRollbackIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	older := r.seal("0.3.0", 48*time.Hour, proto(200, 200), r.k[0], r.k[1])
	newer := r.seal("0.3.1", time.Hour, proto(200, 200), r.k[1], r.k[2])
	if ok, err := m.Offer(newer); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := m.Offer(older); ok || !errors.Is(err, release.ErrStale) {
		t.Fatalf("an older manifest: %v, %v", ok, err)
	}
	// The refusal survives a restart: the chain is persisted.
	m2 := r.open()
	if ok, err := m2.Offer(older); ok || !errors.Is(err, release.ErrStale) {
		t.Fatalf("an older manifest after restart: %v, %v", ok, err)
	}
	if !bytes.Equal(m2.Latest(), newer) {
		t.Fatal("restart lost the applied manifest")
	}
	// Same released_at is not newer either.
	twin := r.seal("0.3.1-b", time.Hour, proto(200, 200), r.k[0], r.k[2])
	if ok, err := m2.Offer(twin); ok || err == nil {
		t.Fatalf("a twin manifest: %v, %v", ok, err)
	}
}

func TestChainSurvivesRestartAndKeyRotation(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	k4 := signer(t, 4)
	m1 := r.seal("0.3.1", 5*time.Hour, func(rel *pb.Release) {
		rel.NextKeys = [][]byte{k4.Public()}
		rel.Policies = []*pb.Policy{{Component: "proto", MinSupported: 250, Recommended: 250}}
	}, r.k[0], r.k[1])
	// m2 is signed by the key m1 introduced; min_supported 400 is clipped to 350 by objects.
	m2 := r.seal("0.3.2", time.Hour, proto(400, 400), k4, r.k[0])
	for i, raw := range [][]byte{m1, m2} {
		if ok, err := m.Offer(raw); err != nil || !ok {
			t.Fatalf("manifest %d: %v, %v", i+1, ok, err)
		}
	}
	want := m.Policy("proto").GetMinSupported()
	wantKeys := len(m.Keys())

	m2nd := r.open()
	if !bytes.Equal(m2nd.Latest(), m2) {
		t.Fatal("restart lost the last manifest")
	}
	if len(m2nd.Keys()) != wantKeys {
		t.Fatalf("restart: %d keys, want %d", len(m2nd.Keys()), wantKeys)
	}
	if got := m2nd.Policy("proto").GetMinSupported(); got != want {
		t.Fatalf("restart: effective min_supported %d, want %d (clipping chain)", got, want)
	}
	// The key introduced by m1 is usable after the restart.
	m3 := r.seal("0.3.3", 0, proto(200, 200), k4, r.k[1])
	if ok, err := m2nd.Offer(m3); err != nil || !ok {
		t.Fatalf("a rotated key after restart: %v, %v", ok, err)
	}
}

func TestReplayWithOtherCompiledKeysStartsClean(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	if ok, err := m.Offer(r.seal("0.3.1", time.Hour, proto(200, 200), r.k[0], r.k[1])); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// A build with other release keys cannot trust the stored chain: fail closed to its own keys.
	cfg := r.config()
	cfg.Keys = []keys.AKey{signer(t, 7).Public(), signer(t, 8).Public(), signer(t, 9).Public()}
	m2, err := release.Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m2.Latest() != nil || len(m2.Keys()) != 3 {
		t.Fatal("a chain signed by other keys was trusted")
	}
}

func TestCorruptStateIsIgnored(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	if ok, err := m.Offer(r.seal("0.3.1", time.Hour, proto(200, 200), r.k[0], r.k[1])); err != nil || !ok {
		t.Fatal(ok, err)
	}
	r.kv.mu.Lock()
	for k := range r.kv.m {
		if strings.HasPrefix(k, "m/") {
			r.kv.m[k] = []byte("garbage")
		}
	}
	r.kv.mu.Unlock()
	if m2 := r.open(); m2.Latest() != nil {
		t.Fatal("garbage became the current manifest")
	}
	// And an unknown schema version is not read at all.
	r.kv.mu.Lock()
	r.kv.m["v"] = []byte{99}
	r.kv.mu.Unlock()
	if m3 := r.open(); m3.Latest() != nil {
		t.Fatal("a chain of an unknown schema version was read")
	}
}

func TestFutureAndExpired(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	// released_at more than a day ahead.
	future := r.seal("0.9.0", -48*time.Hour, nil, r.k[0], r.k[1])
	if ok, err := m.Offer(future); ok || err == nil {
		t.Fatalf("a manifest from the future: %v, %v", ok, err)
	}
	// A manifest of the next hour is fine; one that expired is not.
	soon := r.seal("0.3.1", -time.Hour, nil, r.k[0], r.k[1])
	if ok, err := m.Offer(soon); err != nil || !ok {
		t.Fatalf("a manifest a little ahead: %v, %v", ok, err)
	}
	r2 := newRig(t)
	m2 := r2.open()
	old := r2.seal("0.1.0", 401*24*time.Hour, nil, r2.k[0], r2.k[1])
	if ok, err := m2.Offer(old); ok || err == nil {
		t.Fatalf("an expired manifest: %v, %v", ok, err)
	}
}

// TestClockIsInjected: the manager reads the node's corrected clock (AD13), not the system clock.
func TestClockIsInjected(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, nil, r.k[0], r.k[1]) // not_after = released_at + 400 days
	r.clk.Advance(401 * 24 * time.Hour)
	if ok, err := m.Offer(raw); ok || err == nil {
		t.Fatalf("expired by the injected clock: %v, %v", ok, err)
	}
}

func TestSizeLimit(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	big := make([]byte, release.MaxManifestSize+1)
	if _, err := m.Offer(big); !errors.Is(err, release.ErrTooLarge) {
		t.Fatalf("Offer: %v", err)
	}
	if err := m.Check(big, r.clk.Now()); !errors.Is(err, release.ErrTooLarge) {
		t.Fatalf("Check: %v", err)
	}
	if _, err := m.Offer(nil); err == nil {
		t.Fatal("empty manifest accepted")
	}
	// A manifest with many artifacts that still fits is fine; one that does not is cut at the limit.
	fat := r.seal("0.3.1", time.Hour, func(rel *pb.Release) {
		for i := 0; i < 400; i++ {
			rel.Artifacts = append(rel.Artifacts, &pb.Artifact{Platform: "linux-arm64", Name: strings.Repeat("a", 60), Sha256: make([]byte, 32), Url: "https://example.org/" + strings.Repeat("b", 60)})
		}
	}, r.k[0], r.k[1])
	if len(fat) <= release.MaxManifestSize {
		t.Fatalf("test manifest is only %d bytes", len(fat))
	}
	if ok, err := m.Offer(fat); ok || !errors.Is(err, release.ErrTooLarge) {
		t.Fatalf("an oversize manifest: %v, %v", ok, err)
	}
}

func TestNeverLockOut(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	// min_supported 300 is above the node's own 200 (and one major step from 200 is exactly 300).
	if ok, err := m.Offer(r.seal("0.4.0", time.Hour, proto(300, 300), r.k[0], r.k[1])); err != nil || !ok {
		t.Fatal(ok, err)
	}
	admit := m.Admit("proto", 200)
	for _, peer := range []uint32{100, 199, 200, 300} {
		if !admit(peer) {
			t.Fatalf("peer %d refused by a node that is itself below min_supported", peer)
		}
	}
	if m.PeerRefused("proto", 200, 100, r.clk.Now()) {
		t.Fatal("PeerRefused locks the node out")
	}
	if st := m.Status("proto"); st != objects.UpdateRequired {
		t.Fatalf("status %v, want update required", st)
	}
	// A later manifest with a floor below the node's own refuses older peers with REJECT{version}.
	r.clk.Advance(2 * time.Hour)
	if ok, err := m.Offer(r.seal("0.4.1", 0, proto(150, 150), r.k[1], r.k[2])); err != nil || !ok {
		t.Fatal(ok, err)
	}
	admit = m.Admit("proto", 200)
	if admit(149) {
		t.Fatal("a peer below min_supported is admitted")
	}
	if !admit(150) || !admit(200) || !admit(250) {
		t.Fatal("a peer at or above min_supported is refused")
	}
}

func TestAdmitWithoutManifest(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	if admit := m.Admit("proto", 200); admit == nil || !admit(1) {
		t.Fatal("without a manifest everybody is admitted")
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	// Up to date: no event.
	if _, err := m.Offer(r.seal("0.3.1", 5*time.Hour, proto(200, 200), r.k[0], r.k[1])); err != nil {
		t.Fatal(err)
	}
	// Recommended above the node's version: an event, not required.
	if _, err := m.Offer(r.seal("0.3.2", 4*time.Hour, func(rel *pb.Release) {
		proto(200, 210)(rel)
		rel.NotesUrl = "https://example.org/notes"
	}, r.k[0], r.k[1])); err != nil {
		t.Fatal(err)
	}
	// min_supported above: required.
	if _, err := m.Offer(r.seal("0.3.3", 3*time.Hour, proto(300, 300), r.k[0], r.k[1])); err != nil {
		t.Fatal(err)
	}
	// A refused manifest makes no event.
	if _, err := m.Offer(r.seal("0.3.4", 2*time.Hour, proto(300, 400), r.k[0])); err == nil {
		t.Fatal("one signature accepted")
	}
	ev := r.got()
	if len(ev) != 2 {
		t.Fatalf("%d events: %+v", len(ev), ev)
	}
	if ev[0].Version != "0.3.2" || ev[0].Required || ev[0].NotesURL != "https://example.org/notes" ||
		len(ev[0].Components) != 1 || ev[0].Components[0].Status != objects.UpdateRecommended || ev[0].Components[0].Component != "proto" {
		t.Fatalf("recommended event %+v", ev[0])
	}
	if ev[1].Version != "0.3.3" || !ev[1].Required || ev[1].Components[0].Status != objects.UpdateRequired {
		t.Fatalf("required event %+v", ev[1])
	}
	if !ev[1].At.Equal(r.clk.Now()) {
		t.Fatalf("event time %v", ev[1].At)
	}
}

// TestRestartDoesNotRepeatEvents: replay of the chain is silent.
func TestRestartDoesNotRepeatEvents(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	if _, err := m.Offer(r.seal("0.3.2", time.Hour, proto(200, 210), r.k[0], r.k[1])); err != nil {
		t.Fatal(err)
	}
	before := len(r.got())
	r.open()
	if len(r.got()) != before {
		t.Fatal("replay emitted events")
	}
}

func TestPersistFailureDoesNotApply(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, proto(200, 210), r.k[0], r.k[1])
	r.kv.failPut = true
	if ok, err := m.Offer(raw); ok || err == nil {
		t.Fatalf("Offer with a failing store: %v, %v", ok, err)
	}
	if m.Latest() != nil || len(r.got()) != 0 {
		t.Fatal("state moved although it could not be persisted")
	}
	r.kv.failPut = false
	if ok, err := m.Offer(raw); err != nil || !ok {
		t.Fatalf("retry: %v, %v", ok, err)
	}
}

func TestChainIsBounded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	for i := 0; i < release.MaxChain; i++ {
		rel := r.seal("0.3.1", time.Duration(release.MaxChain+1-i)*time.Minute, nil, r.k[i%3], r.k[(i+1)%3])
		if ok, err := m.Offer(rel); err != nil || !ok {
			t.Fatalf("manifest %d: %v, %v", i, ok, err)
		}
	}
	if n := r.kv.count("m/"); n != release.MaxChain {
		t.Fatalf("%d records", n)
	}
	extra := r.seal("0.3.2", 0, nil, r.k[0], r.k[1])
	if ok, err := m.Offer(extra); ok || !errors.Is(err, release.ErrChainFull) {
		t.Fatalf("beyond the limit: %v, %v", ok, err)
	}
	if r.kv.count("m/") != release.MaxChain {
		t.Fatal("the chain grew past its limit")
	}
}

func TestStorageLayout(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	rel := r.seal("0.3.1", time.Hour, nil, r.k[0], r.k[1])
	if _, err := m.Offer(rel); err != nil {
		t.Fatal(err)
	}
	ver, err := r.kv.Get([]byte("v"))
	if err != nil || len(ver) != 1 || ver[0] != 1 {
		t.Fatalf("schema version %v, %v", ver, err)
	}
	at := uint64(t0.Add(-time.Hour).Unix())
	key := make([]byte, 2+8)
	copy(key, "m/")
	binary.BigEndian.PutUint64(key[2:], at)
	got, err := r.kv.Get(key)
	if err != nil || !bytes.Equal(got, rel) {
		t.Fatalf("record under m/<released_at>: %v", err)
	}
}

func TestConcurrentOffers(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, proto(200, 210), r.k[0], r.k[1])
	var wg sync.WaitGroup
	applied := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := m.Offer(raw)
			if err != nil {
				t.Errorf("Offer: %v", err)
			}
			applied <- ok
		}()
	}
	wg.Wait()
	close(applied)
	n := 0
	for ok := range applied {
		if ok {
			n++
		}
	}
	if n != 1 || len(r.got()) != 1 {
		t.Fatalf("%d applications, %d events; want one each", n, len(r.got()))
	}
}

type fakeDHT struct {
	raw   []byte
	err   error
	valid func(raw []byte, now time.Time) error
}

func (f *fakeDHT) GetRelease(_ context.Context, valid func(raw []byte, now time.Time) error) (*objects.Envelope, error) {
	f.valid = valid
	if f.err != nil {
		return nil, f.err
	}
	return &objects.Envelope{Label: objects.LabelRelease, Raw: f.raw}, nil
}

func TestPollDHT(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, proto(200, 210), r.k[0], r.k[1])
	src := &fakeDHT{raw: raw}
	ok, err := m.PollDHT(t.Context(), src)
	if err != nil || !ok {
		t.Fatalf("PollDHT: %v, %v", ok, err)
	}
	if src.valid == nil {
		t.Fatal("the DHT client was not given the manager's check")
	}
	if err := src.valid(r.seal("0.3.9", time.Hour, nil, r.k[0]), r.clk.Now()); err == nil {
		t.Fatal("the check given to the DHT accepts one signature")
	}
	// Not found and failures are not applications.
	for _, e := range []error{errors.New("dht: not found"), context.DeadlineExceeded} {
		if ok, err := m.PollDHT(t.Context(), &fakeDHT{err: e}); ok || err == nil {
			t.Fatalf("PollDHT(%v) = %v, %v", e, ok, err)
		}
	}
	// A record that does not pass is an error and changes nothing.
	if ok, err := m.PollDHT(t.Context(), &fakeDHT{raw: r.seal("0.3.2", 0, nil, r.k[2])}); ok || err == nil {
		t.Fatalf("PollDHT with a bad record: %v, %v", ok, err)
	}
	if !bytes.Equal(m.Latest(), raw) {
		t.Fatal("a bad DHT record replaced the manifest")
	}
}

func TestFetchSite(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	m := r.open()
	raw := r.seal("0.3.1", time.Hour, proto(200, 210), r.k[0], r.k[1])
	var body []byte = raw
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/redirect" {
			http.Redirect(w, req, "http://example.org/release", http.StatusFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	hc := srv.Client()

	ok, err := m.FetchSite(t.Context(), hc, srv.URL+"/release")
	if err != nil || !ok {
		t.Fatalf("FetchSite: %v, %v", ok, err)
	}

	r2 := newRig(t)
	m2 := r2.open()
	if ok, err := m2.FetchSite(t.Context(), hc, "http://example.org/release"); ok || !errors.Is(err, release.ErrSource) {
		t.Fatalf("plain http: %v, %v", ok, err)
	}
	if ok, err := m2.FetchSite(t.Context(), hc, srv.URL+"/redirect"); ok || err == nil {
		t.Fatalf("a redirect to http: %v, %v", ok, err)
	}
	status = http.StatusNotFound
	if ok, err := m2.FetchSite(t.Context(), hc, srv.URL+"/release"); ok || err == nil {
		t.Fatalf("404: %v, %v", ok, err)
	}
	status = http.StatusOK
	body = bytes.Repeat([]byte{1}, 10<<20)
	if ok, err := m2.FetchSite(t.Context(), hc, srv.URL+"/release"); ok || !errors.Is(err, release.ErrTooLarge) {
		t.Fatalf("10 MiB body: %v, %v", ok, err)
	}
	if m2.Latest() != nil {
		t.Fatal("a bad download became current")
	}
}
