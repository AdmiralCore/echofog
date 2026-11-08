package app_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/store"
)

func xid(n int) [32]byte { var b [32]byte; b[0], b[31] = byte(n), byte(n>>8); return b }
func xhex(n int) string  { b := xid(n); return fmt.Sprintf("%x", b[:]) }

func newAccess(t *testing.T) (*app.ExitAccess, *tclock, store.KV) {
	t.Helper()
	clk := &tclock{t: testNow}
	kv := newKV()
	return app.NewExitAccess(clk, kv), clk, kv
}

func TestUnknownIdentityIsRefusedAndQueued(t *testing.T) {
	x, _, _ := newAccess(t)
	if x.Admit(xid(1)) {
		t.Fatal("an unknown identity is admitted")
	}
	x.Refused(xid(1), "198.51.100.1")
	x.Refused(xid(1), "198.51.100.1") // the same client again: one entry, counted
	p := x.Pending()
	if len(p) != 1 || p[0].ID != xhex(1) || p[0].Tries != 2 {
		t.Fatalf("queue %+v", p)
	}
}

func TestApproveAdmitsAndIsRemembered(t *testing.T) {
	x, clk, kv := newAccess(t)
	x.Refused(xid(1), "198.51.100.1")
	if err := x.Approve(xhex(1), "Anna"); err != nil {
		t.Fatal(err)
	}
	if !x.Admit(xid(1)) {
		t.Fatal("a confirmed identity is refused")
	}
	if len(x.Pending()) != 0 {
		t.Fatal("still waiting after the confirmation")
	}
	// a new start: the confirmation is remembered, the queue is not
	x2 := app.NewExitAccess(clk, kv)
	if !x2.Admit(xid(1)) {
		t.Fatal("the confirmation was lost with the restart")
	}
	if a := x2.Approved(); len(a) != 1 || a[0].Label != "Anna" {
		t.Fatalf("approved %+v", a)
	}
	if err := x2.Revoke(xhex(1)); err != nil {
		t.Fatal(err)
	}
	if x2.Admit(xid(1)) {
		t.Fatal("a revoked identity is admitted")
	}
	if app.NewExitAccess(clk, kv).Admit(xid(1)) {
		t.Fatal("the revocation was lost with the restart")
	}
	if err := x2.Revoke(xhex(1)); err == nil {
		t.Fatal("revoking nothing is not an error the owner should miss")
	}
}

func TestRejectRemovesFromTheQueue(t *testing.T) {
	x, _, _ := newAccess(t)
	x.Refused(xid(1), "198.51.100.1")
	if err := x.Reject(xhex(1)); err != nil {
		t.Fatal(err)
	}
	if len(x.Pending()) != 0 || x.Admit(xid(1)) {
		t.Fatal("rejected client is still waiting or admitted")
	}
	if err := x.Approve(xhex(1), "x"); err == nil {
		t.Fatal("approved a client that is not waiting")
	}
}

// The queue has a ceiling, a limit per source and a lifetime; nothing grows without bound.
func TestQueueIsBounded(t *testing.T) {
	x, clk, _ := newAccess(t)
	for i := 1; i <= app.ExitPendingMax+10; i++ {
		x.Refused(xid(i), fmt.Sprintf("203.0.113.%d", i)) // one source each
	}
	if n := len(x.Pending()); n != app.ExitPendingMax {
		t.Fatalf("queue %d, want the ceiling %d", n, app.ExitPendingMax)
	}
	// the earliest stay: a flood cannot push the real client out
	if x.Pending()[0].ID != xhex(1) {
		t.Fatal("the first waiting client was pushed out by later ones")
	}
	// the queue empties by lifetime
	clk.Advance(time.Duration(app.ExitPendingTTL+1) * time.Second)
	if n := len(x.Pending()); n != 0 {
		t.Fatalf("%d entries after the lifetime", n)
	}
	x.Refused(xid(99), "203.0.113.99")
	if len(x.Pending()) != 1 {
		t.Fatal("an expired queue did not make room")
	}
}

func TestOneSourceCannotFillTheQueue(t *testing.T) {
	x, _, _ := newAccess(t)
	for i := 1; i <= 10; i++ {
		x.Refused(xid(i), "198.51.100.7")
	}
	if n := len(x.Pending()); n != app.ExitPendingPerSource {
		t.Fatalf("one source holds %d entries, want at most %d", n, app.ExitPendingPerSource)
	}
	x.Refused(xid(50), "198.51.100.8")
	if len(x.Pending()) != app.ExitPendingPerSource+1 {
		t.Fatal("another source was blocked by the first")
	}
}

func TestOpenAccessHasAnEndAndCanBeTakenBack(t *testing.T) {
	x, clk, _ := newAccess(t)
	if x.AnyUntil() != 0 || x.Admit(xid(1)) {
		t.Fatal("open access without being asked for")
	}
	until := x.AllowAny(60)
	if until != testNow.Add(60*time.Second).Unix() || x.AnyUntil() != until {
		t.Fatalf("until %d", until)
	}
	if !x.Admit(xid(1)) || !x.Admit(xid(2)) {
		t.Fatal("not open during the window")
	}
	clk.Advance(59 * time.Second)
	if !x.Admit(xid(3)) {
		t.Fatal("closed early")
	}
	clk.Advance(2 * time.Second)
	if x.Admit(xid(3)) || x.AnyUntil() != 0 {
		t.Fatal("still open after the end")
	}
	x.AllowAny(60)
	x.RevokeAny()
	if x.Admit(xid(4)) || x.AnyUntil() != 0 {
		t.Fatal("still open after being taken back")
	}
	if x.AllowAny(0) != 0 || x.AllowAny(-5) != 0 {
		t.Fatal("a window without a positive time was opened")
	}
}

func TestBadIdsAreErrors(t *testing.T) {
	x, _, _ := newAccess(t)
	for _, id := range []string{"", "zz", xhex(1)[:10]} {
		if err := x.Approve(id, "x"); err == nil {
			t.Errorf("Approve(%q) accepted", id)
		}
		if err := x.Reject(id); err == nil {
			t.Errorf("Reject(%q) accepted", id)
		}
		if err := x.Revoke(id); err == nil {
			t.Errorf("Revoke(%q) accepted", id)
		}
	}
	x.Refused(xid(1), "198.51.100.1")
	if err := x.Approve(xhex(1), strings.Repeat("a", 300)); err == nil {
		t.Error("a label of 300 characters was accepted")
	}
}
