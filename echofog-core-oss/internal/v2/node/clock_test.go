package node_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
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

// TestRuntimeAppliesLearnedClockOffset pins the T3.7a clock integration: a node whose raw clock is
// 1000 s ahead can still handshake because the learned offset (T3.8) is passed into the
// handshake. Without the offset the responder would answer REJECT{clock-skew} (>600 s,
// handshake.md §7).
func TestRuntimeAppliesLearnedClockOffset(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)

	realNow := time.Now()
	raw := &fakeRaw{now: realNow.Add(1000 * time.Second)}
	clk, err := clock.New(clock.Config{Now: raw.Now})
	if err != nil {
		t.Fatalf("clock.New: %v", err)
	}
	clk.AddPeerSample(clock.PeerID(b.id.DeviceID), realNow)
	if off := clk.Offset(); off > -990*time.Second || off < -1010*time.Second {
		t.Fatalf("learned offset %v, want about -1000 s", off)
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

	peer, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Session with a learned clock offset: %v", err)
	}
	if peer.Device != b.id.DeviceID {
		t.Fatalf("session peer %x, want %x", peer.Device, b.id.DeviceID)
	}
	_ = sess.Close()

	if off := a.rt.Clock().Offset(); off > -990*time.Second || off < -1010*time.Second {
		t.Fatalf("offset after the handshake %v, want about -1000 s", off)
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
