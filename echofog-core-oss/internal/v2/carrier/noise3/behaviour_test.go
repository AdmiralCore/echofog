package noise3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/elligator"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) AfterFunc(time.Duration, func()) carrier.Timer { return fakeTimer{} }

type fakeTimer struct{}

func (fakeTimer) Stop() bool { return true }

type fakePC struct {
	mu     sync.Mutex
	writes [][]byte
	closed bool
	done   chan struct{}
	once   sync.Once
}

func newFakePC() *fakePC { return &fakePC{done: make(chan struct{})} }

func (p *fakePC) ReadFrom([]byte) (int, net.Addr, error) {
	<-p.done
	return 0, nil, net.ErrClosed
}

func (p *fakePC) WriteTo(b []byte, _ net.Addr) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, net.ErrClosed
	}
	p.writes = append(p.writes, append([]byte(nil), b...))
	return len(b), nil
}

func (p *fakePC) Close() error {
	p.once.Do(func() { close(p.done) })
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *fakePC) LocalAddr() net.Addr              { return flowAddr{id: 99} }
func (p *fakePC) SetDeadline(time.Time) error      { return nil }
func (p *fakePC) SetReadDeadline(time.Time) error  { return nil }
func (p *fakePC) SetWriteDeadline(time.Time) error { return nil }

func (p *fakePC) snapshot() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.writes...)
}

