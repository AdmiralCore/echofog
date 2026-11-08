package node_test

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// fakeRaw is a raw wall clock that is deliberately wrong; the runtime's learned offset brings it
// back to real time.
type fakeRaw struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeRaw) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

type fakeCarrierClock struct{ raw *fakeRaw }

func (c fakeCarrierClock) Now() time.Time { return c.raw.Now() }
func (c fakeCarrierClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	return time.AfterFunc(d, f)
}

// TestRuntimeHandshakeUsesTheRawClock: a node whose raw clock is 1000 s ahead is rejected with
// clock-skew even though its clock carries a matching learned offset: the +-600 s check of the
// handshake uses the raw clock only (handshake.md section 7, audit M1 B-2).
func TestRuntimeHandshakeUsesTheRawClock(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)

	realNow := time.Now()
	raw := &fakeRaw{now: realNow.Add(1000 * time.Second)}
	clk, err := clock.New(clock.Config{Now: raw.Now})
	if err != nil {
		t.Fatalf("clock.New: %v", err)
	}
	// Three outgoing networks that agree with real time pull the offset toward -1000 s, one step
	// of 120 s at a time (more than the cap would never be reached by peers alone).
	for n := 1; n <= 3; n++ {
		clk.AddOutgoingSample(netip.AddrFrom4([4]byte{10, byte(n), 0, 1}), realNow)
	}
	if off := clk.Offset(); off != -120*time.Second {
		t.Fatalf("learned offset %v, want one 120 s step", off)
	}

	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
		c.Clock = clk
		c.CarrierClock = fakeCarrierClock{raw: raw}
	})

	b.start(t)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err == nil {
		_ = sess.Close()
		t.Fatal("a node with a raw clock 1000 s ahead was admitted because of its offset")
	}
	var rej *session.ErrRejected
	if !errors.As(err, &rej) || rej.Reason != handshake.ReasonClockSkew {
		t.Fatalf("Session: %v, want a clock-skew rejection", err)
	}
}

// TestIncomingSessionsGiveNoClockSample: three incoming peers whose clocks are 500 s fast (inside the
// +-600 s window) must not move the responder's offset (audit M1 B-2: only outgoing sessions
// count).
func TestIncomingSessionsGiveNoClockSample(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	b.start(t)
	for i := 0; i < 3; i++ {
		raw := &fakeRaw{now: time.Now().Add(500 * time.Second)}
		a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
			c.CarrierClock = fakeCarrierClock{raw: raw}
		})
		a.start(t)
		pair := memcarrier.NewQUIC()
		queue.push(pair.Server)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client})
		cancel()
		if err != nil {
			t.Fatalf("liar %d: %v", i, err)
		}
		defer sess.Close()
	}
	if off := b.rt.Clock().Offset(); off != 0 {
		t.Fatalf("three incoming liars moved the responder's offset to %v", off)
	}
}

// TestRuntimeWithoutHTTPClientMakesNoRequest: without cfg.HTTP the clock check is off (fail
// closed, audit M1 B-4): no HTTP request leaves the node.
func TestRuntimeWithoutHTTPClientMakesNoRequest(t *testing.T) {
	var n atomic.Int32
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	a.start(t)
	if a.rt.HTTP() != nil {
		t.Fatal("the runtime built an HTTP client of its own")
	}
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { n.Add(1); return nil, errors.New("blocked") })
	defer func() { http.DefaultTransport = old }()
	a.rt.CheckClockForTest()
	time.Sleep(200 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("%d HTTP requests without a configured client", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestClockSkewRejectionTellsTheUser: when a handshake fails because the device clock is more than
// 10 minutes off, the UI gets a clear event in Russian (T3.8-fix1 acceptance, finding 1). The
// tolerance stays at 600 s.
func TestClockSkewRejectionTellsTheUser(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	raw := &fakeRaw{now: time.Now().Add(15 * time.Minute)}
	events := &captureEvents{}
	a := newTestNodeWith(t, cards.Hidden, false, nil, events, func(c *node.Config) {
		c.CarrierClock = fakeCarrierClock{raw: raw}
	})
	b.start(t)
	a.start(t)
	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, _, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client}); err == nil {
		t.Fatal("a clock 15 minutes ahead was admitted")
	}
	e, ok := events.find(node.EventClockSkew)
	if !ok {
		t.Fatal("no clock-skew event for the UI")
	}
	if !strings.Contains(e.Message, "Часы устройства") || !strings.Contains(e.Message, "10 минут") {
		t.Fatalf("message %q is not the Russian diagnosis", e.Message)
	}
}
