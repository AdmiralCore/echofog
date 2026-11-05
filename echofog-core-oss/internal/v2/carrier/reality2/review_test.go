package reality2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGateAdmissionPersists reproduces review finding 1: an admitted 4-tuple must not be
// forgotten while it carries traffic. The lease is sliding, so a short-header packet 11
// minutes after admission still goes to the QUIC stack, not to the cover.
func TestGateAdmissionPersists(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	clk := newTestClock(time.Unix(1793664000, 0))
	g, mask := newTestGate(t, conn, cover, nil, 0, clk)

	src := fakeAddr("198.51.100.20:2000")
	for _, p := range initialFlight(t, tokenHello(t, mask, testAddr, clk.Now())) {
		conn.in <- fakePacket{p, src}
	}
	if _, _, ok := readGate(t, g); !ok {
		t.Fatal("first Initial was not delivered")
	}
	if _, _, ok := readGate(t, g); !ok {
		t.Fatal("second Initial was not delivered")
	}

	clk.Advance(11 * time.Minute)
	short := []byte{0x41, 0x00, 0x01, 0x02, 0x03}
	conn.in <- fakePacket{short, src}
	got, _, ok := readGate(t, g)
	if !ok || !bytes.Equal(got, short) {
		t.Fatal("short-header packet of an admitted 4-tuple was not delivered 11 minutes after admission")
	}
	if cover.count() != 0 {
		t.Fatalf("cover got %d packets from an admitted source", cover.count())
	}
}

