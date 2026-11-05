package session_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestDialAndAcceptLowPower: the platform profile's low_power flag reaches the mux on both sides
// of a handshake (platforms.md §2).
func TestDialAndAcceptLowPower(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aL := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bL := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	pair := memcarrier.NewQUIC()
	est, srv := establishWith(t, pair, verifyCard(t, bc.peer, now), aL,
		session.AcceptConfig{Local: bL, LowPower: true}, session.WithLowPower(true))

	if !est.Session.LowPower() {
		t.Fatal("dialing side is not low power")
	}
	if !srv.Session.LowPower() {
		t.Fatal("accepting side is not low power")
	}
}

// TestTableSessionsSnapshot: the table exposes its live sessions so the runtime can apply a
// background transition to all of them.
func TestTableSessionsSnapshot(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aL := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bL := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	pair := memcarrier.NewQUIC()
	est, _ := establishWith(t, pair, verifyCard(t, bc.peer, now), aL, session.AcceptConfig{Local: bL})

	tbl, err := session.NewTable(session.TableConfig{MaxSessions: 8})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	if got := tbl.Sessions(); len(got) != 0 {
		t.Fatalf("new table has %d sessions", len(got))
	}
	if _, err := tbl.Add(aL, est); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := tbl.Sessions()
	if len(got) != 1 || got[0] != est {
		t.Fatalf("Sessions() = %d entries, want the added session", len(got))
	}
	if max := tbl.Max(); max != 8 {
		t.Fatalf("Max() = %d, want 8", max)
	}
	_ = tbl.Close()
	if got := tbl.Sessions(); len(got) != 0 {
		t.Fatalf("closed table has %d sessions", len(got))
	}
}
