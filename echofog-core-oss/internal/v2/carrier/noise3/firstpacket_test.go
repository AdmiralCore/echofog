package noise3

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// N3-FIX: every datagram of a flow before the first responder packet is at most 1200 bytes,
// the length differs from flow to flow, and the narrow paths (1232) pass.

type tapEvent struct {
	toServer bool
	size     int
}

// udpTap relays datagrams between one client socket and the listener and records their order.
type udpTap struct {
	conn   *net.UDPConn
	server *net.UDPAddr
	mu     sync.Mutex
	events []tapEvent
	client *net.UDPAddr
	up     *net.UDPConn
	// limit drops client datagrams longer than limit (a narrow path); 0 means no limit.
	limit int
}

func newUDPTap(t *testing.T, server net.Addr, limit int) *udpTap {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tp := &udpTap{conn: c, server: server.(*net.UDPAddr), limit: limit}
	t.Cleanup(func() { c.Close() })
	go tp.run()
	return tp
}

func (p *udpTap) addr() *net.UDPAddr { return p.conn.LocalAddr().(*net.UDPAddr) }

func (p *udpTap) record(toServer bool, n int) {
	p.mu.Lock()
	p.events = append(p.events, tapEvent{toServer, n})
	p.mu.Unlock()
}

func (p *udpTap) run() {
	buf := make([]byte, 4096)
	for {
		n, from, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.up == nil {
			up, err := net.DialUDP("udp", nil, p.server)
			if err != nil {
				p.mu.Unlock()
				return
			}
			p.up, p.client = up, from
			go func() {
				b := make([]byte, 4096)
				for {
					m, err := up.Read(b)
					if err != nil {
						return
					}
					p.record(false, m)
					_, _ = p.conn.WriteToUDP(b[:m], p.client)
				}
			}()
		}
		up := p.up
		p.mu.Unlock()
		if p.limit > 0 && n > p.limit {
			p.record(true, -n) // dropped by the narrow path
			continue
		}
		p.record(true, n)
		_, _ = up.Write(buf[:n])
	}
}

func (p *udpTap) snapshot() []tapEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tapEvent(nil), p.events...)
}

// dialThroughTap dials a fresh listener through a tap and returns what the tap saw.
func dialThroughTap(t *testing.T, maxDatagram, pathLimit int) ([]tapEvent, error) {
	t.Helper()
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t), MaxDatagramLen: maxDatagram})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tap := newUDPTap(t, l.Addr(), pathLimit)
	addr := noise3Addr(t, l, 1)
	addr.Port = uint32(tap.addr().Port)
	d := NewDialer(DialerConfig{MaxDatagramLen: maxDatagram})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		if s, err := l.Accept(ctx); err == nil {
			t.Cleanup(func() { _ = s.Close() })
		}
	}()
	c, err := d.Dial(ctx, carrier.DialRequest{Card: cardWith(t, pub), Addr: addr})
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return tap.snapshot(), err
}

// beforeAnswer returns the sizes of the client datagrams sent before the first server datagram.
func beforeAnswer(ev []tapEvent) []int {
	var out []int
	for _, e := range ev {
		if !e.toServer {
			break
		}
		n := e.size
		if n < 0 {
			n = -n
		}
		out = append(out, n)
	}
	return out
}

func TestNoDatagramBeforeAnswerOver1200(t *testing.T) {
	for i := 0; i < 3; i++ {
		ev, err := dialThroughTap(t, 0, 0)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		sizes := beforeAnswer(ev)
		if len(sizes) == 0 {
			t.Fatal("no client datagram seen")
		}
		for _, n := range sizes {
			if n > 1200 {
				t.Fatalf("flow %d: a datagram of %d bytes before the first answer, want <= 1200 (all: %v)", i, n, sizes)
			}
		}
	}
}

func TestFirstDatagramLengthVariesAcrossFlows(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 8; i++ {
		ev, err := dialThroughTap(t, 0, 0)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		sizes := beforeAnswer(ev)
		if len(sizes) == 0 {
			t.Fatal("no client datagram seen")
		}
		seen[sizes[0]] = true
	}
	if len(seen) < 4 {
		t.Fatalf("first datagram lengths of 8 flows: %d distinct values, want at least 4", len(seen))
	}
}

// The universal floor: a path that carries 1232 bytes of UDP payload (IPv6 MTU 1280) works, and
// the dial neither fails with a config error nor loses an Init to the path.
func TestPath1232Passes(t *testing.T) {
	ev, err := dialThroughTap(t, 1232, 1232)
	if err != nil {
		t.Fatalf("Dial on a 1232-byte path: %v", err)
	}
	for _, e := range ev {
		if e.toServer && e.size < 0 {
			t.Fatalf("the path dropped a %d-byte datagram", -e.size)
		}
	}
}

func TestCeilingBelowRangeIsPathTooSmall(t *testing.T) {
	d := NewDialer(DialerConfig{MaxDatagramLen: 899})
	_, pub := newMaskKey(t, time.Now())
	_, err := d.Dial(t.Context(), carrier.DialRequest{Card: cardWith(t, pub), Addr: noise3Addr(t, mustListener(t), 1)})
	if !errors.Is(err, ErrBadConfig) {
		t.Fatalf("Dial with a 899-byte ceiling = %v, want ErrBadConfig", err)
	}
}

func mustListener(t *testing.T) *Listener {
	t.Helper()
	mk, _ := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// The responder never sends more than three times what it has received before the flow is
// validated: the first answer is at most 3x the bytes the tap has seen go up.
func TestResponderAnswerAtMostThreeTimesReceived(t *testing.T) {
	ev, err := dialThroughTap(t, 0, 0)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	up, down := 0, 0
	for _, e := range ev {
		if e.toServer {
			if e.size > 0 {
				up += e.size
			}
			continue
		}
		down += e.size
		if down > 3*up {
			t.Fatalf("responder sent %d bytes after receiving %d (events %v)", down, up, ev)
		}
		if down > 3*up || len(ev) > 8 {
			break
		}
	}
}

func TestDefaultProfileInitLenWithin1200(t *testing.T) {
	p := mustProfile(t)
	if p.InitLen == nil || p.InitLen.Max > 1200 || p.InitLen.Min < 900 {
		t.Fatalf("default init_len = %+v, want inside [900, 1200]", p.InitLen)
	}
	bad := *p
	bad.InitLen = &shape.Range{Min: 1280, Max: 1420}
	if err := bad.Validate(); err == nil {
		t.Fatal("a profile with init_len above 1200 was accepted")
	}
}
