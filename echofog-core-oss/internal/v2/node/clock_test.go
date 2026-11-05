package node_test

import (
	"context"
	"sync"
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
