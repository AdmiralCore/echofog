package node_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// TestNarrowPathMarkTellsTheUserAndFollowsTheNetwork: a narrow-path mark raises an event in Russian
// (neutral form) that says what the connection does about it, belongs to its network, and is
// forgotten when the network changes, as the freeze mark is (T3.12, T6.4d).
func TestNarrowPathMarkTellsTheUserAndFollowsTheNetwork(t *testing.T) {
	events := &captureEvents{}
	a := newTestNodeWith(t, cards.Hidden, false, nil, events, nil)
	a.rt.SetNetworkID("net-a")
	np := node.NarrowPathOf(a.rt)
	np.Mark("net-a", carrier.ReasonMSS)
	e, ok := narrowEvent(events)
	if !ok {
		t.Fatal("no narrow-path event for the UI")
	}
	for _, want := range []string{"Путь сети узкий", "MSS", "MTU 1200"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("message %q lacks %q", e.Message, want)
		}
	}
	if !np.Marked("net-a") {
		t.Fatal("not marked")
	}
	a.rt.SetNetworkID("net-b")
	if np.Marked("net-a") {
		t.Fatal("the mark survived a change of network")
	}
}

// narrowEvent returns the narrow-path event the sink captured.
func narrowEvent(c *captureEvents) (node.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.list {
		if e.Kind == node.EventNarrowPath {
			return e, true
		}
	}
	return node.Event{}, false
}

// The silent-noise3 mark is about the UDP carrier, not about MTU: the message must not claim a
// narrow path (architect's decision on M2).
func TestNoise3SilentMessageIsAboutTheUDPCarrier(t *testing.T) {
	events := &captureEvents{}
	a := newTestNodeWith(t, cards.Hidden, false, nil, events, nil)
	a.rt.SetNetworkID("net-a")
	node.NarrowPathOf(a.rt).Mark("net-a", carrier.ReasonNoise3Silent)
	e, ok := narrowEvent(events)
	if !ok {
		t.Fatal("no event")
	}
	if strings.Contains(e.Message, "MTU 1280") || strings.Contains(e.Message, "MTU") {
		t.Fatalf("message %q claims a narrow path for a silent UDP carrier", e.Message)
	}
	if !strings.Contains(e.Message, "UDP-носитель") {
		t.Fatalf("message %q does not name the UDP carrier", e.Message)
	}
}

// T3: the node hands the narrow-path marks, the network and the MSS opener to the carrier of every
// direct dial; with a proxy there is no MSS opener (a proxy opens its own sockets).
func TestDirectDialCarriesTheNarrowPathState(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		var mu sync.Mutex
		var got []carrier.DialRequest
		hook := func(req carrier.DialRequest) { mu.Lock(); got = append(got, req); mu.Unlock() }
		opts := routeOpts{net: rp.NetWired, bCover: true, hook: hook}
		if proxy {
			opts.dialCtx = func(ctx context.Context, n, a string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, n, a)
			}
		}
		w := newRouteWorld(t, opts)
		_, _ = w.open(t, node.DialOpt{}) // the fake suites fail; only the requests matter
		mu.Lock()
		if len(got) == 0 {
			mu.Unlock()
			t.Fatalf("proxy=%v: no direct dial reached a carrier", proxy)
		}
		for _, req := range got {
			if req.Narrow == nil || req.NetworkID == nil {
				t.Errorf("proxy=%v: the dial request lacks Narrow/NetworkID", proxy)
			}
			if (req.DialMSS != nil) == proxy {
				t.Errorf("proxy=%v: DialMSS set = %v", proxy, req.DialMSS != nil)
			}
		}
		mu.Unlock()
	}
}
