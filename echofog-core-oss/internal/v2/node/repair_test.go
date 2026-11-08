package node_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// T3.13: a session left with a failing only path is repaired at once, not after the path is dead.

// TestFailingOnlyPathIsReplacedByANewPath: with path-migration the dialer binds a second carrier and
// makes it the active path.
func TestFailingOnlyPathIsReplacedByANewPath(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, pathFeatures)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, pathFeatures)
	b.start(t)
	a.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: first.Client})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	waitPaths(t, b.rt, a.id.DeviceID, []uint64{0})

	node.SetRepairOpt(a.rt, node.DialOpt{CarrierFunc: func(context.Context) (mux.Carrier, error) {
		pair := memcarrier.NewQUIC()
		queue.push(pair.Server)
		return pair.Client, nil
	}})
	node.TriggerPathFailing(a.rt, sess)

	waitPaths(t, a.rt, b.id.DeviceID, []uint64{0, 2})
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, active, _ := a.rt.Paths(b.id.DeviceID)
		if active == 2 && slices.Contains(ids, 2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("active path %d of %v, want the new path 2", active, ids)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := sess.Err(); err != nil {
		t.Fatalf("the session did not survive its repair: %v", err)
	}
}

// TestFailingOnlyPathWithoutMigrationIsReplacedByANewSession: without path-migration the session
// cannot take another carrier, so the dialer ends it and dials a new one.
func TestFailingOnlyPathWithoutMigrationIsReplacedByANewSession(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, nil)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, nil)
	b.start(t)
	a.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	_, old, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: first.Client})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	node.SetRepairOpt(a.rt, node.DialOpt{Card: b.peerCard(t), CarrierFunc: func(context.Context) (mux.Carrier, error) {
		pair := memcarrier.NewQUIC()
		queue.push(pair.Server)
		return pair.Client, nil
	}})
	node.TriggerPathFailing(a.rt, old)

	select {
	case <-old.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session with the failing path was not ended")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, est := range node.TableSessions(a.rt) {
			select {
			case <-est.Session.Done():
			default:
				if est.SID != old.SID() {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no new session was dialed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session dialed with an explicit card (an invitation, a contact link) has no card in the card
// book; the repair has to dial with the card the session was opened with. The lab found it: the
// repair failed with "no card for the peer device" in every loaded run.
func TestRepairDialsWithTheCardTheSessionWasOpenedWith(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, pathFeatures)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, pathFeatures)
	b.start(t)
	a.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	card := b.peerCard(t)
	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: card, Carrier: first.Client})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	got := node.RepairCardOf(a.rt, sess.SID())
	if got == nil || got.DeviceID != card.DeviceID {
		t.Fatalf("repair card %v, want the card of the dial", got)
	}
	sess.Close()
	deadline := time.Now().Add(5 * time.Second)
	for node.RepairCardOf(a.rt, sess.SID()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the card of a closed session is still kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When the peer refuses the new path (it lost the session during the outage: the lab saw
// "PATH_BIND refused" six times in a row), the dialer gives up on the session and dials a new one.
func TestRepairFallsBackToANewSessionWhenThePeerRefusesPaths(t *testing.T) {
	queue := newCarrierQueue()
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, pathFeatures)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, pathFeatures)
	b.start(t)
	a.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first := memcarrier.NewQUIC()
	queue.push(first.Server)
	_, old, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: first.Client})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	var calls atomic.Int64
	node.SetRepairOpt(a.rt, node.DialOpt{Card: b.peerCard(t), CarrierFunc: func(context.Context) (mux.Carrier, error) {
		if calls.Add(1) <= 2 { // the carrier to the peer cannot be bound
			return nil, errors.New("the peer refused")
		}
		pair := memcarrier.NewQUIC()
		queue.push(pair.Server)
		return pair.Client, nil
	}})
	node.SetHealthy(a.rt, func(*mux.Session) bool { return false }) // the path stays failing
	node.TriggerPathFailing(a.rt, old)

	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, est := range node.TableSessions(a.rt) {
			select {
			case <-est.Session.Done():
			default:
				if est.SID != old.SID() {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new session after %d failed path attempts", calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
