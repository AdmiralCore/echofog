package noise3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

func selFor(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:16]
}

// Н-1: the replay filter must forget selectors older than the retention window instead of
// saturating forever.
func TestReplayCacheAgesOut(t *testing.T) {
	t0 := time.Unix(10_000, 0)
	c := newReplayCache(nil, t0, nil)
	old := selFor("old")
	if c.seen(old, t0) {
		t.Fatal("a fresh selector is reported as a replay")
	}
	// Move through a whole retention window, one generation at a time.
	for k := 1; k <= bloomGens; k++ {
		c.seen(selFor("tick"), t0.Add(time.Duration(k)*bloomPeriod))
	}
	late := t0.Add(time.Duration(bloomGens)*bloomPeriod + time.Second)
	if c.seen(old, late) {
		t.Fatal("a selector older than the retention window is still treated as a replay")
	}
	if !c.seen(old, late) {
		t.Fatal("a selector observed now is not detected as a replay")
	}
	fresh := selFor("fresh")
	if c.seen(fresh, late) {
		t.Fatal("a fresh selector is reported as a replay")
	}
	if !c.seen(fresh, late) {
		t.Fatal("a fresh selector is not detected as a replay on the second look")
	}
}

// Н-1: after many rotations the false-positive rate must stay bounded; the old single Bloom
// grows without limit.
func TestReplayCacheFPRStaysBounded(t *testing.T) {
	t0 := time.Unix(20_000, 0)
	c := newReplayCache(nil, t0, nil)
	const generations = 12
	const perGeneration = 250_000
	for g := 0; g < generations; g++ {
		at := t0.Add(time.Duration(g) * bloomPeriod)
		for i := 0; i < perGeneration; i++ {
			c.seen(selFor("gen-"+string(rune('a'+g))+"-"+itoa(i)), at)
		}
	}
	final := t0.Add(time.Duration(generations) * bloomPeriod)
	const probes = 20_000
	falsePositives := 0
	for i := 0; i < probes; i++ {
		if c.seen(selFor("unseen-"+itoa(i)), final) {
			falsePositives++
		}
	}
	if falsePositives*100 > probes*3 {
		t.Fatalf("false positives %d/%d exceed 3%% after %d generations", falsePositives, probes, generations)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// Н-2: packets rejected by the per-IP budget must not consume the listener-wide budget.
func TestBudgetChecksIPBeforeListener(t *testing.T) {
	now := time.Unix(30_000, 0)
	b := newBudgets()
	for i := 0; i < strangerPerListener; i++ {
		b.allow("192.0.2.1", now)
	}
	if !b.allow("192.0.2.2", now) {
		t.Fatal("one noisy IP exhausted the listener budget for a different IP")
	}
}

// Н-3: a flood of pending flows must never evict an established flow.
func TestPendingEvictionKeepsEstablished(t *testing.T) {
	mk, _ := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{
		Keys:            []MaskKey{mk},
		Profile:         mustProfile(t),
		MaxPending:      1,
		MaxPendingPerIP: 1,
		MaxFlows:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := time.Now()
	ip1 := netip.MustParseAddr("192.0.2.41")
	f1 := l.newFlow(netip.AddrPortFrom(ip1, 1000), packet.Keys{}, 1300, 0, now)
	if f1 == nil {
		t.Fatal("newFlow returned nil for an empty listener")
	}
	if f1.markEstablished() {
		l.establish(f1)
	}
	ip2 := netip.MustParseAddr("192.0.2.42")
	if f2 := l.newFlow(netip.AddrPortFrom(ip2, 2000), packet.Keys{}, 1300, 0, now); f2 != nil {
		t.Fatal("a new flow was created although the total limit is full and nothing is pending")
	}
	if f1.isClosed() {
		t.Fatal("an established flow was evicted instead of rejecting the new Init")
	}
	if l.flows[netip.AddrPortFrom(ip1, 1000)] != f1 {
		t.Fatal("the established flow disappeared from the flow table")
	}
}

// Н-4: a DNS host must never be resolved by the local system resolver.
func TestDialerRejectsDNSNameWithoutResolver(t *testing.T) {
	_, pub := newMaskKey(t, time.Now())
	card := cardWith(t, pub)
	addr := &pb.Addr{Suite: 1, Host: "noise3.example", Port: 443, Mask: 1, Params: suiteParams(t, shape.IDDefaultUDP)}
	d := NewDialer(DialerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
	if !errors.Is(err, ErrDNSName) {
		t.Fatalf("Dial(DNS name) = %v, want ErrDNSName", err)
	}
}

func TestDialerPassesDNSNameToInjectedDialer(t *testing.T) {
	_, pub := newMaskKey(t, time.Now())
	card := cardWith(t, pub)
	addr := &pb.Addr{Suite: 1, Host: "noise3.example", Port: 443, Mask: 1, Params: suiteParams(t, shape.IDDefaultUDP)}
	sentinel := errors.New("hook")
	var got string
	d := NewDialer(DialerConfig{DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
		got = address
		return nil, sentinel
	}})
	_, err := d.Dial(context.Background(), carrier.DialRequest{Card: card, Addr: addr})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Dial = %v, want the injected dialer error", err)
	}
	if got != "noise3.example:443" {
		t.Fatalf("injected dialer got %q, want the name unresolved", got)
	}
}

// Н-5: the replay cache must also be flushed by a timer, not only by arriving packets.
func TestReplayFlushWhenDue(t *testing.T) {
	kv := newMapKV()
	t0 := time.Unix(40_000, 0)
	c := newReplayCache(kv, t0, nil)
	c.seen(selFor("flush-me"), t0)
	c.maybeFlush(t0.Add(time.Second))
	if ok, _ := kv.Has([]byte(bloomKey)); ok {
		t.Fatal("the cache flushed before the flush interval")
	}
	c.maybeFlush(t0.Add(13 * time.Second))
	if ok, _ := kv.Has([]byte(bloomKey)); !ok {
		t.Fatal("the cache did not flush when the interval passed")
	}
}

// Н-9: the mask-key expiry check uses the injected clock.
func TestDialerMaskKeyExpiryUsesClock(t *testing.T) {
	_, pub := newMaskKey(t, time.Now())
	key := append([]byte{0x02}, pub...)
	card := &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{
		Key:      key,
		NotAfter: uint64(time.Now().Add(24 * time.Hour).Unix()),
	}}}}
	addr := &pb.Addr{Suite: 1, Host: "127.0.0.1", Port: 443, Mask: 1, Params: suiteParams(t, shape.IDDefaultUDP)}
	clock := &fakeClock{t: time.Now().Add(48 * time.Hour)}
	called := false
	d := NewDialer(DialerConfig{Clock: clock, DialContext: func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("hook")
	}})
	_, err := d.Dial(context.Background(), carrier.DialRequest{Card: card, Addr: addr})
	if !errors.Is(err, ErrBadConfig) {
		t.Fatalf("Dial with an expired key = %v, want ErrBadConfig", err)
	}
	if called {
		t.Fatal("the dialer went to the network with an expired mask key")
	}
}

