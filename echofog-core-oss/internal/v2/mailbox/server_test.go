package mailbox

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// testClock is a manually advanced clock; the server uses it for queue and item lifetimes and for
// the daily quota window.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Unix(1793664000, 0)} // the vector time
}

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

type fixture struct {
	srv    *Server
	clock  *testClock
	signer *keys.Signer
	node   [32]byte
	self   [32]byte
	hs     *identity.KeyPair
	queue  []byte
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()

	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	t.Cleanup(tw.Destroy)
	st, err := store.Open(store.Options{Dir: t.TempDir(), Key: tw.Account.StorageKey()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hs, err := identity.NewKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	t.Cleanup(hs.Destroy)

	signer, err := keys.NewEd25519Signer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	clock := newTestClock()
	cfg := Config{
		Self:  tw.Account.NodeID(),
		HS:    hs,
		Store: st.Store("v2/mailbox"),
		Now:   clock.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &fixture{
		srv:    srv,
		clock:  clock,
		signer: signer,
		node:   tw.Account.NodeID(),
		self:   tw.Account.NodeID(),
		hs:     hs,
		queue:  bytes.Repeat([]byte{0x90}, QueueSize),
	}
}

func (f *fixture) peer(sid byte) Peer {
	var p Peer
	p.Node = f.node
	p.Device = [32]byte{0xd0, sid}
	p.SID = [16]byte{sid}
	p.Path = mux.PathDirect
	p.Remote = netip.MustParseAddrPort("198.51.100.7:443")
	return p
}

func (f *fixture) authKey() []byte { return f.signer.Public().Key() }

func (f *fixture) create(t *testing.T, p Peer, ttl uint32, public bool, powBits uint32) uint64 {
	t.Helper()
	exp, err := f.srv.CreateQueue(p, QueueCreate{
		Req: 1, Queue: f.queue, AuthKey: f.authKey(), TTL: ttl, Public: public, PowBits: powBits,
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return exp
}

func signFor(t *testing.T, s *keys.Signer, op byte, sid [16]byte, queue []byte, req uint64) []byte {
	t.Helper()
	return s.Sign(AuthInput(op, sid, queue, req))
}

func (f *fixture) fetch(t *testing.T, p Peer, after []byte, max uint32, wait uint32) ([]Item, bool) {
	t.Helper()
	items, more, err := f.srv.Fetch(context.Background(), p, Fetch{
		Req: 2, Queue: f.queue, After: after, Max: max, Wait: wait,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return items, more
}

func (f *fixture) deposit(t *testing.T, p Peer, blob []byte, pow uint64) (uint32, []byte) {
	t.Helper()
	st, id, err := f.srv.Deposit(p, Deposit{Req: 3, Queue: f.queue, Blob: blob, Pow: pow})
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	return st, id
}

func depositStatus(t *testing.T, s *Server, p Peer, queue, blob []byte) uint32 {
	t.Helper()
	st, _, err := s.Deposit(p, Deposit{Req: 1, Queue: queue, Blob: blob})
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	return st
}

// ---- queue lifecycle

func TestQueueCreateDepositFetchAck(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	exp := f.create(t, p, 3600, false, 0)
	if want := uint64(f.clock.Now().Add(time.Hour).Unix()); exp != want {
		t.Fatalf("expires %d, want %d", exp, want)
	}

	blob := []byte("hello mailbox")
	status, id := f.deposit(t, p, blob, 0)
	if status != StatusStored || len(id) != ItemSize {
		t.Fatalf("deposit status %d id %x", status, id)
	}

	items, more := f.fetch(t, p, nil, 10, 0)
	if len(items) != 1 || more {
		t.Fatalf("fetch %d items more=%v", len(items), more)
	}
	if !bytes.Equal(items[0].ID, id) || !bytes.Equal(items[0].Blob, blob) {
		t.Fatalf("item %x %q", items[0].ID, items[0].Blob)
	}
	if items[0].Stored != uint64(f.clock.Now().Unix()) {
		t.Fatalf("stored %d", items[0].Stored)
	}

	if err := f.srv.Ack(p, Ack{
		Req: 4, Queue: f.queue, IDs: [][]byte{id},
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 4),
	}); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if items, _ := f.fetch(t, p, nil, 10, 0); len(items) != 0 {
		t.Fatalf("ack left %d items", len(items))
	}
}

func TestQueueCreateRejectsBadRequests(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)

	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue[:15], AuthKey: f.authKey()}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a 15-byte queue: %v", err)
	}
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue, AuthKey: []byte{1, 2}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a short auth key: %v", err)
	}
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue, AuthKey: f.authKey(), TTL: 30*86400 + 1}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a TTL above 30 days: %v", err)
	}
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue, AuthKey: f.authKey(), Public: true, PowBits: MaxPowBits + 1}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("pow_bits above 26: %v", err)
	}

	exp := f.create(t, p, 0, false, 0)
	if got := exp - uint64(f.clock.Now().Unix()); got != uint64(MaxQueueTTL/time.Second) {
		t.Fatalf("default TTL %d s, want %d", got, MaxQueueTTL/time.Second)
	}
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue, AuthKey: f.authKey()}); !errors.Is(err, ErrQueueExists) {
		t.Fatalf("duplicate queue: %v", err)
	}
}

