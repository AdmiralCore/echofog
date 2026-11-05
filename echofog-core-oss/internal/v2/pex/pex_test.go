package pex

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// memKV is the store.KV of the tests: one namespace, ordered keys, safe for concurrent use.
type memKV struct {
	mu   sync.Mutex
	vals map[string][]byte
}

func newMemKV() *memKV { return &memKV{vals: map[string][]byte{}} }

func (m *memKV) Get(key []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[string(key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (m *memKV) Put(key, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[string(key)] = append([]byte(nil), value...)
	return nil
}

func (m *memKV) PutTTL(key, value []byte, _ time.Duration) error { return m.Put(key, value) }

func (m *memKV) Delete(key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, string(key))
	return nil
}

func (m *memKV) Has(key []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.vals[string(key)]
	return ok, nil
}

func (m *memKV) Scan(prefix []byte, limit int) ([]store.Entry, error) {
	return m.ScanFrom(prefix, nil, limit)
}

func (m *memKV) ScanFrom(prefix, start []byte, limit int) ([]store.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Entry
	for k, v := range m.vals {
		kb := []byte(k)
		if !hasPrefix(kb, prefix) || (len(start) > 0 && string(kb) <= string(start)) {
			continue
		}
		out = append(out, store.Entry{Key: append([]byte(nil), kb...), Value: append([]byte(nil), v...)})
		if len(out) == limit {
			break
		}
	}
	// The real store returns records in key order; the tests below do not depend on the order,
	// but sorting keeps behaviour comparable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && string(out[j].Key) < string(out[j-1].Key); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func hasPrefix(b, prefix []byte) bool {
	if len(prefix) > len(b) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type cardOpts struct {
	name     string
	seq      uint64
	scope    objects.Scope
	role     uint32
	flags    uint32
	region   string
	host     string
	port     uint32
	notAfter time.Duration
}

func testCard(t *testing.T, o cardOpts) *objects.Card {
	t.Helper()
	if o.host == "" {
		o.host = "192.0.2.10"
	}
	if o.port == 0 {
		o.port = 443
	}
	if o.notAfter == 0 {
		o.notAfter = 24 * time.Hour
	}
	seed := func(what string) []byte {
		h := sha256.Sum256([]byte("pex test " + o.name + " " + what))
		return h[:]
	}
	account, err := keys.NewEd25519Signer(seed("account"))
	if err != nil {
		t.Fatalf("account signer: %v", err)
	}
	device, err := keys.NewEd25519Signer(seed("device"))
	if err != nil {
		t.Fatalf("device signer: %v", err)
	}
	raw := make([]byte, 32)
	copy(raw, seed("x25519"))
	hsKey := keys.New(keys.X25519, raw)
	maskKey := keys.New(keys.X25519, raw)

	notAfter := uint64(testNow.Add(o.notAfter).Unix())
	devices, err := objects.SealDeviceList(account, 1, notAfter, &pb.DeviceList{
		Devices: []*pb.Device{{Key: device.Public(), Added: uint64(testNow.Unix())}},
	})
	if err != nil {
		t.Fatalf("device list: %v", err)
	}

	body := &pb.NodeCard{
		ProtoVer: 200,
		HsKey:    hsKey,
		L1Suites: []uint32{1, 2},
		Role:     o.role,
		Flags:    o.flags,
		Account:  account.Public(),
		Devices:  devices,
		Scope:    uint32(o.scope),
		MaskKeys: []*pb.MaskKey{{Key: maskKey, NotAfter: notAfter}},
		Addrs:    []*pb.Addr{{Suite: objects.SuiteReality2QUIC, Host: o.host, Port: o.port, Mask: 1}},
		Release:  "test",
		Services: nil,
		Features: nil,
	}
	if o.region != "" {
		body.Exit = &pb.ExitOffer{Region: o.region, Price: 1}
	}
	env, err := objects.SealCard(device, o.seq, notAfter, body)
	if err != nil {
		t.Fatalf("seal card: %v", err)
	}
	c, err := objects.VerifyCard(env, testNow)
	if err != nil {
		t.Fatalf("verify card: %v", err)
	}
	return c
}

func addrOf(t *testing.T, c *objects.Card) netip.AddrPort {
	t.Helper()
	a := c.Addrs()[0]
	ip, err := netip.ParseAddr(a.GetHost())
	if err != nil {
		t.Fatalf("parse card host: %v", err)
	}
	return netip.AddrPortFrom(ip, uint16(a.GetPort()))
}

func openTestStore(t *testing.T, kv *memKV) *Store {
	t.Helper()
	s, err := Open(Options{KV: kv, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("pex.Open: %v", err)
	}
	return s
}

func TestCandidateIsNewNotGuard(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	c := testCard(t, cardOpts{name: "candidate", seq: 10})

	if !st.AddCandidate(c, testNow) {
		t.Fatal("AddCandidate refused a verified public card")
	}
	if st.LenNew() != 1 || st.LenTried() != 0 {
		t.Fatalf("new/tried = %d/%d, want 1/0", st.LenNew(), st.LenTried())
	}
	if got := st.Guards(10); len(got) != 0 {
		t.Fatalf("a card never reached became a guard candidate: %d", len(got))
	}
	if got := st.Serve(testNow); len(got) != 0 {
		t.Fatalf("a card never reached is served to PEX: %d", len(got))
	}
	if got := st.DialCandidates(10); len(got) != 1 || got[0].DeviceID != c.DeviceID {
		t.Fatalf("dial candidates %d, want the new one", len(got))
	}
}

func TestWorkingSessionMovesCandidateToTried(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	c := testCard(t, cardOpts{name: "tried", seq: 10, notAfter: 72 * time.Hour})

	if st.AddCandidate(c, testNow) != true {
		t.Fatal("AddCandidate")
	}
	if !st.NoteSession(c, addrOf(t, c), testNow) {
		t.Fatal("NoteSession did not confirm the card")
	}
	if st.LenNew() != 0 || st.LenTried() != 1 {
		t.Fatalf("new/tried = %d/%d, want 0/1", st.LenNew(), st.LenTried())
	}
	if got := st.Guards(10); len(got) != 1 {
		t.Fatalf("tried card is not a guard candidate: %d", len(got))
	}
	if got := st.Serve(testNow); len(got) != 1 {
		t.Fatalf("tried card reached now is not served: %d", len(got))
	}
	// 25 hours later the 24-hour confirmation is stale for PEX but the node stays tried: the card
	// itself is still valid (72 h lifetime).
	later := testNow.Add(25 * time.Hour)
	if got := st.Serve(later); len(got) != 0 {
		t.Fatalf("a card reached 25 h ago is still served: %d", len(got))
	}
	if got := st.Guards(10); len(got) != 1 {
		t.Fatalf("a tried card stopped being a guard candidate: %d", len(got))
	}
	if !st.NoteSession(c, addrOf(t, c), later) {
		t.Fatal("re-confirmation refused")
	}
	if got := st.Serve(later); len(got) != 1 {
		t.Fatalf("re-confirmed card is not served: %d", len(got))
	}
}

func TestExpiredCardsAreNotOffered(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	short := testCard(t, cardOpts{name: "short", seq: 10, port: 6100})
	if !st.NoteSession(short, addrOf(t, short), testNow) {
		t.Fatal("NoteSession short")
	}

	// 25 hours later the 24-hour card is past not_after (and the skew window): it must be neither
	// served nor offered for guards or dials.
	later := testNow.Add(25 * time.Hour)
	if got := st.Serve(later); len(got) != 0 {
		t.Fatalf("Serve returned an expired card: %d", len(got))
	}
	if got := st.Guards(10); len(got) != 0 {
		t.Fatalf("Guards returned an expired card: %d", len(got))
	}
	if got := st.DialCandidates(10); len(got) != 0 {
		t.Fatalf("DialCandidates returned an expired card: %d", len(got))
	}

	// A live card is still offered: only expiry is filtered, not the whole tried table.
	long := testCard(t, cardOpts{name: "long", seq: 10, port: 6101, notAfter: 72 * time.Hour})
	if !st.NoteSession(long, addrOf(t, long), later) {
		t.Fatal("NoteSession long")
	}
	if got := st.Serve(later); len(got) != 1 || got[0].DeviceID != long.DeviceID {
		t.Fatalf("Serve = %d cards, want only the live one", len(got))
	}
	if got := st.Guards(10); len(got) != 1 || got[0].DeviceID != long.DeviceID {
		t.Fatalf("Guards = %d cards, want only the live one", len(got))
	}
	if got := st.DialCandidates(10); len(got) != 1 || got[0].DeviceID != long.DeviceID {
		t.Fatalf("DialCandidates = %d cards, want only the live one", len(got))
	}
}

func TestServeRequiresTheReachedAddressInTheCurrentCard(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	first := testCard(t, cardOpts{name: "moving", seq: 10, host: "192.0.2.10", port: 443})
	if !st.NoteSession(first, addrOf(t, first), testNow) {
		t.Fatal("NoteSession")
	}
	// The same device reissues its card with another address; PEX brings the newer card.
	next := testCard(t, cardOpts{name: "moving", seq: 11, host: "192.0.2.11", port: 8443})
	if next.DeviceID != first.DeviceID {
		t.Fatal("test cards must belong to the same device")
	}
	if st.AddCandidate(next, testNow.Add(time.Minute)) {
		t.Fatal("AddCandidate reported a new device for a known one")
	}
	if got := st.Serve(testNow.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("served a card whose new address was never reached: %d", len(got))
	}
	if got := st.Guards(10); len(got) != 1 {
		t.Fatalf("guards = %d, want the tried device", len(got))
	}
	if !st.NoteSession(next, addrOf(t, next), testNow.Add(2*time.Minute)) {
		t.Fatal("NoteSession for the new address")
	}
	if got := st.Serve(testNow.Add(2 * time.Minute)); len(got) != 1 {
		t.Fatalf("served = %d, want the re-confirmed card", len(got))
	}
}

func TestUnlistedIsNeverStored(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	hidden := testCard(t, cardOpts{
		name: "hidden", seq: 10, scope: objects.ScopePeer,
		flags: objects.FlagUnlisted,
	})
	if st.AddCandidate(hidden, testNow) {
		t.Fatal("AddCandidate stored an unlisted card")
	}
	st.NoteSession(hidden, addrOf(t, hidden), testNow)
	if st.LenNew() != 0 || st.LenTried() != 0 {
		t.Fatalf("unlisted card entered the tables: new/tried = %d/%d", st.LenNew(), st.LenTried())
	}
	if len(st.Serve(testNow)) != 0 || len(st.Guards(10)) != 0 || len(st.DialCandidates(10)) != 0 {
		t.Fatal("unlisted card is used")
	}
}

func TestTablesPersistAcrossOpen(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	candidate := testCard(t, cardOpts{name: "persist-new", seq: 10, port: 4000})
	tried := testCard(t, cardOpts{name: "persist-tried", seq: 10, port: 4001})
	st.AddCandidate(candidate, testNow)
	if !st.NoteSession(tried, addrOf(t, tried), testNow) {
		t.Fatal("NoteSession")
	}

	again := openTestStore(t, kv)
	if again.LenNew() != 1 || again.LenTried() != 1 {
		t.Fatalf("after reopen new/tried = %d/%d, want 1/1", again.LenNew(), again.LenTried())
	}
	if got := again.Serve(testNow); len(got) != 1 || got[0].DeviceID != tried.DeviceID {
		t.Fatalf("served after reopen = %d, want the tried card", len(got))
	}
}

func TestWorkingAndRelayAccounting(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	relay := testCard(t, cardOpts{name: "relay", seq: 10, flags: objects.FlagRelay, port: 5000})
	if !st.NoteSession(relay, addrOf(t, relay), testNow) {
		t.Fatal("NoteSession relay")
	}
	if st.Working() != 1 {
		t.Fatalf("Working = %d, want 1", st.Working())
	}
	if !st.HasRelay() {
		t.Fatal("HasRelay = false for a tried relay card")
	}
}

func TestResponseFiltersAndOrder(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	own := testCard(t, cardOpts{name: "own", seq: 10, port: 5100})
	relay := testCard(t, cardOpts{name: "relay", seq: 10, flags: objects.FlagRelay, port: 5101})
	exit := testCard(t, cardOpts{name: "exit", seq: 10, flags: objects.FlagExit, role: objects.RoleServer, region: "NL", port: 5102})
	asker := testCard(t, cardOpts{name: "asker", seq: 10, port: 5103})
	candidate := testCard(t, cardOpts{name: "not-tried", seq: 10, port: 5104})
	for _, c := range []*objects.Card{relay, exit, asker} {
		st.NoteSession(c, addrOf(t, c), testNow)
	}
	st.AddCandidate(candidate, testNow)

	svc, err := New(Config{
		Store:      st,
		SelfNode:   own.NodeID,
		SelfDevice: own.DeviceID,
		OwnCard:    func(time.Time) *objects.Card { return own },
	})
	if err != nil {
		t.Fatalf("pex.New: %v", err)
	}
	peer := Peer{Node: asker.NodeID, Device: asker.DeviceID, Card: asker}

	got, release := svc.Response(peer, Request{Max: 16}, testNow)
	if release != nil {
		t.Fatal("release manifest returned without release = true")
	}
	if len(got) != 3 {
		t.Fatalf("cards = %d, want own + relay + exit", len(got))
	}
	if got[0].DeviceID != own.DeviceID {
		t.Fatal("the node's own card is not first")
	}
	for _, c := range got {
		if c.DeviceID == asker.DeviceID {
			t.Fatal("the asking node's card was returned")
		}
		if c.DeviceID == candidate.DeviceID {
			t.Fatal("a never-reached candidate was returned")
		}
	}

	got, _ = svc.Response(peer, Request{Max: 16, WantFlags: objects.FlagRelay}, testNow)
	if len(got) != 1 || got[0].DeviceID != relay.DeviceID {
		t.Fatalf("want_flags relay returned %d cards", len(got))
	}
	got, _ = svc.Response(peer, Request{Max: 16, WantRole: objects.RoleServer}, testNow)
	if len(got) != 1 || got[0].DeviceID != exit.DeviceID {
		t.Fatalf("want_role server returned %d cards", len(got))
	}
	got, _ = svc.Response(peer, Request{Max: 16, Region: "NL"}, testNow)
	if len(got) != 1 || got[0].DeviceID != exit.DeviceID {
		t.Fatalf("region NL returned %d cards", len(got))
	}
	got, _ = svc.Response(peer, Request{Max: 1}, testNow)
	if len(got) != 1 || got[0].DeviceID != own.DeviceID {
		t.Fatalf("max = 1 returned %d cards", len(got))
	}
}

func TestResponseLimitsSixteen(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	own := testCard(t, cardOpts{name: "own", seq: 10, port: 5200})
	for i := 0; i < 20; i++ {
		c := testCard(t, cardOpts{name: fmt.Sprintf("many-%d", i), seq: 10, port: uint32(5300 + i)})
		if !st.NoteSession(c, addrOf(t, c), testNow) {
			t.Fatalf("NoteSession %d", i)
		}
		_ = c
	}
	svc, err := New(Config{
		Store: st, SelfNode: own.NodeID, SelfDevice: own.DeviceID,
		OwnCard: func(time.Time) *objects.Card { return own },
	})
	if err != nil {
		t.Fatalf("pex.New: %v", err)
	}
	got, _ := svc.Response(Peer{}, Request{Max: 100}, testNow)
	if len(got) != MaxCards {
		t.Fatalf("cards = %d, want %d", len(got), MaxCards)
	}
}

func TestReleaseIsOnlyServedOnRequestAndOnlyWhenAvailable(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	own := testCard(t, cardOpts{name: "own", seq: 10, port: 5400})
	manifest := []byte("release-manifest")
	svc, err := New(Config{
		Store: st, SelfNode: own.NodeID, SelfDevice: own.DeviceID,
		OwnCard: func(time.Time) *objects.Card { return own },
		Release: func() []byte { return manifest },
	})
	if err != nil {
		t.Fatalf("pex.New: %v", err)
	}
	if _, release := svc.Response(Peer{}, Request{Max: 1}, testNow); release != nil {
		t.Fatal("release returned without a request")
	}
	if _, release := svc.Response(Peer{}, Request{Max: 1, Release: true}, testNow); string(release) != string(manifest) {
		t.Fatalf("release = %q, want the manifest", release)
	}

	// Without the T4.5 hook the manifest is never returned, even when asked.
	svc2, err := New(Config{
		Store: st, SelfNode: own.NodeID, SelfDevice: own.DeviceID,
		OwnCard: func(time.Time) *objects.Card { return own },
	})
	if err != nil {
		t.Fatalf("pex.New without release: %v", err)
	}
	if _, release := svc2.Response(Peer{}, Request{Max: 1, Release: true}, testNow); release != nil {
		t.Fatal("release served without a release store (T4.5)")
	}
}

func TestGetRateLimit(t *testing.T) {
	svc, err := New(Config{Store: openTestStore(t, newMemKV())})
	if err != nil {
		t.Fatalf("pex.New: %v", err)
	}
	var sid [16]byte
	sid[0] = 1
	if !svc.Allow(sid, testNow) || !svc.Allow(sid, testNow) {
		t.Fatal("the burst of two GETs was refused")
	}
	if svc.Allow(sid, testNow.Add(time.Second)) {
		t.Fatal("a third GET inside the window was allowed")
	}
	if !svc.Allow(sid, testNow.Add(GetInterval)) {
		t.Fatal("GET after the interval was refused")
	}
	var other [16]byte
	other[0] = 2
	if !svc.Allow(other, testNow) {
		t.Fatal("another session shares the limiter")
	}
}

func TestLazyTrigger(t *testing.T) {
	cases := []struct {
		working  int
		relay    bool
		lowPower bool
		want     bool
	}{
		{0, true, false, true},
		{7, true, false, true},
		{8, true, false, false},
		{8, false, false, true},
		{3, true, true, true},
		{3, false, true, true},
		{4, true, true, false},
		// low_power: "only below 4 peers", regardless of relay (platforms.md §2).
		{10, false, true, false},
	}
	for _, tc := range cases {
		if got := shouldAsk(tc.working, tc.relay, tc.lowPower); got != tc.want {
			t.Fatalf("shouldAsk(%d, %v, %v) = %v, want %v", tc.working, tc.relay, tc.lowPower, got, tc.want)
		}
	}
}

func TestLazyDelayRange(t *testing.T) {
	cases := []struct {
		index int
		want  time.Duration
	}{
		{0, LazyDelayMin},
		{9, LazyDelayMax},
		{4, 5 * time.Second},
	}
	for _, tc := range cases {
		var seen int
		svc, err := New(Config{
			Store: openTestStore(t, newMemKV()),
			Rand: func(n int) int {
				seen = n
				return tc.index
			},
		})
		if err != nil {
			t.Fatalf("pex.New: %v", err)
		}
		if got := svc.delay(); got != tc.want {
			t.Fatalf("delay(index %d) = %s, want %s", tc.index, got, tc.want)
		}
		if seen != 10 {
			t.Fatalf("delay asked for %d buckets, want 10", seen)
		}
	}
}

// TestGetDeadlineUsesSystemClock pins the fix for the 4-second GET timeout: the corrected clock
// (Now) may be off by any offset, but the mux deadline must be built from the system clock, or the
// real wait becomes 4 s + offset (and negative offsets expire immediately).
func TestGetDeadlineUsesSystemClock(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-1000 * time.Second, 0, 1000 * time.Second} {
		offset := offset
		svc, err := New(Config{
			Store:    openTestStore(t, newMemKV()),
			Now:      func() time.Time { return base.Add(offset) },
			Deadline: func() time.Time { return base },
		})
		if err != nil {
			t.Fatalf("pex.New: %v", err)
		}
		if got, want := svc.readDeadline(), base.Add(GetTimeout); !got.Equal(want) {
			t.Fatalf("offset %s: deadline %s, want %s (the corrected clock leaked into the deadline)",
				offset, got, want)
		}
	}

	// The default deadline source is the system clock: about 4 s from now.
	svc, err := New(Config{Store: openTestStore(t, newMemKV())})
	if err != nil {
		t.Fatalf("pex.New: %v", err)
	}
	if d := time.Until(svc.readDeadline()); d < 3*time.Second || d > 5*time.Second {
		t.Fatalf("default GET timeout is %s, want about %s", d, GetTimeout)
	}
}

func TestOpenAppliesTableLimitsAndDeduplicates(t *testing.T) {
	kv := newMemKV()
	st := openTestStore(t, kv)
	for i := 0; i < 3; i++ {
		c := testCard(t, cardOpts{name: fmt.Sprintf("limit-%d", i), seq: 10, port: uint32(6200 + i)})
		if !st.AddCandidate(c, testNow) {
			t.Fatalf("AddCandidate %d", i)
		}
	}
	// Simulate a crash between moving a device: the same device is in both tables. tried must win
	// and the tables must be clipped to their configured limits on load.
	dup := testCard(t, cardOpts{name: "dup", seq: 10, port: 6300})
	e := &entry{card: dup, first: uint64(testNow.Unix()), last: uint64(testNow.Unix())}
	if err := kv.Put(recordKey(keyNew, dup.DeviceID), encodeRecord(e)); err != nil {
		t.Fatalf("put new: %v", err)
	}
	if err := kv.Put(recordKey(keyTried, dup.DeviceID), encodeRecord(e)); err != nil {
		t.Fatalf("put tried: %v", err)
	}

	limited, err := Open(Options{KV: kv, Now: func() time.Time { return testNow }, MaxNew: 2, MaxTried: 1})
	if err != nil {
		t.Fatalf("pex.Open: %v", err)
	}
	if limited.LenNew() != 2 || limited.LenTried() != 1 {
		t.Fatalf("after limits new/tried = %d/%d, want 2/1", limited.LenNew(), limited.LenTried())
	}
	seen := map[[32]byte]bool{}
	for _, c := range limited.DialCandidates(10) {
		if seen[c.DeviceID] {
			t.Fatalf("device %x appears twice in DialCandidates", c.DeviceID)
		}
		seen[c.DeviceID] = true
	}
	if !seen[dup.DeviceID] {
		t.Fatal("the tried copy of a duplicated device was not kept")
	}
}

func TestEarlySafeGet(t *testing.T) {
	if !EarlySafe() {
		t.Fatal("pex GET is not marked 0-RTT safe")
	}
}
