package mailbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
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
	cfg    Config
	kv     store.KV
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

	t.Cleanup(func() {
		if n := srv.UnderflowErrors(); n != 0 {
			t.Errorf("a mailbox counter was asked to go below zero %d times", n)
		}
	})

	return &fixture{
		srv:    srv,
		clock:  clock,
		signer: signer,
		node:   tw.Account.NodeID(),
		self:   tw.Account.NodeID(),
		hs:     hs,
		queue:  bytes.Repeat([]byte{0x90}, QueueSize),
		cfg:    cfg,
		kv:     cfg.Store,
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

// otherPeer is a peer of another account: the queue was not created by it, so it is external to
// every queue f.peer creates.
func (f *fixture) otherPeer(sid byte) Peer {
	p := f.peer(sid)
	p.Node = [32]byte{0xe0 + sid}
	return p
}

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
	exp := f.create(t, p, 1, false, 0)
	if want := uint64(f.clock.Now().Add(24 * time.Hour).Unix()); exp != want {
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
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: f.queue, AuthKey: f.authKey(), TTL: 31}); !errors.Is(err, ErrBadRequest) {
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

func TestQueuesPerNodeLimit(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Limits = &Limits{QueuesPerNode: 1} })
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	other := bytes.Repeat([]byte{0x77}, QueueSize)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: other, AuthKey: f.authKey()}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a second queue for one node: %v", err)
	}
	// Another node is unaffected.
	q := f.peer(2)
	q.Node[0] ^= 0xff
	if _, err := f.srv.CreateQueue(q, QueueCreate{Req: 1, Queue: other, AuthKey: f.authKey()}); err != nil {
		t.Fatalf("another node: %v", err)
	}
}

func TestQueueExpires(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 1, false, 0)
	f.deposit(t, p, []byte("x"), 0)

	f.clock.Advance(25 * time.Hour)
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

func goodPow(t *testing.T, queue, blob []byte, bits uint32) uint64 {
	t.Helper()
	for pow := uint64(0); ; pow++ {
		if ValidPow(queue, blob, pow, bits) {
			return pow
		}
	}
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
	// The owner is on the authenticated path: a wrong proof is the service error 0x100, not the
	// indistinguishable refusal that an external sender gets (services.md 5.1, AD20).
	bad := invalidPow(t, f.queue, blob, 16)
	if _, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: bad}); !errors.Is(err, ErrPowInvalid) {
		t.Fatalf("wrong pow of the owner: err %v, want %v", err, ErrPowInvalid)
	}
	// A valid one is stored.
	if status, _ := f.deposit(t, p, blob, goodPow(t, f.queue, blob, 16)); status != StatusStored {
		t.Fatalf("valid pow: status %d", status)
	}
}

