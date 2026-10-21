package routing

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The CLI, the config defaults and the relay itself agree on how many circuits a relay carries.
func TestRelayLimits_MaxCircuitsDefaultsAgree(t *testing.T) {
	if got := config.DefaultConfig("t").Relay.Server.MaxCircuits; got != config.DefaultRelayMaxCircuits {
		t.Fatalf("config default %d, want %d", got, config.DefaultRelayMaxCircuits)
	}
	if got := NewRelayServer(nil, nil, nil).GetPolicy().MaxCircuits; got != config.DefaultRelayMaxCircuits {
		t.Fatalf("relay default %d, want %d", got, config.DefaultRelayMaxCircuits)
	}
	if got := limitsOf(config.RelayServerConfig{}).maxCircuits; got != config.DefaultRelayMaxCircuits {
		t.Fatalf("an unset MaxCircuits means %d, want %d", got, config.DefaultRelayMaxCircuits)
	}
	if got := limitsOf(config.RelayServerConfig{MaxCircuits: -1, PeerMaxMbps: -1}); got.maxCircuits != 0 || got.peerRate != 0 {
		t.Fatalf("a negative value means no limit, got %+v", got)
	}
}

// Many identities from one IP share one cap; a peer elsewhere in the same /24 still gets in until
// the subnet cap, and one from another network is not affected.
func TestRelayServer_PerIPCircuitCap(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	rs.SetPolicy(config.RelayServerConfig{Enabled: true, MaxCircuitsPerIP: 4})
	addrs := map[types.PeerID]string{}
	rs.addrOf = func(id types.PeerID) string { return addrs[id] }
	_, lim := rs.policy()

	rs.circuitsMu.Lock()
	for i := 0; i < 4; i++ {
		id := mkID(byte(0x10 + i)) // a fresh identity every time, one address
		addrs[id] = "203.0.113.5:4000"
		rs.circuits[uint32(i+1)] = &Circuit{ID: uint32(i + 1), PeerA: id, PeerB: mkID(0xee), ipKey: "203.0.113.5", netKey: "203.0.113.0/24"}
	}
	rs.circuitsMu.Unlock()

	sybil := mkID(0x20)
	addrs[sybil] = "203.0.113.5:4001"
	target := mkID(0x0b)
	rs.handleCircuitOpenReq(target[:], sybil)
	if rs.limitRefused.Load() != 1 {
		t.Fatal("a fifth identity from the same IP must be refused")
	}

	ip, sub := ratelimit.AddrKeys("203.0.113.9:1")
	rs.circuitsMu.RLock()
	defer rs.circuitsMu.RUnlock()
	if r := rs.admitLocked(mkID(0x21), ip, sub, false, lim); r != "" {
		t.Fatalf("a neighbour in the /24 must still get in below the subnet cap, refused: %s", r)
	}
	ip, sub = ratelimit.AddrKeys("198.51.100.7:1")
	if r := rs.admitLocked(mkID(0x22), ip, sub, false, lim); r != "" {
		t.Fatalf("a peer from another network is not affected, refused: %s", r)
	}
}

// Fresh peers can fill only part of the table; the rest is kept for peers connected a while.
func TestRelayServer_ReservedShareForEstablishedPeers(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	rs.SetPolicy(config.RelayServerConfig{Enabled: true, MaxCircuits: 8, ReservedCircuitsPct: 25})
	_, lim := rs.policy()
	if lim.newPeerCircuits != 6 {
		t.Fatalf("fresh peers may hold %d circuits, want 6", lim.newPeerCircuits)
	}
	rs.circuitsMu.Lock()
	for i := 0; i < 6; i++ {
		rs.circuits[uint32(i+1)] = &Circuit{ID: uint32(i + 1), PeerA: mkID(byte(0x30 + i)), PeerB: mkID(0xee)}
	}
	rs.circuitsMu.Unlock()

	newcomer, regular := mkID(0x40), mkID(0x41)
	rs.connectedAt[newcomer] = time.Now()
	rs.connectedAt[regular] = time.Now().Add(-establishedAfter - time.Minute)
	now := time.Now()
	rs.circuitsMu.RLock()
	defer rs.circuitsMu.RUnlock()
	if r := rs.admitLocked(newcomer, "", "", rs.established(newcomer, now), lim); r == "" {
		t.Fatal("a newcomer must not take the reserved slots")
	}
	if r := rs.admitLocked(regular, "", "", rs.established(regular, now), lim); r != "" {
		t.Fatalf("an established peer must get a reserved slot, refused: %s", r)
	}
}