// TestCoverEvictsIdleAtCapacity reproduces review finding 2: at a full session table the idle
// per-source socket must be evicted before a new stranger falls back to the shared socket.
func TestCoverEvictsIdleAtCapacity(t *testing.T) {
	srv := newPortEchoServer(t)
	replies := make(chan string, 8)
	now := time.Unix(1793664000, 0)
	c, err := NewUDPCover(CoverConfig{
		Addr:        srv,
		Reply:       func(p []byte, src net.Addr) { replies <- string(p) },
		MaxSessions: 1,
		IdleTimeout: 30 * time.Second,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sourceA := fakeAddr("198.51.100.1:1")
	sourceB := fakeAddr("198.51.100.2:2")
	reply := func() string {
		t.Helper()
		select {
		case p := <-replies:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("no cover reply")
			return ""
		}
	}

	c.Forward(sourceA, clientInitialForCID([]byte{1, 2, 3, 4}))
	portA := portOf(t, reply())
	now = now.Add(31 * time.Second)
	c.Forward(sourceB, clientInitialForCID([]byte{5, 6, 7, 8}))
	_ = reply()
	now = now.Add(time.Second)
	c.Forward(sourceA, clientInitialForCID([]byte{1, 2, 3, 4}))
	portA2 := portOf(t, reply())
	if portA2 == portA {
		t.Fatalf("source port %s reused after 31 s idle at capacity: the idle session was not evicted", portA)
	}
}

// portOf extracts the source port from a reply of newPortEchoServer: a short header, the
// client's four-byte SCID, then the decimal port.
func portOf(t *testing.T, reply string) string {
	t.Helper()
	if len(reply) < 6 {
		t.Fatalf("short echo reply %q", reply)
	}
	return reply[5:]
}

// TestGatePendingFlightExpires reproduces review finding 3: a partial flight must go to the
// cover by the clock, not only when the next packet happens to arrive. The decision is made on
// the injected clock; the sweep itself is the production timer.
func TestGatePendingFlightExpires(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	sk := mustMask(t)
	clk := newTestClock(time.Unix(1793664000, 0))
	g, err := NewGate(conn, GateConfig{
		Masks:      []MaskKey{{Private: sk}},
		Addrs:      []string{testAddr},
		Cover:      cover,
		Replay:     NewReplay(ReplayConfig{}),
		PendingTTL: 20 * time.Millisecond,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	startPump(t, g)

	src := fakeAddr("198.51.100.30:3")
	conn.in <- fakePacket{initialFlight(t, tokenHello(t, sk, testAddr, clk.Now()))[0], src}
	waitReads(t, conn, 2)
	if cover.count() != 0 {
		t.Fatal("a partial flight was forwarded before its TTL")
	}
	clk.Advance(21 * time.Millisecond)
	waitCount(t, cover, 1)
}

// TestReplayKVErrorFailsClosed reproduces review finding 9: a replay cache that cannot read
// its stored filter must not admit tokens (fail-open would reopen a 900-second replay window).
func TestReplayKVErrorFailsClosed(t *testing.T) {
	kv := &flakyKV{err: errors.New("storage is down")}
	now := time.Unix(1793664000, 0)
	r := NewReplay(ReplayConfig{KV: kv, Now: func() time.Time { return now }})
	mask := sha256Sum("mask")
	token := []byte("0123456789abcdef0123456789abcdef")
	if !r.SeenOrAdd(mask, token) {
		t.Fatal("token admitted while the replay cache could not be read")
	}

	kv.setErr(nil)
	now = now.Add(2 * time.Second)
	if r.SeenOrAdd(mask, token) {
		t.Fatal("token still rejected two seconds after the KV recovered")
	}
}

// TestCoverSharedCIDNotHijacked covers review finding 4: on the shared socket a stranger must
// not be able to take over another stranger's reply by reusing its connection ID.
func TestCoverSharedCIDNotHijacked(t *testing.T) {
	srv := newCIDReplyServer(t)
	replies := make(chan string, 8)
	now := time.Unix(1793664000, 0)
	c, err := NewUDPCover(CoverConfig{
		Addr:        srv,
		Reply:       func(p []byte, src net.Addr) { replies <- src.String() + ":" + string(p) },
		MaxSessions: 1,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	first := fakeAddr("198.51.100.10:10")
	attacker := fakeAddr("198.51.100.66:66")
	target := fakeAddr("198.51.100.77:77")
	// The first source takes the only per-source slot; the next two share the socket.
	c.Forward(first, clientInitialForCID([]byte{9, 9, 9, 9}))
	cid := []byte{1, 2, 3, 4}
	c.Forward(target, clientInitialForCID(cid))
	c.Forward(attacker, clientInitialForCID(cid))

	deadline := time.After(2 * time.Second)
	seen := 0
	wantBody := ":" + string(append([]byte{0x41}, cid...))
	for seen < 2 {
		select {
		case r := <-replies:
			if !strings.HasSuffix(r, wantBody) {
				continue // the per-source reply of the first stranger
			}
			if strings.HasPrefix(r, attacker.String()+":") {
				t.Fatalf("a reply was routed to the stranger that reused the connection ID: %q", r)
			}
			seen++
		case <-deadline:
			t.Fatalf("got %d shared-socket replies, want 2", seen)
		}
	}
}

// TestCoverSharedCIDLongestMatch covers the other half of finding 4: a short header carries no
// connection-ID length, so the most specific (longest) known ID must win.
func TestCoverSharedCIDLongestMatch(t *testing.T) {
	srv := newCIDReplyServer(t)
	replies := make(chan string, 8)
	now := time.Unix(1793664000, 0)
	c, err := NewUDPCover(CoverConfig{
		Addr:        srv,
		Reply:       func(p []byte, src net.Addr) { replies <- src.String() },
		MaxSessions: 1,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	first := fakeAddr("198.51.100.10:10")
	long := fakeAddr("198.51.100.20:20")
	short := fakeAddr("198.51.100.30:30")
	full := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	full[0] = 1
	c.Forward(first, clientInitialForCID([]byte{9, 9, 9, 9}))
	c.Forward(short, clientInitialForCID(full[:4]))
	c.Forward(long, clientInitialForCID(full))

	// The cover replies to each packet with its own SCID: four, then eight bytes. Both must
	// reach the source that owns that length, whatever the map order.
	got := map[string]int{}
	deadline := time.After(2 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case src := <-replies:
			got[src]++
		case <-deadline:
			t.Fatalf("got %d replies, want 3", i)
		}
	}
	if got[short.String()] != 1 {
		t.Fatalf("short-CID source got %d replies, want 1", got[short.String()])
	}
	if got[long.String()] != 1 {
		t.Fatalf("long-CID source got %d replies, want 1", got[long.String()])
	}
}

// newPortEchoServer replies with a short header carrying the client's SCID and the source port
// the cover saw: the host may be reached through a per-source socket or the shared one, so the
// reply must be routable either way.
func newPortEchoServer(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			p := buf[:n]
			if len(p) < 8 || p[0]&0x80 == 0 {
				continue
			}
			q := 6 + int(p[5])
			if q >= len(p) {
				continue
			}
			l := int(p[q])
			if q+1+l > len(p) {
				continue
			}
			reply := append([]byte{0x41}, p[q+1:q+1+l]...)
			reply = append(reply, fmt.Sprintf("%d", src.Port)...)
			_, _ = conn.WriteToUDP(reply, src)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

// newCIDReplyServer echoes a short-header reply whose DCID is the client's SCID.
func newCIDReplyServer(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			p := buf[:n]
			if len(p) < 8 || p[0]&0x80 == 0 {
				continue
			}
			q := 6 + int(p[5])
			if q >= len(p) {
				continue
			}
			l := int(p[q])
			if q+1+l > len(p) {
				continue
			}
			reply := append([]byte{0x41}, p[q+1:q+1+l]...)
			_, _ = conn.WriteToUDP(reply, src)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

// TestCoverKeepsActiveSession covers the other half of finding 4: a session with recent
// traffic is never evicted to make room, even at capacity; the newcomer waits on the shared
// socket.
func TestCoverKeepsActiveSession(t *testing.T) {
	srv := newPortEchoServer(t)
	replies := make(chan string, 8)
	now := time.Unix(1793664000, 0)
	c, err := NewUDPCover(CoverConfig{
		Addr:        srv,
		Reply:       func(p []byte, src net.Addr) { replies <- string(p) },
		MaxSessions: 1,
		IdleTimeout: 30 * time.Second,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sourceA := fakeAddr("198.51.100.1:1")
	sourceB := fakeAddr("198.51.100.2:2")
	reply := func() string {
		t.Helper()
		select {
		case p := <-replies:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("no cover reply")
			return ""
		}
	}

	c.Forward(sourceA, clientInitialForCID([]byte{1, 2, 3, 4}))
	portA := portOf(t, reply())
	now = now.Add(10 * time.Second)
	c.Forward(sourceB, clientInitialForCID([]byte{5, 6, 7, 8}))
	_ = reply()
	now = now.Add(10 * time.Second)
	c.Forward(sourceA, clientInitialForCID([]byte{1, 2, 3, 4}))
	if portA2 := portOf(t, reply()); portA2 != portA {
		t.Fatalf("an active session was evicted: port %s became %s", portA, portA2)
	}
}

// TestCoverReportsErrors covers finding 5: cover socket errors reach ListenerConfig.OnError.
func TestCoverReportsErrors(t *testing.T) {
	errs := make(chan error, 4)
	c, err := NewUDPCover(CoverConfig{
		Addr:    &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
		Reply:   func([]byte, net.Addr) {},
		OnError: func(err error) { errs <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.dial = func(string, *net.UDPAddr, *net.UDPAddr) (*net.UDPConn, error) {
		return nil, errors.New("dial failed")
	}
	c.Forward(fakeAddr("198.51.100.9:9"), []byte("x"))
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "dial failed") {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cover socket error was not reported")
	}
}

// TestReplaySingleFlusher covers finding 10: a cache shared by several listeners runs one
// flusher, not one per listener.
func TestReplaySingleFlusher(t *testing.T) {
	r := NewReplay(ReplayConfig{KV: newMemKV()})
	r.acquire()
	r.acquire()
	if n := r.flushers.Load(); n != 1 {
		t.Fatalf("%d flushers for two listeners, want 1", n)
	}
	r.release()
	if n := r.flushers.Load(); n != 1 {
		t.Fatalf("%d flushers after one release, want 1", n)
	}
	r.release()
	if n := r.flushers.Load(); n != 0 {
		t.Fatalf("%d flushers after the last release, want 0", n)
	}
}

// flakyKV is a KV whose reads fail until setErr(nil).
type flakyKV struct {
	mu  sync.Mutex
	err error
	m   map[string][]byte
}

func (kv *flakyKV) setErr(err error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.err = err
}

func (kv *flakyKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.err != nil {
		return nil, false, kv.err
	}
	v, ok := kv.m[string(key)]
	return append([]byte(nil), v...), ok, nil
}

func (kv *flakyKV) Put(_ context.Context, key, value []byte) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.err != nil {
		return kv.err
	}
	if kv.m == nil {
		kv.m = make(map[string][]byte)
	}
	kv.m[string(key)] = append([]byte(nil), value...)
	return nil
}