// TestDepositRefusalDependsOnQueueOwnership covers the AD20 table of services.md 5.1: an external
// sender (not rec.Owner) sees one refused for "no such queue", "full", "no proof" and "bad proof";
// the owner keeps the real codes and ERROR{0x100}. Status 3 (too-large) is the only exception.
func TestDepositRefusalDependsOnQueueOwnership(t *testing.T) {
	t.Run("public", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.Limits = &Limits{PublicItems: 2} })
		owner, external := f.peer(1), f.otherPeer(2)
		f.create(t, owner, 0, true, 1)

		// No queue record means no owner can be authenticated: refused to both.
		unknown := bytes.Repeat([]byte{0x7a}, QueueSize)
		for _, p := range []Peer{external, owner} {
			if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: unknown, Blob: []byte("x")}); err != nil || st != StatusRefused {
				t.Fatalf("unknown queue for node %x: status %d err %v, want refused", p.Node[:2], st, err)
			}
		}

		blob := []byte("first contact")
		// Missing proof: the owner is told to compute one, the external sender only sees refused.
		if st, _, err := f.srv.Deposit(external, Deposit{Req: 1, Queue: f.queue, Blob: blob}); err != nil || st != StatusRefused {
			t.Fatalf("external without pow: status %d err %v, want refused", st, err)
		}
		if st, _, err := f.srv.Deposit(owner, Deposit{Req: 1, Queue: f.queue, Blob: blob}); err != nil || st != StatusPowRequired {
			t.Fatalf("owner without pow: status %d err %v, want %d", st, err, StatusPowRequired)
		}

		// Bad proof: the service code only on the authenticated path (handler.go maps it to 0x100).
		bad := invalidPow(t, f.queue, blob, 1)
		if st, _, err := f.srv.Deposit(external, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: bad}); err != nil || st != StatusRefused {
			t.Fatalf("external with a bad pow: status %d err %v, want refused", st, err)
		}
		if _, _, err := f.srv.Deposit(owner, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: bad}); !errors.Is(err, ErrPowInvalid) {
			t.Fatalf("owner with a bad pow: err %v, want %v", err, ErrPowInvalid)
		}

		// Success fills the two slots, one by each sender.
		for _, tc := range []struct {
			name string
			p    Peer
			blob []byte
		}{{"owner", owner, []byte("owner work")}, {"external", external, []byte("external work")}} {
			if st, _, err := f.srv.Deposit(tc.p, Deposit{Req: 1, Queue: f.queue, Blob: tc.blob, Pow: goodPow(t, f.queue, tc.blob, 1)}); err != nil || st != StatusStored {
				t.Fatalf("%s deposit: status %d err %v, want stored", tc.name, st, err)
			}
		}

		// Full: the owner keeps 2, the external sender still sees 4.
		ownerBlob := []byte("owner over the limit")
		if st, _, err := f.srv.Deposit(owner, Deposit{Req: 1, Queue: f.queue, Blob: ownerBlob, Pow: goodPow(t, f.queue, ownerBlob, 1)}); err != nil || st != StatusFull {
			t.Fatalf("full queue for the owner: status %d err %v, want %d", st, err, StatusFull)
		}
		externalBlob := []byte("external over the limit")
		if st, _, err := f.srv.Deposit(external, Deposit{Req: 1, Queue: f.queue, Blob: externalBlob, Pow: goodPow(t, f.queue, externalBlob, 1)}); err != nil || st != StatusRefused {
			t.Fatalf("full queue for the external sender: status %d err %v, want refused", st, err)
		}
	})

	t.Run("private", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.Limits = &Limits{PrivateItems: 1} })
		owner, external := f.peer(1), f.otherPeer(2)
		f.create(t, owner, 0, false, 0)
		if st, _ := f.deposit(t, owner, []byte("the one"), 0); st != StatusStored {
			t.Fatalf("owner deposit: status %d", st)
		}
		if st, _ := f.deposit(t, owner, []byte("over the limit"), 0); st != StatusFull {
			t.Fatalf("full queue for the owner: status %d, want %d", st, StatusFull)
		}
		if st, _, err := f.srv.Deposit(external, Deposit{Req: 1, Queue: f.queue, Blob: []byte("over the limit")}); err != nil || st != StatusRefused {
			t.Fatalf("full queue for the external sender: status %d err %v, want refused", st, err)
		}
	})
}