// Н-10: every restart draws a new uniform(12, 18) silence interval.
func TestRestartRedrawsSilenceInterval(t *testing.T) {
	clock := &fakeClock{t: time.Unix(50_000, 0)}
	maskPub, err := maskPublic([32]byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	pc := newFakePC()
	c := newUnitDialConn(t, clock, maskPub, func(context.Context) (net.PacketConn, net.Addr, error) {
		return pc, flowAddr{id: 3}, nil
	})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.restartAfter = time.Second
	if _, err := c.write(make([]byte, 1200)); err != nil {
		t.Fatal(err)
	}
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
	c.mu.Lock()
	got := c.restartAfter
	c.mu.Unlock()
	if got == time.Second {
		t.Fatal("restartAfter was not redrawn on restart")
	}
	if got < restartMin || got > restartMax {
		t.Fatalf("restartAfter = %v outside [%v, %v]", got, restartMin, restartMax)
	}
}

// Н-8: a genuinely new handshake on the same 4-tuple gets a new flow and new reply keys.
func TestNewInitOnEstablishedTupleStartsNewFlow(t *testing.T) {
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
	src := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	now := uint32(time.Now().Unix())
	first, keys := craftInit(t, pub, now, 1300)
	if _, err := conn.Write(first); err != nil {
		t.Fatal(err)
	}
	f1 := waitFlow(t, l, src)
	if _, err := conn.Write(mustSeal(t, keys, 0, 40)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !f1.isEstablished() {
		time.Sleep(10 * time.Millisecond)
	}
	if !f1.isEstablished() {
		t.Fatal("the flow did not become established")
	}
	vaddr := f1.vaddr
	next, nextKeys := craftInit(t, pub, now+20, 1300)
	if _, err := conn.Write(next); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	var f2 *flow
	for time.Now().Before(deadline) {
		l.mu.Lock()
		f2 = l.flows[src]
		l.mu.Unlock()
		if f2 != nil && f2 != f1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f2 == nil || f2 == f1 {
		t.Fatal("a new handshake on the same 4-tuple stayed in the old flow")
	}
	if f2.vaddr == vaddr {
		t.Fatal("the new flow reused the old virtual address")
	}
	f2.mu.Lock()
	sel := f2.keys[0].Sel
	f2.mu.Unlock()
	if !bytes.Equal(sel[:], nextKeys.Sel[:]) {
		t.Fatal("the responder did not adopt the new Init keys for the new flow")
	}
}

// A late sibling Init of the current handshake must not reset the flow.
func TestLateSiblingInitKeepsFlow(t *testing.T) {
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
	src := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	now := uint32(time.Now().Unix())
	first, keys := craftInit(t, pub, now, 1300)
	_, _ = conn.Write(first)
	f := waitFlow(t, l, src)
	_, _ = conn.Write(mustSeal(t, keys, 0, 40))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !f.isEstablished() {
		time.Sleep(10 * time.Millisecond)
	}
	sibling, _ := craftInit(t, pub, now, 1300)
	_, _ = conn.Write(sibling)
	time.Sleep(150 * time.Millisecond)
	l.mu.Lock()
	got := l.flows[src]
	l.mu.Unlock()
	if got != f {
		t.Fatal("a late sibling Init reset an established flow")
	}
	f.mu.Lock()
	n := len(f.keys)
	f.mu.Unlock()
	if n < 2 {
		t.Fatalf("sibling keys were not kept (%d sets)", n)
	}
}

func waitFlow(t *testing.T, l *Listener, src netip.AddrPort) *flow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		f := l.flows[src]
		l.mu.Unlock()
		if f != nil {
			return f
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no flow for %v", src)
	return nil
}