// A circuit kept alive by a trickle of tiny frames is idle all the same; a busy one stays.
func TestRelayServer_ReapByBytesMoved(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	old := time.Now().Add(-10 * time.Minute)

	trickle := &Circuit{ID: 1, PeerA: mkID(1), PeerB: mkID(2), CreatedAt: old}
	trickle.LastActive.Store(time.Now().UnixNano()) // a frame just now...
	trickle.periodBytes.Store(200)                  // ...but hardly anything over the period

	busy := &Circuit{ID: 2, PeerA: mkID(3), PeerB: mkID(4), CreatedAt: old}
	busy.LastActive.Store(time.Now().UnixNano())
	busy.periodBytes.Store(10 << 20)

	signal := &Circuit{ID: 3, PeerA: mkID(5), PeerB: mkID(6), CreatedAt: time.Now().Add(-3 * time.Minute), SignalOnly: true}
	signal.LastActive.Store(time.Now().UnixNano())

	rs.circuits[1], rs.circuits[2], rs.circuits[3] = trickle, busy, signal
	if n := rs.reapIdleCircuits(defaultCircuitIdle, defaultCircuitIdleKB<<10); n != 2 {
		t.Fatalf("reaped %d circuits, want 2 (the trickle and the expired signaling one)", n)
	}
	if _, ok := rs.circuits[2]; !ok {
		t.Fatal("the busy circuit must stay")
	}
	if busy.periodBytes.Load() != 0 {
		t.Fatal("a new period starts with nothing counted")
	}
	// the busy circuit now has to move enough again in its new period
	if n := rs.reapIdleCircuits(defaultCircuitIdle, defaultCircuitIdleKB<<10); n != 0 {
		t.Fatal("a circuit is not judged before its period is over")
	}
}

func TestRelayServer_CircuitByteBudgets(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	rs.SetPolicy(config.RelayServerConfig{Enabled: true, CircuitMaxMB: 1, PeerMaxMBPerDay: 2, PeerMaxMbps: -1})
	_, lim := rs.policy()
	now := time.Now()
	peer := mkID(0x50)
	use := rs.useOf(peer, lim)

	c := &Circuit{ID: 1, PeerA: peer, use: use}
	c.BytesRouted = 1<<20 - 100
	if v := rs.chargeFrame(c, 100, lim, now); v != framePass {
		t.Fatalf("a frame that fits the circuit budget must pass, got %v", v)
	}
	c.BytesRouted = 1 << 20
	if v := rs.chargeFrame(c, 1, lim, now); v != frameOverBudget {
		t.Fatalf("the circuit budget must close the circuit, got %v", v)
	}

	// the same peer's other circuits share its daily budget
	c2 := &Circuit{ID: 2, PeerA: peer, use: use}
	for i := 0; i < 2; i++ {
		if v := rs.chargeFrame(c2, 1<<20-100, lim, now); v != framePass {
			t.Fatalf("frame %d within the daily budget refused: %v", i, v)
		}
		c2.BytesRouted = 0
	}
	if v := rs.chargeFrame(c2, 1<<20, lim, now); v != frameOverBudget {
		t.Fatalf("the peer's daily budget must close the circuit, got %v", v)
	}
	if use.exhausted(lim, now) {
		t.Fatal("a refused frame is not counted: the budget is not used up yet")
	}
	use.bytes = lim.peerDayBytes
	if !use.exhausted(lim, now) {
		t.Fatal("a peer that used its whole budget opens no more circuits today")
	}
	// a new day starts afresh
	if v := rs.chargeFrame(c2, 1000, lim, now.Add(24*time.Hour)); v != framePass {
		t.Fatalf("a new day must reset the daily budget, got %v", v)
	}
}