// One proof of work buys one public queue slot: a repeat of the same blob is answered stored and adds
// no second item (services.md 5.2).
func TestPublicQueueDeduplicatesADirectDeposit(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, true, 1)
	blob := []byte("one work")
	pow := goodPow(t, f.queue, blob, 1)

	st, id, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: pow})
	if err != nil || st != StatusStored || len(id) != ItemSize {
		t.Fatalf("first deposit: status %d item %x err %v", st, id, err)
	}
	for i := 0; i < 99; i++ { // one work, a hundred more deposits of the same blob
		st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: pow})
		if err != nil || st != StatusStored {
			t.Fatalf("repeat %d: status %d err %v, want stored", i, st, err)
		}
	}
	if items, _ := f.fetch(t, p, nil, 10, 0); len(items) != 1 {
		t.Fatalf("the public queue holds %d items after 100 deposits of one blob, want 1", len(items))
	}

	// a different blob takes another slot
	other := []byte("another work")
	if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: other, Pow: goodPow(t, f.queue, other, 1)}); err != nil || st != StatusStored {
		t.Fatalf("a different blob: status %d err %v", st, err)
	}
	if items, _ := f.fetch(t, p, nil, 10, 0); len(items) != 2 {
		t.Fatalf("the public queue holds %d items, want 2", len(items))
	}

	// the memory of a deposit is bound to its queue
	second := bytes.Repeat([]byte{0x91}, QueueSize)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: second, AuthKey: f.authKey(), Public: true, PowBits: 1}); err != nil {
		t.Fatal(err)
	}
	if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: second, Blob: blob, Pow: goodPow(t, second, blob, 1)}); err != nil || st != StatusStored {
		t.Fatalf("the same blob in another queue: status %d err %v", st, err)
	}
	items, _, err := f.srv.Fetch(context.Background(), p, Fetch{Req: 2, Queue: second, Max: 10,
		Auth: signFor(t, f.signer, OpFetch, p.SID, second, 2)})
	if err != nil || len(items) != 1 {
		t.Fatalf("the second queue holds %d items: %v", len(items), err)
	}

	// the memory lives as long as the item: after its lifetime a repeat is a new deposit
	f.clock.Advance(8 * 24 * time.Hour)
	if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: pow}); err != nil || st != StatusStored {
		t.Fatalf("after the item lifetime: status %d err %v", st, err)
	}
	if items, _ := f.fetch(t, p, nil, 10, 0); len(items) != 1 {
		t.Fatalf("the public queue holds %d items after the lifetime, want the one new deposit", len(items))
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
	for i := 0; i < 2; i++ {
		blob := []byte{byte('x' + i)}
		st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: pub, Blob: blob, Pow: goodPow(t, pub, blob, 1)})
		if err != nil || st != StatusStored {
			t.Fatalf("public deposit %d: status %d err %v", i, st, err)
		}
	}
	blob := []byte("over the limit")
	if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: pub, Blob: blob, Pow: goodPow(t, pub, blob, 1)}); err != nil || st != StatusFull {
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
	// Without a queue record there is no owner to authenticate, so even the creator of the queue
	// sees the external refusal: it must not become an oracle for whether a queue exists.
	if st, _ := f.deposit(t, p, []byte("x"), 0); st != StatusRefused {
		t.Fatalf("deposit to an unknown queue: %d, want refused", st)
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
	// F is the external sender's view: its own relay-unreachable (5) is the same refusal as every
	// other failure of the forwarding path (services.md 5.3, AD20).
	if st != StatusRefused {
		t.Fatalf("relay != self: status %d, want refused", st)
	}
}

type forwarderFake struct {
	connected bool
	status    uint32
	err       error
}

func (f *forwarderFake) Connected([]byte) bool { return f.connected }

func (f *forwarderFake) Forward(context.Context, Forward) (uint32, error) {
	return f.status, f.err
}

// F passes on what M answered, but the caller is the external sender: only stored and too-large keep
// their meaning; 1, 2, 4, 5 and 6 all become refused (services.md 5.3, AD20).
func TestForwarderMapsTheExternalRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status uint32
		err    error
		want   uint32
	}{
		{"stored", StatusStored, nil, StatusStored},
		{"too-large", StatusTooLarge, nil, StatusTooLarge},
		{"unknown-queue", StatusUnknownQueue, nil, StatusRefused},
		{"full", StatusFull, nil, StatusRefused},
		{"relay-unreachable", StatusRelayUnreachable, nil, StatusRefused},
		{"pow-required", StatusPowRequired, nil, StatusRefused},
		{"refused", StatusRefused, nil, StatusRefused},
		{"dial error", 0, errors.New("no route"), StatusRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(c *Config) {
				c.Forwarder = &forwarderFake{connected: true, status: tc.status, err: tc.err}
			})
			p := f.peer(1)
			st, err := f.srv.Forward(context.Background(), p, Forward{
				Req: 1, Relay: bytes.Repeat([]byte{0xaa}, 32), Sealed: []byte("sealed body"),
			})
			if err != nil || st != tc.want {
				t.Fatalf("status %d err %v, want %d", st, err, tc.want)
			}
		})
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

// ---- T7.3 fix 1: expiry, units, defaults and resources ----------------------

// countingKV counts every value byte a scan materialises, so a test can prove FETCH does not read
// the whole queue before applying the page limit.
type countingKV struct {
	store.KV
	scanned int64
}

func (c *countingKV) Scan(prefix []byte, limit int) ([]store.Entry, error) {
	entries, err := c.KV.Scan(prefix, limit)
	for _, e := range entries {
		c.scanned += int64(len(e.Value))
	}
	return entries, err
}

func (c *countingKV) ScanFrom(prefix, start []byte, limit int) ([]store.Entry, error) {
	entries, err := c.KV.ScanFrom(prefix, start, limit)
	for _, e := range entries {
		c.scanned += int64(len(e.Value))
	}
	return entries, err
}

