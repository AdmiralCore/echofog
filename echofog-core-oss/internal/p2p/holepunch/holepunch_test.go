package holepunch

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	idA = types.PeerID{0xA}
	idB = types.PeerID{0xB}
)

// link delivers coordination messages between services in memory, as a relay would.
type link struct {
	from  types.PeerID
	mu    *sync.Mutex
	peers map[types.PeerID]*Service
	drop  func(m message) bool
	log   *[]message
}

func (l *link) Send(_ context.Context, to types.PeerID, payload []byte) error {
	var m message
	_ = json.Unmarshal(payload, &m)
	l.mu.Lock()
	if l.log != nil {
		*l.log = append(*l.log, m)
	}
	target := l.peers[to]
	drop := l.drop != nil && l.drop(m)
	l.mu.Unlock()
	if target == nil {
		return errors.New("no route to peer")
	}
	if drop {
		return nil
	}
	go target.Handle(l.from, payload)
	return nil
}

// fakeNet is the "network": punching and dialling only produce a connection when both ends act.
type fakeNet struct {
	mu        sync.Mutex
	connected map[types.PeerID]bool // keyed by the peer that is now connected to us
	punching  map[string]chan struct{}
	punchHits int
	dials     []string
	// scripted behaviour
	failFirstPunches int // PunchHole fails this many times before it can succeed
	dialWorks        bool
	punchWorks       bool
}

func newFakeNet() *fakeNet {
	return &fakeNet{connected: map[types.PeerID]bool{}, punching: map[string]chan struct{}{}, dialWorks: true, punchWorks: true}
}

type endpoint struct {
	n    *fakeNet
	self types.PeerID
	peer *endpoint
}

func (e *endpoint) PunchHole(ctx context.Context, addr string, peer types.PeerID) error {
	e.n.mu.Lock()
	e.n.punchHits++
	if e.n.failFirstPunches > 0 {
		e.n.failFirstPunches--
		e.n.mu.Unlock()
		return errors.New("punch timed out")
	}
	ch := make(chan struct{})
	e.n.punching[addr] = ch
	works := e.n.punchWorks
	e.n.mu.Unlock()
	if !works {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-ch: // the peer dialled while we were punching
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("punch timed out")
	}
}

func (e *endpoint) DialDirect(ctx context.Context, addr string) error {
	e.n.mu.Lock()
	e.n.dials = append(e.n.dials, addr)
	works := e.n.dialWorks
	e.n.mu.Unlock()
	if !works {
		return errors.New("dial failed")
	}
	// give the other side a moment to be punching, then connect
	time.Sleep(20 * time.Millisecond)
	e.n.mu.Lock()
	defer e.n.mu.Unlock()
	e.n.connected[e.self] = true
	e.n.connected[e.peer.self] = true
	for _, ch := range e.n.punching {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	return nil
}

type pair struct {
	a, b *Service
	net  *fakeNet
	log  []message
}

func newPair(t *testing.T, mod func(cfgA, cfgB *Config)) *pair {
	n := newFakeNet()
	ea, eb := &endpoint{n: n, self: idA}, &endpoint{n: n, self: idB}
	ea.peer, eb.peer = eb, ea
	var mu sync.Mutex
	peers := map[types.PeerID]*Service{}
	p := &pair{net: n}
	connected := func(self types.PeerID) func(types.PeerID) bool {
		return func(types.PeerID) bool { n.mu.Lock(); defer n.mu.Unlock(); return n.connected[self] }
	}
	cfgA := Config{Transport: ea, Signal: &link{from: idA, mu: &mu, peers: peers, log: &p.log}, AllowPrivate: true,
		OwnAddrs: func() []string { return []string{"203.0.113.1:4001"} }, Connected: connected(idA), ConnectWait: 500 * time.Millisecond}
	cfgB := Config{Transport: eb, Signal: &link{from: idB, mu: &mu, peers: peers}, AllowPrivate: true,
		OwnAddrs: func() []string { return []string{"198.51.100.2:4002"} }, Connected: connected(idB), ConnectWait: 500 * time.Millisecond}
	if mod != nil {
		mod(&cfgA, &cfgB)
	}
	p.a, p.b = New(cfgA), New(cfgB)
	mu.Lock()
	peers[idA], peers[idB] = p.a, p.b
	mu.Unlock()
	t.Cleanup(func() { p.a.Close(); p.b.Close() })
	return p
}

func TestInitiatorPunchesWhileTheResponderDials(t *testing.T) {
	p := newPair(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA] && p.net.connected[idB])
	assert.Equal(t, 1, p.net.punchHits, "first attempt: the initiator punches")
	assert.Equal(t, []string{"203.0.113.1:4001"}, p.net.dials, "and the responder dials the initiator's address")

	var kinds []string
	for _, m := range p.log {
		kinds = append(kinds, m.T)
	}
	assert.Equal(t, []string{"connect", "sync"}, kinds, "CONNECT then SYNC, as in DCUtR")
}

