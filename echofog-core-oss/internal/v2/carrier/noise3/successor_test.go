package noise3

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
)

// establishedFlow starts a listener, opens one flow from a fresh UDP socket and makes it
// established with a data packet. It returns the listener, socket, source, flow and its keys.
func establishedFlow(t *testing.T, clock Clock, ts uint32) (*Listener, []byte, *net.UDPConn, netip.AddrPort, *flow, packet.Keys) {
	t.Helper()
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	src := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	first, keys := craftInit(t, pub, ts, 1300)
	if _, err := conn.Write(first); err != nil {
		t.Fatal(err)
	}
	f := waitFlow(t, l, src)
	if _, err := conn.Write(mustSeal(t, keys, 0, 40)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "flow established", f.isEstablished)
	return l, pub, conn, src, f, keys
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func flowAt(l *Listener, src netip.AddrPort) *flow {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flows[src]
}

// A-2: an authentic Init with a newer ts, as any contact of a hidden node can build it, must not
// tear down an established flow that is still carrying data.
func TestNewInitDoesNotDropActiveFlow(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	l, pub, conn, src, f1, keys := establishedFlow(t, clock, uint32(clock.Now().Unix()))
	forged, _ := craftInit(t, pub, uint32(clock.Now().Unix())+20, 1300)
	clock.Advance(time.Second)
	if _, err := conn.Write(forged); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := flowAt(l, src); got != f1 || f1.isClosed() {
		t.Fatal("a newer-ts Init removed the established flow")
	}
	// The old peer keeps talking with its own keys: packet 1 is still accepted by f1.
	f1.mu.Lock()
	before := f1.inBytes
	f1.mu.Unlock()
	if _, err := conn.Write(mustSeal(t, keys, 1, 40)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old flow still receives", func() bool {
		f1.mu.Lock()
		defer f1.mu.Unlock()
		return f1.inBytes > before
	})
	if got := flowAt(l, src); got != f1 {
		t.Fatal("the old flow lost its slot while its peer was active")
	}
}

// A-2: the new flow takes over on its first authentic data packet, and the old one goes.
func TestSuccessorTakesOverOnFirstData(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	l, pub, conn, src, f1, _ := establishedFlow(t, clock, uint32(clock.Now().Unix()))
	next, nextKeys := craftInit(t, pub, uint32(clock.Now().Unix())+20, 1300)
	if _, err := conn.Write(next); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := conn.Write(mustSeal(t, nextKeys, 0, 40)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "successor installed", func() bool {
		f := flowAt(l, src)
		return f != nil && f != f1
	})
	f2 := flowAt(l, src)
	f2.mu.Lock()
	sel := f2.keys[0].Sel
	f2.mu.Unlock()
	if sel != nextKeys.Sel {
		t.Fatal("the new flow does not carry the new Init keys")
	}
	if !f1.isClosed() {
		t.Fatal("the old flow survived the takeover")
	}
	waitFor(t, "successor established", f2.isEstablished)
}

// A-2: an honest restart of the client after the old flow has been silent for 18 s still works
// without waiting for data.
func TestRestartAfterSilenceReplacesOldFlow(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	l, pub, conn, src, f1, _ := establishedFlow(t, clock, uint32(clock.Now().Unix()))
	clock.Advance(19 * time.Second)
	next, nextKeys := craftInit(t, pub, uint32(clock.Now().Unix()), 1300)
	if _, err := conn.Write(next); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old flow replaced", func() bool {
		f := flowAt(l, src)
		return f != nil && f != f1
	})
	f2 := flowAt(l, src)
	f2.mu.Lock()
	sel := f2.keys[0].Sel
	f2.mu.Unlock()
	if sel != nextKeys.Sel || !f1.isClosed() {
		t.Fatal("the silent old flow was not replaced by the new Init")
	}
}