func TestRelayServer_PeerAndNodeSpeedLimits(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	rs.SetPolicy(config.RelayServerConfig{Enabled: true, PeerMaxMbps: 8}) // 1 MB/s, burst 1 MiB
	_, lim := rs.policy()
	now := time.Now()
	c := &Circuit{ID: 1, PeerA: mkID(0x60), use: rs.useOf(mkID(0x60), lim)}
	passed := 0
	for i := 0; i < 100; i++ {
		if rs.chargeFrame(c, 64<<10, lim, now) == framePass {
			passed++
		}
	}
	if passed != 16 {
		t.Fatalf("%d frames of 64 KiB passed at once, want 16 (the 1 MiB burst)", passed)
	}
	if rs.chargeFrame(c, 64<<10, lim, now.Add(time.Second)) != framePass {
		t.Fatal("the bucket must refill with time")
	}

	// the node-wide limit covers everybody together
	rs.SetPolicy(config.RelayServerConfig{Enabled: true, PeerMaxMbps: -1, MaxMbps: 8})
	_, lim = rs.policy()
	passed = 0
	for i := 0; i < 40; i++ {
		ci := &Circuit{ID: uint32(10 + i), PeerA: mkID(byte(0x70 + i)), use: rs.useOf(mkID(byte(0x70+i)), lim)}
		if rs.chargeFrame(ci, 64<<10, lim, now) == framePass {
			passed++
		}
	}
	if passed > 16 {
		t.Fatalf("%d frames passed the 1 MB/s node limit at once", passed)
	}
}

// The usage records of peers without circuits are forgotten once they have nothing to remember.
func TestRelayServer_PruneUse(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	_, lim := rs.policy()
	idle, holder, spender := mkID(1), mkID(2), mkID(3)
	rs.useOf(idle, lim)
	rs.useOf(holder, lim)
	u := rs.useOf(spender, lim)
	u.day, u.bytes = time.Now().UTC().Truncate(24*time.Hour), 5000
	rs.pruneUse(map[types.PeerID]bool{holder: true}, time.Now())
	if _, ok := rs.use[idle]; ok {
		t.Fatal("a peer that used nothing and holds nothing must be forgotten")
	}
	if _, ok := rs.use[holder]; !ok {
		t.Fatal("a peer holding a circuit must be kept")
	}
	if _, ok := rs.use[spender]; !ok {
		t.Fatal("a peer that used part of today's budget must be kept")
	}
}

// The relay refuses a message for the mailbox from the mailbox's counts alone.
func TestRelayServer_MailboxRefusesEarly(t *testing.T) {
	mb := messenger.NewMailboxManager(newTestStore())
	mb.SetLimits(messenger.MailboxLimits{MaxItemSizeBytes: 100, MaxTotalMessages: 1, MaxMessagesPerPeer: 10})
	rs := NewRelayServer(nil, mb, nil)
	if err := rs.mailboxRefuses(mkID(1), 500); !errors.Is(err, messenger.ErrItemTooLarge) {
		t.Fatalf("an oversized item must be refused, got %v", err)
	}
	if err := rs.mailboxRefuses(mkID(1), 50); err != nil {
		t.Fatalf("an empty mailbox must not refuse, got %v", err)
	}
	if err := mb.Store(messenger.MailboxItem{RecipientID: mkID(1), SenderID: mkID(2), Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := rs.mailboxRefuses(mkID(3), 50); !errors.Is(err, messenger.ErrMailboxFull) {
		t.Fatalf("a full mailbox must refuse, got %v", err)
	}
	if NewRelayServer(nil, nil, nil).mailboxRefuses(mkID(1), 1) == nil {
		t.Fatal("a relay without a mailbox keeps nothing")
	}
}

func TestRelayServer_WarnLimitedCounts(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	for i := 0; i < 5; i++ {
		rs.warnLimited("x")
	}
	if rs.logSkipped["x"] != 4 {
		t.Fatalf("lines held back: %d, want 4", rs.logSkipped["x"])
	}
}

type testStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newTestStore() *testStore { return &testStore{m: map[string][]byte{}} }

func (s *testStore) Set(k, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(k)] = append([]byte(nil), v...)
	return nil
}
func (s *testStore) Get(k []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[string(k)]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}
func (s *testStore) Delete(k []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(k))
	return nil
}
func (s *testStore) FindValues(prefix []byte, _ int) ([]storage.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Entry
	for k, v := range s.m {
		if strings.HasPrefix(k, string(prefix)) {
			out = append(out, storage.Entry{Key: []byte(k), Value: v})
		}
	}
	return out, nil
}
func (s *testStore) FindValuesSeek(prefix, _ []byte, limit int) ([]storage.Entry, error) {
	return s.FindValues(prefix, limit)
}
func (s *testStore) Exists(k []byte) bool { _, err := s.Get(k); return err == nil }
func (s *testStore) Close()               {}
