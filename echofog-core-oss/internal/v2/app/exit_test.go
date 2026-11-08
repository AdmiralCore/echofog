package app_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/node"
)

// fakeExitHost is the part of the runtime that runs an exit, without a network.
type fakeExitHost struct {
	mu       sync.Mutex
	enabled  bool
	opts     node.ExitOptions
	status   node.ExitStatus
	rechecks int
}

func (h *fakeExitHost) EnableExit(o node.ExitOptions) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled, h.opts = true, o
	return nil
}

func (h *fakeExitHost) DisableExit() {
	h.mu.Lock()
	h.enabled = false
	h.mu.Unlock()
}

func (h *fakeExitHost) RecheckExit() {
	h.mu.Lock()
	h.rechecks++
	h.mu.Unlock()
}

func (h *fakeExitHost) ExitStatus() node.ExitStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.status
	st.Enabled = h.enabled
	return st
}

func (h *fakeExitHost) options() node.ExitOptions {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.opts
}

func (h *fakeExitHost) setStatus(st node.ExitStatus) {
	h.mu.Lock()
	h.status = st
	h.mu.Unlock()
}

func newExitHostRig(t *testing.T) (*app.App, *fakeExitHost) {
	t.Helper()
	r := newRig(t, app.ModePublic)
	h := &fakeExitHost{}
	a, err := app.New(app.Config{Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: clock{}, Self: nid(1), ExitHost: h})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, h
}

// The open access is a privilege of a running exit: with the exit off there is nothing to open.
func TestTheOpenWindowNeedsTheExitOn(t *testing.T) {
	a, _ := newExitHostRig(t)
	if _, err := a.ExitAllowAny(context.Background(), time.Minute); !errors.Is(err, app.ErrExitNotOn) {
		t.Fatalf("ExitAllowAny with the exit off = %v, want ErrExitNotOn", err)
	}
}

// Turning the exit off takes the open window back, and turning it on again must not bring the old
// window with it: the owner opens it each time on purpose.
func TestTurningTheExitOffTakesTheOpenWindowBack(t *testing.T) {
	a, h := newExitHostRig(t)
	ctx := context.Background()
	if _, err := a.ExitEnable(ctx, app.ExitSettings{}); err != nil {
		t.Fatalf("ExitEnable: %v", err)
	}
	open, err := a.ExitAllowAny(ctx, time.Minute)
	if err != nil || open.Until == 0 {
		t.Fatalf("ExitAllowAny = %+v, %v", open, err)
	}
	if err := a.ExitDisable(); err != nil {
		t.Fatalf("ExitDisable: %v", err)
	}
	if _, err := a.ExitEnable(ctx, app.ExitSettings{}); err != nil {
		t.Fatalf("ExitEnable again: %v", err)
	}
	if got := a.ExitStatus().AnyUntil; got != 0 {
		t.Fatalf("the open window came back after a restart of the exit: until %d", got)
	}
	o := h.options()
	if o.Admit == nil || o.Admit(nid(2), netip.AddrPort{}) {
		t.Fatal("the exit admits a stranger after the window was taken back")
	}
}
