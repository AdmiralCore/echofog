package node_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// pathFeatures offers path-migration on both test runtimes.
func pathFeatures(cfg *node.Config) {
	cfg.Features = []uint32{uint32(frame.FeaturePathMigration)}
}

// count counts events of one kind.
func (c *captureEvents) count(kind node.EventKind) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.list {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// waitPaths waits until both runtimes see the given path IDs for the peer.
func waitPaths(t *testing.T, rt *node.Runtime, peer node.DeviceID, want []uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, _, ok := rt.Paths(peer)
		slices.Sort(ids)
		if ok && slices.Equal(ids, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("paths %v (ok %v), want %v", ids, ok, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestAddPathBindsSecondCarrierAndKeepsTheStandbyLimit pins wire.md Â§8.2â€“Â§8.3 at the runtime
// level: another carrier to the same peer becomes a path, and at most two standby paths stay.
func TestAddPathBindsSecondCarrierAndKeepsTheStandbyLimit(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, pathFeatures)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, pathFeatures)
	b.start(t)
	a.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: first.Client,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	sid := sess.SID()
	waitPaths(t, b.rt, a.id.DeviceID, []uint64{0})

	for _, want := range [][]uint64{{0, 2}, {0, 2, 4}, {0, 4, 6}} {
		pair := memcarrier.NewQUIC()
		queue.push(pair.Server)
		if _, err := a.rt.AddPath(ctx, b.id.DeviceID, node.DialOpt{Carrier: pair.Client}); err != nil {
			t.Fatalf("AddPath: %v", err)
		}
		waitPaths(t, a.rt, b.id.DeviceID, want)
		waitPaths(t, b.rt, a.id.DeviceID, want)
	}
	if sess.SID() != sid {
		t.Fatal("a bound path changed the session ID")
	}
	if err := sess.Err(); err != nil {
		t.Fatalf("session ended: %v", err)
	}
}

// TestNetworkChangedOpensANewPathAndAbandonsTheOld pins the reaction to the NetworkChanged event:
// a new carrier is bound to every live session and the old paths are abandoned (wire.md Â§8.2,
// Â§8.3).
func TestNetworkChangedOpensANewPathAndAbandonsTheOld(t *testing.T) {
	queue := newCarrierQueue()
	events := &captureEvents{}
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, events, pathFeatures)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(cfg *node.Config) {
		cfg.Features = []uint32{uint32(frame.FeaturePathMigration)}
		cfg.Events = events
	})
	b.start(t)
	a.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: first.Client,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	sid := sess.SID()
	waitPaths(t, b.rt, a.id.DeviceID, []uint64{0})

	second := memcarrier.NewQUIC()
	queue.push(second.Server)
	if err := a.rt.NetworkChanged(ctx, node.DialOpt{Carrier: second.Client}); err != nil {
		t.Fatalf("NetworkChanged: %v", err)
	}
	waitPaths(t, a.rt, b.id.DeviceID, []uint64{2})
	waitPaths(t, b.rt, a.id.DeviceID, []uint64{2})
	if sess.SID() != sid {
		t.Fatal("NetworkChanged changed the session ID")
	}
	if err := sess.Err(); err != nil {
		t.Fatalf("the session ended on a network change: %v", err)
	}
	if !events.has(node.EventNetworkChanged) {
		t.Fatal("the operator was not told about the network change")
	}
}

// TestAddPathRefusedUnderLowPower pins platforms.md Â§2: low_power keeps no standby paths.
func TestAddPathRefusedUnderLowPower(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, pathFeatures)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(cfg *node.Config) {
		cfg.Features = []uint32{uint32(frame.FeaturePathMigration)}
		cfg.Profile.LowPower = true
	})
	b.start(t)
	a.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	if _, _, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: first.Client,
	}); err != nil {
		t.Fatalf("Session: %v", err)
	}
	second := memcarrier.NewQUIC()
	if _, err := a.rt.AddPath(ctx, b.id.DeviceID, node.DialOpt{Carrier: second.Client}); !errors.Is(err, node.ErrTooManyPaths) {
		t.Fatalf("AddPath under low_power: %v, want ErrTooManyPaths", err)
	}
	select {
	case <-second.Client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the unused carrier was not closed")
	}
}

// TestNetworkChangedLowPower pins the B1 fix: under low_power a session has no standby paths,
// yet a network change must move it to the new path (the new one is bound and made active, the
// old one is abandoned), not lose the new path and keep the old one.
func TestNetworkChangedLowPower(t *testing.T) {
	queue := newCarrierQueue()
	events := &captureEvents{}
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, func(cfg *node.Config) {
		cfg.Features = []uint32{uint32(frame.FeaturePathMigration)}
		cfg.Profile.LowPower = true
	})
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(cfg *node.Config) {
		cfg.Features = []uint32{uint32(frame.FeaturePathMigration)}
		cfg.Profile.LowPower = true
		cfg.Events = events
	})
	b.start(t)
	a.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: first.Client,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	sid := sess.SID()
	waitPaths(t, b.rt, a.id.DeviceID, []uint64{0})

	second := memcarrier.NewQUIC()
	queue.push(second.Server)
	if err := a.rt.NetworkChanged(ctx, node.DialOpt{Carrier: second.Client}); err != nil {
		t.Fatalf("NetworkChanged under low_power: %v", err)
	}
	waitPaths(t, a.rt, b.id.DeviceID, []uint64{2})
	waitPaths(t, b.rt, a.id.DeviceID, []uint64{2})
	if _, active, ok := a.rt.Paths(b.id.DeviceID); !ok || active != 2 {
		t.Fatalf("initiator active path %d (ok %v), want 2", active, ok)
	}
	if _, active, ok := b.rt.Paths(a.id.DeviceID); !ok || active != 2 {
		t.Fatalf("responder active path %d (ok %v), want 2", active, ok)
	}
	select {
	case <-first.Client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the old carrier is still open")
	}
	if n := events.count(node.EventNetworkChanged); n != 1 {
		t.Fatalf("%d network-changed events, want exactly 1", n)
	}
	if sess.SID() != sid {
		t.Fatal("NetworkChanged changed the session ID")
	}
	if err := sess.Err(); err != nil {
		t.Fatalf("the session ended on a low_power network change: %v", err)
	}
}

// TestAddPathClosesSuppliedCarrierOnEarlyErrors pins resource ownership: when AddPath refuses
// before handing the carrier to addPathTo, it still closes the carrier the caller gave it.
func TestAddPathClosesSuppliedCarrierOnEarlyErrors(t *testing.T) {
	n := newTestNodeWith(t, cards.Hidden, false, nil, nil, pathFeatures)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stopped := memcarrier.NewQUIC()
	peer := node.DeviceID([32]byte{0xab})
	if _, err := n.rt.AddPath(ctx, peer, node.DialOpt{Carrier: stopped.Client}); !errors.Is(err, node.ErrClosed) {
		t.Fatalf("AddPath on a stopped runtime: %v, want ErrClosed", err)
	}
	select {
	case <-stopped.Client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("AddPath leaked the carrier on a stopped runtime")
	}

	n.start(t)
	withoutSession := memcarrier.NewQUIC()
	if _, err := n.rt.AddPath(ctx, peer, node.DialOpt{Carrier: withoutSession.Client}); !errors.Is(err, node.ErrNoSession) {
		t.Fatalf("AddPath without a session: %v, want ErrNoSession", err)
	}
	select {
	case <-withoutSession.Client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("AddPath leaked the carrier without a session")
	}
}