func TestQueueExpires(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 60, false, 0)
	f.deposit(t, p, []byte("x"), 0)

	f.clock.Advance(61 * time.Second)
	if _, _, err := f.srv.Fetch(context.Background(), p, Fetch{
		Req: 1, Queue: f.queue, Max: 1,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 1),
	}); !errors.Is(err, ErrUnknownQueue) {
		t.Fatalf("an expired queue: %v", err)
	}
}

func TestItemsExpireAfterSevenDays(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	f.deposit(t, p, []byte("old"), 0)

	f.clock.Advance(6 * 24 * time.Hour)
	if items, _ := f.fetch(t, p, nil, 10, 0); len(items) != 1 {
		t.Fatalf("day 6: %d items", len(items))
	}
	f.clock.Advance(2 * 24 * time.Hour)
	if items, _ := f.fetch(t, p, nil, 10, 0); len(items) != 0 {
		t.Fatalf("day 8: %d unacknowledged items survived", len(items))
	}
}

// ---- auth

func TestFetchAuthIsBoundToTheSession(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	f.deposit(t, p, []byte("x"), 0)

	other := f.peer(2) // same node, another session
	_, _, err := f.srv.Fetch(context.Background(), other, Fetch{
		Req: 2, Queue: f.queue, Max: 10,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
	})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("auth from another session: %v", err)
	}
	if _, _, err := f.srv.Fetch(context.Background(), p, Fetch{
		Req: 2, Queue: f.queue, Max: 10,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 3),
	}); !errors.Is(err, ErrAuth) {
		t.Fatalf("auth for another request: %v", err)
	}
	if _, _, err := f.srv.Fetch(context.Background(), p, Fetch{
		Req: 2, Queue: f.queue, Max: 10,
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 2),
	}); !errors.Is(err, ErrAuth) {
		t.Fatalf("an ACK signature opened FETCH: %v", err)
	}
}

func TestAckAuthIsBoundToTheSession(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	_, id := f.deposit(t, p, []byte("x"), 0)

	other := f.peer(2)
	err := f.srv.Ack(other, Ack{
		Req: 3, Queue: f.queue, IDs: [][]byte{id},
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 3),
	})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("ACK auth from another session: %v", err)
	}
}

func TestAuthKeyMayBeAnAkey(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	if _, err := f.srv.CreateQueue(p, QueueCreate{
		Req: 1, Queue: f.queue, AuthKey: f.signer.Public(), // 33-byte akey
	}); err != nil {
		t.Fatalf("akey auth_key: %v", err)
	}
	f.deposit(t, p, []byte("x"), 0)
	if items, _ := f.fetch(t, p, nil, 1, 0); len(items) != 1 {
		t.Fatalf("fetch with an akey auth_key: %d items", len(items))
	}
}

// ---- proof of work and public queues

func invalidPow(t *testing.T, queue, blob []byte, bits uint32) uint64 {
	t.Helper()
	for pow := uint64(1); pow < 1_000_000; pow++ {
		if !ValidPow(queue, blob, pow, bits) {
			return pow
		}
	}
	t.Fatal("no invalid proof found")
	return 0
}

func TestPublicQueueRequiresProofOfWork(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, true, 16)
	blob := []byte("first contact")

	// No proof at all (the field is absent or zero): ask the sender to compute one.
	if status, _ := f.deposit(t, p, blob, 0); status != StatusPowRequired {
		t.Fatalf("without pow: status %d, want %d", status, StatusPowRequired)
	}
	// A wrong proof is a protocol error with the service code.
	bad := invalidPow(t, f.queue, blob, 16)
	if _, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: bad}); !errors.Is(err, ErrPowInvalid) {
		t.Fatalf("wrong pow: %v", err)
	}
	// A valid one is stored.
	var good uint64
	for good = 1; ; good++ {
		if ValidPow(f.queue, blob, good, 16) {
			break
		}
	}
	if status, _ := f.deposit(t, p, blob, good); status != StatusStored {
		t.Fatalf("valid pow: status %d", status)
	}
}