func TestLastAttemptSwapsRolesSoBothDial(t *testing.T) {
	p := newPair(t, nil)
	p.net.failFirstPunches = 2 // attempts one and two cannot punch through
	p.net.dialWorks = false    // and nothing can be dialled until the third attempt

	// the hook runs when the initiator sends SYNC, before the responder reacts to it
	syncs := 0
	p.a.cfg.Signal.(*link).drop = func(m message) bool {
		if m.T == "sync" {
			p.net.mu.Lock()
			syncs++
			p.net.dialWorks = syncs >= 3
			p.net.mu.Unlock()
		}
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	var both []bool
	for _, m := range p.log {
		if m.T == "sync" {
			both = append(both, m.Both)
		}
	}
	assert.Equal(t, []bool{false, false, true}, both, "attempts 1 and 2 punch, attempt 3 has both sides dial")
}

func TestNoOwnAddressMeansNoAttempt(t *testing.T) {
	p := newPair(t, func(a, _ *Config) { a.OwnAddrs = func() []string { return nil } })
	err := p.a.Connect(context.Background(), idB)
	assert.ErrorIs(t, err, ErrNoOwnAddr)
	assert.Empty(t, p.log, "nothing is sent when we cannot say where we are")
}

func TestPeerWithoutUsableAddressesIsGivenUpOn(t *testing.T) {
	p := newPair(t, func(_, b *Config) { b.OwnAddrs = func() []string { return []string{"10.0.0.5:4002"} } })
	p.a.cfg.AllowPrivate = false // a real network: a LAN address is useless to a remote peer
	p.b.cfg.AllowPrivate = true
	err := p.a.Connect(context.Background(), idB)
	assert.ErrorIs(t, err, ErrNoRemoteAddr)
}

func TestUnansweredConnectFailsInsteadOfHanging(t *testing.T) {
	p := newPair(t, nil)
	p.b.cfg.Signal.(*link).drop = func(m message) bool { return m.T == "answer" }
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	err := p.a.Connect(ctx, idB)
	assert.ErrorIs(t, err, ErrFailed)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestAddressFiltering(t *testing.T) {
	s := New(Config{OwnAddrs: func() []string { return nil }, Connected: func(types.PeerID) bool { return false }})
	got := s.usable([]string{
		"203.0.113.9:4001",   // fine
		"203.0.113.9:4001",   // duplicate
		"127.0.0.1:4001",     // loopback
		"192.168.1.5:4001",   // LAN
		"10.1.2.3:4001",      // LAN
		"100.64.0.7:4001",    // carrier-grade NAT range
		"169.254.1.1:4001",   // link local
		"0.0.0.0:4001",       // unspecified
		"224.0.0.1:4001",     // multicast
		"203.0.113.9:0",      // no port
		"not an address",     // garbage
		"[2001:db8::1]:4001", // public IPv6 is fine
	})
	var s2 []string
	for _, a := range got {
		s2 = append(s2, a.String())
	}
	assert.Equal(t, []string{"203.0.113.9:4001", "[2001:db8::1]:4001"}, s2)
}

func TestAtMostEightAddressesAreUsed(t *testing.T) {
	s := New(Config{})
	var in []string
	for i := 1; i <= 20; i++ {
		in = append(in, "203.0.113."+string(rune('0'+i%10))+":"+"40"+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	assert.LessOrEqual(t, len(s.usable(in)), maxAddrs)
}

func TestGarbageMessagesAreIgnored(t *testing.T) {
	p := newPair(t, nil)
	for _, payload := range [][]byte{nil, []byte("{"), []byte(`{"t":"nonsense"}`), make([]byte, maxMessage+1), []byte(`{"t":"sync","n":1}`)} {
		p.b.Handle(idA, payload) // must not panic or start anything
	}
	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.Empty(t, p.net.dials)
	assert.Zero(t, p.b.responders)
}

func TestSyncWithoutAMatchingConnectDoesNothing(t *testing.T) {
	p := newPair(t, nil)
	raw, _ := json.Marshal(message{T: "sync", N: 42})
	p.b.Handle(idA, raw)
	time.Sleep(50 * time.Millisecond)
	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.Empty(t, p.net.dials, "an unsolicited SYNC cannot make us dial anywhere")
}

func TestOnePeerCannotMakeUsAnswerInABurst(t *testing.T) {
	p := newPair(t, nil)
	for i := 1; i <= 10; i++ {
		raw, _ := json.Marshal(message{T: "connect", N: uint64(i), Addr: []string{"203.0.113.1:4001"}})
		p.b.Handle(idA, raw)
	}
	p.b.mu.Lock()
	n := len(p.b.expecting)
	p.b.mu.Unlock()
	assert.Equal(t, maxAttempts, n, "three attempts are legitimate, the rest of the burst is dropped")
}

func TestTotalIncomingAttemptsAreCapped(t *testing.T) {
	p := newPair(t, nil)
	for i := 0; i < maxResponders+5; i++ {
		var from types.PeerID
		from[0], from[1] = 0x77, byte(i)
		raw, _ := json.Marshal(message{T: "connect", N: uint64(i), Addr: []string{"203.0.113.1:4001"}})
		p.b.Handle(from, raw)
	}
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	assert.Equal(t, maxResponders, p.b.responders)
}

func TestSecondConnectToTheSamePeerIsRejectedWhileOneRuns(t *testing.T) {
	p := newPair(t, nil)
	p.net.punchWorks = false // the first attempt keeps running: nothing ever connects
	p.net.dialWorks = false
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = p.a.Connect(ctx, idB) }()
	time.Sleep(150 * time.Millisecond)
	assert.ErrorIs(t, p.a.Connect(ctx, idB), ErrBusy)
}

func TestAlreadyConnectedNeedsNoPunching(t *testing.T) {
	p := newPair(t, nil)
	p.net.mu.Lock()
	p.net.connected[idA] = true
	p.net.mu.Unlock()
	require.NoError(t, p.a.Connect(context.Background(), idB))
	assert.Empty(t, p.log)
}

// A reply that arrives after the attempt gave up must not be taken for a new request and answered:
// that would let two nodes keep each other talking.
func TestAStaleAnswerIsDroppedNotAnswered(t *testing.T) {
	p := newPair(t, nil)
	raw, _ := json.Marshal(message{T: "answer", N: 99, Addr: []string{"203.0.113.1:4001"}})
	p.a.Handle(idB, raw)
	time.Sleep(50 * time.Millisecond)
	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.Empty(t, p.net.dials)
	assert.Zero(t, p.a.responders)
	assert.Empty(t, p.a.expecting)
	assert.Empty(t, p.b.expecting, "nothing was sent back")
}

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// Two peers behind one router cannot reach each other's public address; the LAN address works.
func TestPeersBehindTheSameNATGetEachOthersLANAddressesFirst(t *testing.T) {
	own := []netip.AddrPort{ap("203.0.113.21:4242")}
	remote := []netip.AddrPort{ap("203.0.113.21:5555")}
	got := withLAN(own, remote, []string{"10.20.1.12:4242", "203.0.113.99:1", "bogus", "127.0.0.1:9"})
	assert.Equal(t, []netip.AddrPort{ap("10.20.1.12:4242"), ap("203.0.113.21:5555")}, got,
		"LAN address first, then the public one; a public or loopback address is never taken as LAN")
}

func TestPeersBehindOtherNATsDoNotGetToKnowPrivateAddresses(t *testing.T) {
	own := []netip.AddrPort{ap("203.0.113.21:4242")}
	remote := []netip.AddrPort{ap("203.0.113.23:4242")}
	got := withLAN(own, remote, []string{"10.20.3.11:4242"})
	assert.Equal(t, remote, got, "somebody else's LAN address is useless here and may belong to a device of ours")
}

func TestLANAddressesAreFewAndPrivate(t *testing.T) {
	var in []string
	for i := 1; i <= 9; i++ {
		in = append(in, netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 168, 1, byte(i)}), 4242).String())
	}
	assert.Len(t, lan(in), 4)
	assert.Empty(t, lan([]string{"8.8.8.8:53", "169.254.1.1:4242", "[::1]:4242", "10.0.0.1:0"}))
}