// TestExpiredItemsReleaseQueueAndTotalsOnRealTime uses the real clock: the store must not be the
// thing that reclaims quota. After every item is past ItemTTL, the queue counters and the relay
// totals must both be free again.
func TestExpiredItemsReleaseQueueAndTotalsOnRealTime(t *testing.T) {
	const itemTTL = 80 * time.Millisecond
	const pause = 250 * time.Millisecond

	t.Run("queue", func(t *testing.T) {
		f := newFixture(t, func(c *Config) {
			c.Now = time.Now
			c.Limits = &Limits{PrivateItems: 1, ItemTTL: itemTTL}
		})
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		if st, _ := f.deposit(t, p, []byte("old"), 0); st != StatusStored {
			t.Fatalf("first deposit: status %d", st)
		}
		time.Sleep(pause)
		if st, _ := f.deposit(t, p, []byte("new"), 0); st != StatusStored {
			t.Fatalf("a queue held its quota after every item expired: status %d", st)
		}
	})

	t.Run("totals", func(t *testing.T) {
		f := newFixture(t, func(c *Config) {
			c.Now = time.Now
			c.Limits = &Limits{TotalItems: 1, ItemTTL: itemTTL}
		})
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		if st, _ := f.deposit(t, p, []byte("old"), 0); st != StatusStored {
			t.Fatalf("first deposit: status %d", st)
		}

		other := bytes.Repeat([]byte{0x91}, QueueSize)
		if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: other, AuthKey: f.authKey()}); err != nil {
			t.Fatalf("second queue: %v", err)
		}
		time.Sleep(pause)
		if st := depositStatus(t, f.srv, p, other, []byte("new")); st != StatusStored {
			t.Fatalf("relay totals held an expired item and blocked a second queue: status %d", st)
		}
	})
}

// TestQueueTTLIsInDays pins the unit decision: ttl is days, 0 means 30, 31 is refused.
func TestQueueTTLIsInDays(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	cases := []struct {
		name    string
		ttl     uint32
		days    int
		wantErr bool
	}{
		{"zero is thirty days", 0, 30, false},
		{"one day", 1, 1, false},
		{"thirty days", 30, 30, false},
		{"above the maximum", 31, 0, true},
		{"uint32 maximum", math.MaxUint32, 0, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := bytes.Repeat([]byte{byte(0xa0 + i)}, QueueSize)
			exp, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: q, AuthKey: f.authKey(), TTL: tc.ttl})
			if tc.wantErr {
				if !errors.Is(err, ErrBadRequest) {
					t.Fatalf("ttl %d: %v, want ErrBadRequest", tc.ttl, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ttl %d: %v", tc.ttl, err)
			}
			want := uint64(f.clock.Now().Add(time.Duration(tc.days) * 24 * time.Hour).Unix())
			if exp != want {
				t.Fatalf("ttl %d expires %d, want %d (%d days)", tc.ttl, exp, want, tc.days)
			}
		})
	}
}

func TestFetchMaxZeroReturnsTheDefaultPage(t *testing.T) {
	t.Run("many items", func(t *testing.T) {
		f := newFixture(t, nil)
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		for i := 0; i < 40; i++ {
			if st, _ := f.deposit(t, p, []byte{byte(i)}, 0); st != StatusStored {
				t.Fatalf("deposit %d: %d", i, st)
			}
		}
		items, more := f.fetch(t, p, nil, 0, 0)
		if len(items) != 32 || !more {
			t.Fatalf("max=0 on 40 items: %d items more=%v, want 32/true", len(items), more)
		}
	})

	t.Run("few items", func(t *testing.T) {
		f := newFixture(t, nil)
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		for i := 0; i < 5; i++ {
			if st, _ := f.deposit(t, p, []byte{byte(i)}, 0); st != StatusStored {
				t.Fatalf("deposit %d: %d", i, st)
			}
		}
		items, more := f.fetch(t, p, nil, 0, 0)
		if len(items) != 5 || more {
			t.Fatalf("max=0 on 5 items: %d items more=%v, want 5/false", len(items), more)
		}
	})

	t.Run("bounded by ITEMS", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.Limits = &Limits{FetchItems: 3} })
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		for i := 0; i < 5; i++ {
			if st, _ := f.deposit(t, p, []byte{byte(i)}, 0); st != StatusStored {
				t.Fatalf("deposit %d: %d", i, st)
			}
		}
		items, _ := f.fetch(t, p, nil, 0, 0)
		if len(items) != 3 {
			t.Fatalf("max=0 with FetchItems=3: %d items", len(items))
		}
	})
}