func TestPublicQueueFullDoesNotBlockPrivate(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Limits = &Limits{PublicItems: 2}
	})
	p := f.peer(1)
	pub := bytes.Repeat([]byte{0x11}, QueueSize)
	priv := bytes.Repeat([]byte{0x22}, QueueSize)
	for _, q := range [][]byte{pub, priv} {
		if _, err := f.srv.CreateQueue(p, QueueCreate{
			Req: 1, Queue: q, AuthKey: f.authKey(), Public: bytes.Equal(q, pub),
			PowBits: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	blob := []byte("x")
	var good uint64
	for good = 0; ; good++ {
		if ValidPow(pub, blob, good, 1) {
			break
		}
	}
	for i := 0; i < 2; i++ {
		st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: pub, Blob: blob, Pow: good})
		if err != nil || st != StatusStored {
			t.Fatalf("public deposit %d: status %d err %v", i, st, err)
		}
	}
	if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: pub, Blob: blob, Pow: good}); err != nil || st != StatusFull {
		t.Fatalf("full public queue: status %d err %v", st, err)
	}
	if st := depositStatus(t, f.srv, p, priv, blob); st != StatusStored {
		t.Fatalf("private queue after a public flood: status %d", st)
	}
}

func TestPrivateQueueLimits(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Limits = &Limits{PrivateItems: 2} })
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	for i := 0; i < 2; i++ {
		if st, _ := f.deposit(t, p, []byte{byte(i)}, 0); st != StatusStored {
			t.Fatalf("deposit %d: %d", i, st)
		}
	}
	if st, _ := f.deposit(t, p, []byte{3}, 0); st != StatusFull {
		t.Fatalf("queue over its item limit: %d", st)
	}
}

func TestDepositRejections(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	if st, _ := f.deposit(t, p, []byte("x"), 0); st != StatusUnknownQueue {
		t.Fatalf("deposit to an unknown queue: %d", st)
	}
	f.create(t, p, 0, false, 0)
	if st, _ := f.deposit(t, p, bytes.Repeat([]byte{1}, MaxBlob+1), 0); st != StatusTooLarge {
		t.Fatalf("oversized blob: %d", st)
	}
}

// ---- quotas

func TestDailyQuotaIsNotResetByANewSession(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Limits = &Limits{QuotaItemsPerDay: 2}
	})
	first := f.peer(1)
	f.create(t, first, 0, false, 0)

	for i := 0; i < 2; i++ {
		if st, _ := f.deposit(t, first, []byte{byte(i)}, 0); st != StatusStored {
			t.Fatalf("deposit %d: %d", i, st)
		}
	}
	second := f.peer(2) // same node ID, a fresh session
	if st, _ := f.deposit(t, second, []byte{9}, 0); st != StatusRefused {
		t.Fatalf("a new session reset the daily quota: status %d", st)
	}
	f.clock.Advance(24 * time.Hour)
	if st, _ := f.deposit(t, second, []byte{10}, 0); st != StatusStored {
		t.Fatalf("the next day: status %d", st)
	}
}

func TestPerNodeQuotaCountsTheForwarder(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Limits = &Limits{QuotaItemsPerDay: 1}
	})
	owner := f.peer(1)
	f.create(t, owner, 0, false, 0)

	eph, err := identity.NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eph.Destroy)
	body := Deposit{Req: 5, Queue: f.queue, Blob: []byte("first")}.Append(nil)
	sealed, err := SealForward(privateOf(t, eph), f.hs.Public().Key(), body)
	if err != nil {
		t.Fatal(err)
	}

	forwarder := f.peer(9) // the depositing node id is the forwarder, not the hidden sender
	if st, _ := f.srv.Forward(context.Background(), forwarder, Forward{Req: 1, Relay: f.self[:], Sealed: sealed}); st != StatusStored {
		t.Fatalf("first forward: %d", st)
	}

	body2 := Deposit{Req: 6, Queue: f.queue, Blob: []byte("second")}.Append(nil)
	sealed2, err := SealForward(privateOf(t, eph), f.hs.Public().Key(), body2)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := f.srv.Forward(context.Background(), forwarder, Forward{Req: 2, Relay: f.self[:], Sealed: sealed2}); st != StatusRefused {
		t.Fatalf("the forwarder exceeded its daily quota: %d", st)
	}
}

// ---- metered and forward

func TestMeteredServerStoresNothing(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Metered = true })
	p := f.peer(1)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue, AuthKey: f.authKey()}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a metered server created a queue: %v", err)
	}

	// A FORWARD is the heavy path; it is refused even if the queue already existed from before
	// the profile became metered.
	eph, err := identity.NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eph.Destroy)
	body := Deposit{Req: 1, Queue: f.queue, Blob: []byte("x")}.Append(nil)
	sealed, err := SealForward(privateOf(t, eph), f.hs.Public().Key(), body)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := f.srv.Forward(context.Background(), p, Forward{Req: 1, Relay: f.self[:], Sealed: sealed}); st != StatusRefused {
		t.Fatalf("a metered server forwarded a deposit: %d", st)
	}
}

