package mux

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// T3.11: the initiator may open service streams the moment message 3 is written, before the
// responder has attached its Guard to a session. wire.md §6.2 resets a stream opened before the
// handshake completed; the window between "the responder answered message 1" and "the session is
// bound" is part of the handshake, so the Guard holds such streams (bounded, with a deadline)
// instead of refusing an honest peer.

// manualAfter is an injected timer source (AD13): timers fire only when the test says so.
type manualAfter struct {
	mu     sync.Mutex
	timers []*manualTimer
}

type manualTimer struct {
	f       func()
	stopped bool
	fired   bool
	d       time.Duration
}

func (m *manualAfter) after(d time.Duration, f func()) func() bool {
	t := &manualTimer{f: f, d: d}
	m.mu.Lock()
	m.timers = append(m.timers, t)
	m.mu.Unlock()
	return func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		was := !t.stopped && !t.fired
		t.stopped = true
		return was
	}
}

// fire runs every pending timer.
func (m *manualAfter) fire() {
	m.mu.Lock()
	var run []*manualTimer
	for _, t := range m.timers {
		if !t.stopped && !t.fired {
			t.fired = true
			run = append(run, t)
		}
	}
	m.mu.Unlock()
	for _, t := range run {
		t.f()
	}
}

// handshaken opens stream 0 with an HS_HELLO, hands it to the responder side and returns the
// pieces of a session that is not yet bound on the responder.
type halfOpen struct {
	e    *carrierEnds
	pend *Pending
	a    *Session
}

func newHalfOpen(t *testing.T, hold HoldConfig) *halfOpen {
	t.Helper()
	ik, _ := vectorKeys(t)
	e := newCarrier(t, false)
	ctx := ctxT(t)
	raw0, err := e.ca.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}})
	if _, err := raw0.Write(hello); err != nil {
		t.Fatal(err)
	}
	binder := NewBinder()
	_, pend, err := binder.Accept(ctx, e.gb, PathDirect)
	if !errors.Is(err, ErrNewSession) {
		t.Fatalf("binder: %v", err)
	}
	if hold.MaxStreams >= 0 {
		pend.Guard.Hold(hold)
	}
	a, err := NewSession(Config{Keys: ik, Policy: chatPolicy}, e.ga, raw0, PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return &halfOpen{e: e, pend: pend, a: a}
}

func (h *halfOpen) bindResponder(t *testing.T) *Session {
	t.Helper()
	_, rk := vectorKeys(t)
	b, err := NewSession(Config{Keys: rk, Policy: chatPolicy}, h.pend.Guard, h.pend.Stream, PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func waitHeld(t *testing.T, g *Guard, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for g.Held() != want {
		if time.Now().After(deadline) {
			t.Fatalf("guard holds %d streams, want %d", g.Held(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGuardHeldStreamIsServedOnceTheSessionIsBound(t *testing.T) {
	h := newHalfOpen(t, HoldConfig{})
	ctx := ctxT(t)
	st, err := h.a.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(0x40, 0, deliver(1, "early")); err != nil {
		t.Fatal(err)
	}
	waitHeld(t, h.pend.Guard, 1)
	if h.pend.Guard.Rejected() != 0 {
		t.Fatal("an honest stream opened right after message 3 was refused")
	}
	b := h.bindResponder(t)
	rs, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("held stream lost: %v", err)
	}
	f, err := rs.ReadFrame()
	if err != nil || f.Type != 0x40 {
		t.Fatalf("held stream data: %+v %v", f, err)
	}
	if h.pend.Guard.Held() != 0 {
		t.Fatal("the queue still holds a stream after bind")
	}
}

// Without Hold the rule of wire.md §6.2 stands: a stream before the session is refused.
func TestGuardWithoutHoldStillRefusesEarlyStreams(t *testing.T) {
	h := newHalfOpen(t, HoldConfig{MaxStreams: -1})
	ctx := ctxT(t)
	raw, err := h.e.ca.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: frame.Open{Service: 3, Version: 2}.Append(nil)}))
	f, err := frame.NewReader(raw, nil).ReadFrame()
	if err != nil || f.Type != frame.TypeClose {
		t.Fatalf("early stream answered %+v %v, want CLOSE", f, err)
	}
}

func TestGuardHoldIsBoundedByCount(t *testing.T) {
	h := newHalfOpen(t, HoldConfig{MaxStreams: 2})
	ctx := ctxT(t)
	var raws []RawStream
	for i := 0; i < 4; i++ {
		raw, err := h.e.ca.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: frame.Open{Service: 3, Version: 2}.Append(nil)}))
		raws = append(raws, raw)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.pend.Guard.Rejected() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("rejected %d, want 2 beyond the cap", h.pend.Guard.Rejected())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.pend.Guard.Held(); got != 2 {
		t.Fatalf("held %d, want the cap 2", got)
	}
	// The refused ones got CLOSE{protocol-violation}.
	f, err := frame.NewReader(raws[3], nil).ReadFrame()
	if err != nil || f.Type != frame.TypeClose {
		t.Fatalf("stream over the cap answered %+v %v", f, err)
	}
}

func TestGuardHoldExpiresOnTheInjectedClock(t *testing.T) {
	var clk manualAfter
	h := newHalfOpen(t, HoldConfig{For: 10 * time.Second, After: clk.after})
	ctx := ctxT(t)
	raw, err := h.e.ca.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: frame.Open{Service: 3, Version: 2}.Append(nil)}))
	waitHeld(t, h.pend.Guard, 1)
	clk.fire() // the session was never bound
	if h.pend.Guard.Held() != 0 {
		t.Fatal("the queue was not dropped when the hold expired")
	}
	f, err := frame.NewReader(raw, nil).ReadFrame()
	if err != nil || f.Type != frame.TypeClose {
		t.Fatalf("expired stream answered %+v %v, want CLOSE", f, err)
	}
	// After the expiry nothing more is held: wire.md §6.2 applies again.
	late, err := h.e.ca.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	late.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: frame.Open{Service: 3, Version: 2}.Append(nil)}))
	f, err = frame.NewReader(late, nil).ReadFrame()
	if err != nil || f.Type != frame.TypeClose {
		t.Fatalf("late stream answered %+v %v", f, err)
	}
	if h.pend.Guard.Held() != 0 {
		t.Fatal("a stream was held after the expiry")
	}
}

// A bind that never comes must not keep memory: closing the carrier drops the queue and stops the
// timer.
func TestGuardHoldIsDroppedWhenTheCarrierCloses(t *testing.T) {
	var clk manualAfter
	h := newHalfOpen(t, HoldConfig{After: clk.after})
	ctx := ctxT(t)
	raw, err := h.e.ca.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: frame.Open{Service: 3, Version: 2}.Append(nil)}))
	waitHeld(t, h.pend.Guard, 1)
	h.e.cb.Close()
	waitHeld(t, h.pend.Guard, 0)
	clk.mu.Lock()
	defer clk.mu.Unlock()
	for _, tm := range clk.timers {
		if !tm.stopped && !tm.fired {
			t.Fatal("the hold timer is still pending after the carrier closed")
		}
	}
}