// TestMeteredRefusesOnlyDeposits keeps a metered node useful: it still drains what it already
// accepted and can create queues, but it does not store new deposits.
func TestMeteredRefusesOnlyDeposits(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	_, id := f.deposit(t, p, []byte("accepted before metering"), 0)

	f.srv.cfg.Metered = true

	if st := depositStatus(t, f.srv, p, f.queue, []byte("new")); st != StatusRefused {
		t.Fatalf("metered DEPOSIT: status %d, want refused", st)
	}
	other := bytes.Repeat([]byte{0x92}, QueueSize)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: other, AuthKey: f.authKey()}); err != nil {
		t.Fatalf("metered QUEUE_CREATE: %v", err)
	}
	items, _ := f.fetch(t, p, nil, 10, 0)
	if len(items) != 1 {
		t.Fatalf("metered FETCH returned %d items, want the stored one", len(items))
	}
	if err := f.srv.Ack(p, Ack{
		Req: 4, Queue: f.queue, IDs: [][]byte{id},
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 4),
	}); err != nil {
		t.Fatalf("metered ACK: %v", err)
	}
}

// TestQueueRecordKeepsNoRawAuthKey proves the store holds a verifier/binding, not the key bytes,
// and that a restarted relay can still verify FETCH auth.
func TestQueueRecordKeepsNoRawAuthKey(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	if _, id := f.deposit(t, p, []byte("after restart"), 0); len(id) != ItemSize {
		t.Fatalf("deposit id %x", id)
	}

	raw, err := f.kv.Get(queueKey(f.queue))
	if err != nil {
		t.Fatalf("queue record: %v", err)
	}
	if bytes.Contains(raw, f.authKey()) {
		t.Fatal("the queue record contains the raw auth_key")
	}

	restarted, err := New(f.cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	items, _, err := restarted.Fetch(context.Background(), p, Fetch{
		Req: 2, Queue: f.queue, Max: 10,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("FETCH after restart: %d items, %v", len(items), err)
	}
}

// TestFetchReadsOnlyThePageBudget makes one small FETCH on a queue of large items; the store must
// not materialise the whole queue before the 1 MiB page limit applies.
func TestFetchReadsOnlyThePageBudget(t *testing.T) {
	counter := &countingKV{}
	f := newFixture(t, func(c *Config) {
		counter.KV = c.Store
		c.Store = counter
	})
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	blob := bytes.Repeat([]byte{0xab}, MaxBlob)
	for i := 0; i < 100; i++ {
		if st, _, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob}); err != nil || st != StatusStored {
			t.Fatalf("deposit %d: %d %v", i, st, err)
		}
	}

	counter.scanned = 0
	items, _, err := f.srv.Fetch(context.Background(), p, Fetch{
		Req: 2, Queue: f.queue, Max: 500,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("a non-empty queue returned an empty page")
	}
	// Page (<=1 MiB) plus one look-ahead chunk of at most 8 items.
	const budget = 2 << 20
	if counter.scanned > budget {
		t.Fatalf("FETCH scanned %d bytes, budget %d", counter.scanned, budget)
	}
}

// timerClock jumps backwards once, which is what a corrected clock can do while a FETCH waits.
type timerClock struct {
	mu        sync.Mutex
	base      time.Time
	calls     int
	jumpAfter int
	back      time.Duration
}

func (c *timerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.jumpAfter > 0 && c.calls > c.jumpAfter {
		return c.base.Add(-c.back)
	}
	return c.base
}

func (c *timerClock) reset() {
	c.mu.Lock()
	c.calls = 0
	c.mu.Unlock()
}

// TestFetchTimerWindowIsCapped checks the timer branch added with the long-poll guard: even when
// the corrected clock moves backwards, one timer is never longer than FetchWait.
func TestFetchTimerWindowIsCapped(t *testing.T) {
	clk := &timerClock{
		base:      time.Unix(1793664000, 0),
		jumpAfter: 6,
		back:      2 * MaxWait,
	}
	fired := make(chan time.Duration, 1)
	block := make(chan time.Time)
	f := newFixture(t, func(c *Config) {
		c.Now = clk.Now
		c.After = func(d time.Duration) <-chan time.Time {
			fired <- d
			return block
		}
	})
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	clk.reset()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := f.srv.Fetch(ctx, p, Fetch{
			Req: 2, Queue: f.queue, Max: 10, Wait: 3600,
			Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
		})
		done <- err
	}()

	select {
	case d := <-fired:
		if d > f.srv.Limits().FetchWait {
			t.Fatalf("timer window %s exceeds FetchWait %s", d, f.srv.Limits().FetchWait)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FETCH never started its timer")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled FETCH: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled FETCH did not return")
	}
}