func TestForwardRequiresRelaySelf(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)

	body := Deposit{Req: 1, Queue: f.queue, Blob: []byte("x")}.Append(nil)
	eph, err := identity.NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eph.Destroy)
	sealed, err := SealForward(privateOf(t, eph), f.hs.Public().Key(), body)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.srv.Forward(context.Background(), p, Forward{Req: 1, Relay: bytes.Repeat([]byte{0xff}, 32), Sealed: sealed})
	if st != StatusRelayUnreachable {
		t.Fatalf("relay != self: status %d", st)
	}
}

func TestForwardReplayIsStoredOnce(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)

	eph, err := identity.NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eph.Destroy)
	body := Deposit{Req: 5, Queue: f.queue, Blob: []byte("once")}.Append(nil)
	sealed, err := SealForward(privateOf(t, eph), f.hs.Public().Key(), body)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		st, _ := f.srv.Forward(context.Background(), p, Forward{Req: uint64(i + 1), Relay: f.self[:], Sealed: sealed})
		if st != StatusStored {
			t.Fatalf("forward %d: status %d", i, st)
		}
	}
	items, _ := f.fetch(t, p, nil, 10, 0)
	if len(items) != 1 {
		t.Fatalf("a replayed sealed deposit was stored %d times", len(items))
	}
}

func TestForwardBadSealed(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	if st, _ := f.srv.Forward(context.Background(), p, Forward{
		Req: 1, Relay: f.self[:], Sealed: bytes.Repeat([]byte{1}, 48),
	}); st != StatusRefused {
		t.Fatalf("a sealed body that does not open: status %d", st)
	}
}

// ---- long poll and paging

func TestFetchLongPollWakesOnDeposit(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)

	done := make(chan []Item, 1)
	go func() {
		items, _, err := f.srv.Fetch(context.Background(), p, Fetch{
			Req: 2, Queue: f.queue, Max: 10, Wait: 5,
			Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
		})
		if err != nil {
			t.Errorf("Fetch: %v", err)
		}
		done <- items
	}()

	f.deposit(t, p, []byte("wake"), 0)

	select {
	case items := <-done:
		if len(items) != 1 || string(items[0].Blob) != "wake" {
			t.Fatalf("long poll returned %d items", len(items))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a waiting FETCH did not wake on deposit")
	}
}

func TestFetchPageFitsTheFrameMaximum(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	blob := bytes.Repeat([]byte{0xab}, 32768)
	for i := 0; i < 40; i++ { // 40 * 32 KiB = 1.25 MiB
		if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob}); err != nil || st != StatusStored {
			t.Fatalf("deposit %d: %d %v", i, st, err)
		}
	}
	items, more, err := f.srv.Fetch(context.Background(), p, Fetch{
		Req: 2, Queue: f.queue, Max: 100,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !more {
		t.Fatal("a 1.25 MiB queue was returned in one page without more")
	}
	enc := Items{Req: 2, Items: items, More: more}.Append(nil)
	if len(enc) > MaxItemsBytes {
		t.Fatalf("ITEMS payload is %d bytes, maximum is %d", len(enc), MaxItemsBytes)
	}
	if len(items) == 0 {
		t.Fatal("a non-empty queue returned an empty page")
	}
}

func TestFetchPaging(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	for i := 0; i < 3; i++ {
		f.deposit(t, p, []byte{byte('a' + i)}, 0)
	}
	first, more := f.fetch(t, p, nil, 2, 0)
	if len(first) != 2 || !more {
		t.Fatalf("page 1: %d items more=%v", len(first), more)
	}
	second, more := f.fetch(t, p, first[len(first)-1].ID, 2, 0)
	if len(second) != 1 || more {
		t.Fatalf("page 2: %d items more=%v", len(second), more)
	}
}

func TestConcurrentDepositsAndFetches(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, _ := f.deposit(t, p, []byte{byte(i)}, 0)
			if st != StatusStored {
				t.Errorf("deposit %d: status %d", i, st)
			}
		}(i)
	}
	wg.Wait()

	items, _ := f.fetch(t, p, nil, 100, 0)
	if len(items) != 8 {
		t.Fatalf("%d of 8 deposits stored", len(items))
	}
}

func privateOf(t *testing.T, kp *identity.KeyPair) []byte {
	t.Helper()
	var out []byte
	if err := kp.WithPrivate(func(b []byte) error { out = append([]byte(nil), b...); return nil }); err != nil {
		t.Fatalf("private key: %v", err)
	}
	return out
}