func noPreludeProfile(t *testing.T) *shape.Profile {
	t.Helper()
	p, err := shape.Builtin(shape.IDNone)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newUnitDialConn(t *testing.T, clock Clock, maskPub [32]byte, dial func(context.Context) (net.PacketConn, error)) *dialConn {
	t.Helper()
	return newDialConn(DialerConfig{}, clock,
		noPreludeProfile(t), maskPub, dial, flowAddr{id: 3})
}

func TestClientSendsFreshInits(t *testing.T) {
	var maskSK [32]byte
	if _, err := rand.Read(maskSK[:]); err != nil {
		t.Fatal(err)
	}
	maskPub, err := maskPublic(maskSK)
	if err != nil {
		t.Fatal(err)
	}
	pc := newFakePC()
	c := newUnitDialConn(t, &fakeClock{t: time.Unix(1000, 0)}, maskPub, func(context.Context) (net.PacketConn, error) {
		return pc, nil
	})
	c.conn = pc

	inner := make([]byte, 1200)
	for i := range inner {
		inner[i] = byte(i)
	}
	for i := 0; i < 6; i++ {
		if _, err := c.write(inner); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	c.mu.Lock()
	sets := len(c.sets)
	c.mu.Unlock()
	if sets != 4 {
		t.Errorf("outstanding key sets = %d, want 4", sets)
	}
	writes := pc.snapshot()
	if len(writes) != 6 {
		t.Fatalf("wrote %d datagrams, want 6", len(writes))
	}
	seen := make(map[string]bool)
	for i, d := range writes {
		if len(d) < packet.MinInitLen {
			t.Fatalf("Init %d is %d bytes, below %d", i, len(d), packet.MinInitLen)
		}
		if seen[string(d)] {
			t.Fatalf("Init %d repeats an earlier datagram", i)
		}
		seen[string(d)] = true
		repr := d[:32]
		u := elligator.Forward(repr)
		z, err := curve25519.X25519(maskSK[:], u)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := packet.Derive(z, maskPub[:], u, repr)
		if err != nil {
			t.Fatal(err)
		}
		init, err := packet.OpenInit(keys, d)
		if err != nil {
			t.Fatalf("Init %d does not open: %v", i, err)
		}
		if !bytes.Equal(init.Inner, inner) {
			t.Fatalf("Init %d carries a different inner datagram", i)
		}
	}
}

func TestDialConnRestartsAfterSilence(t *testing.T) {
	clock := &fakeClock{t: time.Unix(2000, 0)}
	var maskSK [32]byte
	if _, err := rand.Read(maskSK[:]); err != nil {
		t.Fatal(err)
	}
	maskPub, err := maskPublic(maskSK)
	if err != nil {
		t.Fatal(err)
	}
	first := newFakePC()
	second := newFakePC()
	calls := 0
	c := newUnitDialConn(t, clock, maskPub, func(context.Context) (net.PacketConn, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.restartAfter = time.Second
	if _, err := c.write([]byte("hello inner")); err != nil {
		t.Fatal(err)
	}
	// Pretend the responder authenticated and the initiator adopted its keys.
	c.mu.Lock()
	k := c.sets[0]
	c.adopted = &k
	c.lastAuth = clock.Now()
	c.lastSend = clock.Now().Add(time.Millisecond)
	c.mu.Unlock()
	clock.Advance(2 * time.Second)
	if _, err := c.write(make([]byte, 1200)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("dial calls = %d, want 2", calls)
	}
	if !first.closed {
		t.Error("old outer socket was not closed")
	}
	c.mu.Lock()
	adopted := c.adopted
	c.mu.Unlock()
	if adopted != nil {
		t.Error("adopted keys survived the flow restart")
	}
	writes := second.snapshot()
	if len(writes) == 0 || len(writes[len(writes)-1]) < packet.MinInitLen {
		t.Fatal("the new flow did not start with an Init")
	}
}

func TestBudgets(t *testing.T) {
	now := time.Unix(3000, 0)
	b := newBudgets()
	for i := 0; i < strangerPerIP; i++ {
		if !b.allow("192.0.2.1", now) {
			t.Fatalf("per-IP budget rejected packet %d", i)
		}
	}
	if b.allow("192.0.2.1", now) {
		t.Fatal("per-IP budget accepted packet 51")
	}
	if !b.allow("192.0.2.2", now) {
		t.Fatal("a different IP shares the per-IP budget")
	}

	total := newBudgets()
	allowed := 0
	for ip := 1; ip <= 40; ip++ {
		addr := netip.AddrFrom4([4]byte{198, 51, 100, byte(ip)}).String()
		for i := 0; i < strangerPerIP; i++ {
			if total.allow(addr, now) {
				allowed++
			}
		}
	}
	if allowed != strangerPerListener {
		t.Fatalf("listener budget allowed %d packets, want %d", allowed, strangerPerListener)
	}
	if total.allow("203.0.113.1", now) {
		t.Fatal("listener budget accepted packet 2001")
	}
}

type mapKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMapKV() *mapKV { return &mapKV{m: make(map[string][]byte)} }

func (k *mapKV) Has(key []byte) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.m[string(key)]
	return ok, nil
}

func (k *mapKV) Get(key []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[string(key)]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), v...), nil
}

func (k *mapKV) PutTTL(key, value []byte, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[string(key)] = append([]byte(nil), value...)
	return nil
}

func TestReplayCachePersists(t *testing.T) {
	kv := newMapKV()
	now := time.Unix(4000, 0)
	c := newReplayCache(kv, now, nil)
	sel := bytes.Repeat([]byte{0x42}, 16)
	if c.seen(sel) {
		t.Fatal("a fresh selector is reported as a replay")
	}
	if !c.seen(sel) {
		t.Fatal("the same selector is not reported as a replay")
	}
	c.flush()
	again := newReplayCache(kv, now, nil)
	if !again.seen(sel) {
		t.Fatal("the replay cache did not survive a restart")
	}
}

func TestPendingLimitsAndEviction(t *testing.T) {
	mk, _ := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{
		Keys:            []MaskKey{mk},
		Profile:         mustProfile(t),
		MaxPending:      3,
		MaxPendingPerIP: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ip := netip.MustParseAddr("192.0.2.10")
	keys := packet.Keys{}
	for i := 0; i < 2; i++ {
		if f := l.newFlow(netip.AddrPortFrom(ip, uint16(1000+i)), keys, 1300, time.Now()); f == nil {
			t.Fatal("newFlow returned nil below the per-IP limit")
		}
	}
	if l.pending != 2 {
		t.Fatalf("pending = %d, want 2", l.pending)
	}
	if f := l.newFlow(netip.AddrPortFrom(ip, 2000), keys, 1300, time.Now()); f == nil {
		t.Fatal("newFlow returned nil above the per-IP limit")
	}
	if l.pendingIP[ip] != 2 || l.pending != 2 {
		t.Fatalf("per-IP pending = %d, total = %d; want 2 and 2", l.pendingIP[ip], l.pending)
	}
	// A different IP fills the total limit and then evicts a random pending flow.
	for i := 0; i < 2; i++ {
		l.newFlow(netip.AddrPortFrom(netip.MustParseAddr("192.0.2.11"), uint16(3000+i)), keys, 1300, time.Now())
	}
	if len(l.flows) != 3 {
		t.Fatalf("flows = %d, want the total limit 3", len(l.flows))
	}
}

func TestUnderLoad(t *testing.T) {
	mk, _ := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t),
		MaxPending: 4, MaxPendingPerIP: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.underLoad(nil) {
		t.Fatal("idle listener reports load")
	}
	keys := packet.Keys{}
	for i := 0; i < 3; i++ {
		l.newFlow(netip.AddrPortFrom(netip.MustParseAddr("192.0.2.20"), uint16(4000+i)), keys, 1300, time.Now())
	}
	if !l.underLoad(nil) {
		t.Fatal("listener at 75% of the pending limit does not report load")
	}
}

func TestAntiAmplification(t *testing.T) {
	f := &flow{inBytes: 100}
	if !f.maySendLocked(300) {
		t.Fatal("300 bytes to 100 received rejected")
	}
	if f.maySendLocked(301) {
		t.Fatal("301 bytes to 100 received accepted")
	}
	f.confirmed = true
	if f.maySendLocked(301) {
		t.Fatal("confirmation alone removed the cap before an authenticated packet")
	}
	f.addrAuthed = true
	if !f.maySendLocked(100000) {
		t.Fatal("confirmed and authenticated flow is still capped")
	}
}

func TestRegistration(t *testing.T) {
	mk, _ := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	d := NewDialer(DialerConfig{})
	reg := carrier.NewRegistry()
	d.Register(reg)
	l.Register(reg)
	if got, ok := reg.DialerFor(objects.SuiteNoise3); !ok || got != carrier.Dialer(d) {
		t.Fatalf("noise3 dialer not registered: %v %v", got, ok)
	}
	if got, ok := reg.ListenerFor(objects.SuiteNoise3); !ok || got != carrier.Listener(l) {
		t.Fatalf("noise3 listener not registered: %v %v", got, ok)
	}
}

// craftInit builds an Init for the given masking public key and returns the client keys too.
func craftInit(t *testing.T, maskPub []byte, ts uint32, total int) ([]byte, packet.Keys) {
	t.Helper()
	sk, repr, err := elligator.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	z, err := curve25519.X25519(sk, maskPub)
	if err != nil {
		t.Fatal(err)
	}
	u := elligator.Forward(repr)
	keys, err := packet.Derive(z, maskPub, u, repr)
	if err != nil {
		t.Fatal(err)
	}
	inner := make([]byte, 1200)
	for i := range inner {
		inner[i] = byte(i)
	}
	dg, err := packet.SealInit(keys, repr, ts, inner, total)
	if err != nil {
		t.Fatal(err)
	}
	return dg, keys
}

func noReply(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("listener answered %d bytes", n)
	}
}