// TestFetchWaiterCaps checks both long-poll limits: per queue and per relay.
func TestFetchWaiterCaps(t *testing.T) {
	t.Run("per queue", func(t *testing.T) {
		f := newFixture(t, nil)
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		for i := 0; i < MaxWaitersPerKey; i++ {
			if _, ok := f.srv.addWaiter(string(f.queue)); !ok {
				t.Fatalf("waiter %d of %d was refused", i, MaxWaitersPerKey)
			}
		}
		start := time.Now()
		items, more, err := f.srv.Fetch(context.Background(), p, Fetch{
			Req: 2, Queue: f.queue, Max: 10, Wait: 30,
			Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
		})
		if err != nil || len(items) != 0 || more {
			t.Fatalf("over the waiter cap: %d items more=%v err=%v", len(items), more, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("FETCH blocked for %s over the waiter cap", elapsed)
		}
	})

	t.Run("relay", func(t *testing.T) {
		f := newFixture(t, nil)
		p := f.peer(1)
		f.create(t, p, 0, false, 0)
		for i := 0; i < MaxWaiters; i++ {
			if _, ok := f.srv.addWaiter(fmt.Sprintf("other/%d", i)); !ok {
				t.Fatalf("global waiter %d of %d was refused", i, MaxWaiters)
			}
		}
		start := time.Now()
		items, _, err := f.srv.Fetch(context.Background(), p, Fetch{
			Req: 2, Queue: f.queue, Max: 10, Wait: 30,
			Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 2),
		})
		if err != nil || len(items) != 0 {
			t.Fatalf("over the global waiter cap: %d items err=%v", len(items), err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("FETCH blocked for %s over the global waiter cap", elapsed)
		}
	})
}

// TestDiversityUnitPrefixes pins the quota unit: /24 for IPv4 and /32 for IPv6.
func TestDiversityUnitPrefixes(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"203.0.113.7:443", "203.0.113.0/24"},
		{"198.51.100.200:443", "198.51.100.0/24"},
		{"[2001:db8:1::5]:443", "2001:db8::/32"},
		{"[2001:db9::5]:443", "2001:db9::/32"},
	}
	for _, tc := range cases {
		p := Peer{Path: mux.PathDirect, Remote: netip.MustParseAddrPort(tc.addr)}
		if got := diversityUnit(p); got != tc.want {
			t.Fatalf("%s: unit %q, want %q", tc.addr, got, tc.want)
		}
	}
	if got := diversityUnit(Peer{Path: mux.PathRelay, Remote: netip.MustParseAddrPort("203.0.113.7:443")}); got != "" {
		t.Fatalf("a relayed path has a diversity unit: %q", got)
	}
}

// TestUnitQuotaUsesTheDiversityPrefix checks that two different nodes on one /24 (IPv4) or /32
// (IPv6) share the unit quota while another unit is unaffected.
func TestUnitQuotaUsesTheDiversityPrefix(t *testing.T) {
	cases := []struct {
		name      string
		first     string
		sameUnit  string
		otherUnit string
	}{
		{"ipv4 /24", "203.0.113.10:443", "203.0.113.20:443", "203.0.114.10:443"},
		{"ipv6 /32", "[2001:db8::10]:443", "[2001:db8::20]:443", "[2001:db9::10]:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(c *Config) {
				c.Limits = &Limits{QuotaItemsPerDay: 1, QuotaFactor: 1}
			})
			owner := f.peer(1)
			f.create(t, owner, 0, false, 0)

			peerAt := func(n byte, addr string) Peer {
				p := f.peer(n)
				p.Node[0] ^= n
				p.Remote = netip.MustParseAddrPort(addr)
				return p
			}
			first := peerAt(2, tc.first)
			same := peerAt(3, tc.sameUnit)
			other := peerAt(4, tc.otherUnit)

			if st := depositStatus(t, f.srv, first, f.queue, []byte("a")); st != StatusStored {
				t.Fatalf("first unit deposit: %d", st)
			}
			if st := depositStatus(t, f.srv, same, f.queue, []byte("b")); st != StatusRefused {
				t.Fatalf("another node in the same unit: %d, want refused", st)
			}
			if st := depositStatus(t, f.srv, other, f.queue, []byte("c")); st != StatusStored {
				t.Fatalf("another unit: %d", st)
			}
		})
	}
}