func TestInvalidInitGetsNoReply(t *testing.T) {
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := uint32(time.Now().Unix())

	cases := []struct {
		name string
		pkt  func() []byte
	}{
		{"short", func() []byte { return make([]byte, 1200) }},
		{"zero representative", func() []byte {
			d := make([]byte, 1300)
			return d
		}},
		{"wrong selector", func() []byte {
			d, _ := craftInit(t, pub, now, 1300)
			d[32] ^= 0xff
			return d
		}},
		{"broken AEAD", func() []byte {
			d, _ := craftInit(t, pub, now, 1300)
			d[len(d)-1] ^= 0xff
			return d
		}},
		{"old timestamp", func() []byte {
			d, _ := craftInit(t, pub, now-301, 1300)
			return d
		}},
		{"future timestamp", func() []byte {
			d, _ := craftInit(t, pub, now+301, 1300)
			return d
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write(tc.pkt()); err != nil {
				t.Fatal(err)
			}
			noReply(t, conn)
		})
	}
}

func TestExhaustedBudgetGetsNoReply(t *testing.T) {
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := l.clock.Now()
	for i := 0; i < strangerPerIP; i++ {
		if !l.budget.allow("127.0.0.1", now) {
			t.Fatalf("budget rejected %d", i)
		}
	}
	conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d, _ := craftInit(t, pub, uint32(time.Now().Unix()), 1300)
	if _, err := conn.Write(d); err != nil {
		t.Fatal(err)
	}
	noReply(t, conn)
	l.mu.Lock()
	flows := len(l.flows)
	l.mu.Unlock()
	if flows != 0 {
		t.Fatalf("an over-budget Init created %d flows", flows)
	}
}

func TestReplayDropsSecondInit(t *testing.T) {
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d, _ := craftInit(t, pub, uint32(time.Now().Unix()), 1300)
	if _, err := conn.Write(d); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var f *flow
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, x := range l.flows {
			f = x
		}
		l.mu.Unlock()
		if f != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f == nil {
		t.Fatal("the first Init did not create a flow")
	}
	if _, err := conn.Write(d); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case <-time.After(200 * time.Millisecond):
	}
	f.mu.Lock()
	n := len(f.keys)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("replayed Init was processed: %d key sets", n)
	}
}

func TestNATRebinding(t *testing.T) {
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	target := l.Addr().(*net.UDPAddr)

	a, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	init, keys := craftInit(t, pub, uint32(time.Now().Unix()), 1300)
	if _, err := a.Write(init); err != nil {
		t.Fatal(err)
	}
	srcA := a.LocalAddr().(*net.UDPAddr).AddrPort()
	srcB := b.LocalAddr().(*net.UDPAddr).AddrPort()
	var f *flow
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		f = l.flows[srcA]
		l.mu.Unlock()
		if f != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f == nil {
		t.Fatal("Init did not create a flow")
	}
	if _, err := a.Write(mustSeal(t, keys, 0, 40)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// One authenticated packet from a new address must not move the reply address.
	if _, err := b.Write(mustSeal(t, keys, 20, 40)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := flowSrc(f); got != srcA {
		t.Fatalf("one copied packet moved the reply address to %v", got)
	}
	// Two packets with numbers above every number seen from the old address move it.
	if _, err := b.Write(mustSeal(t, keys, 21, 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(mustSeal(t, keys, 22, 40)); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if flowSrc(f) == srcB {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reply address stayed at %v, want %v", flowSrc(f), srcB)
}

func mustSeal(t *testing.T, keys packet.Keys, n uint64, innerLen int) []byte {
	t.Helper()
	inner := make([]byte, innerLen)
	if _, err := rand.Read(inner); err != nil {
		t.Fatal(err)
	}
	dg, err := packet.SealData(keys.KI2R, keys.HPI2R, n, inner, 0)
	if err != nil {
		t.Fatal(err)
	}
	return dg
}

func flowSrc(f *flow) netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.src
}
