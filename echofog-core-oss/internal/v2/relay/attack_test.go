package relay

import (
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// H1 (v1): INCOMING carried an unsigned "source" that any neighbour could set. In v2 the source
// is the node R authenticated on its own session with A.
func TestIncomingSourceIsTheAuthenticatedNode(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a, b, c := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
	circ, err := a.dial(b)
	if err != nil {
		t.Fatal(err)
	}
	defer circ.Close()
	in := <-b.in
	if string(in.Incoming.Source) != string(a.node[:]) {
		t.Fatal("INCOMING.source is not the node R authenticated on A's session")
	}
	if string(in.Incoming.Target) != string(b.node[:]) || in.Incoming.CID != circ.CID() {
		t.Fatal("INCOMING does not carry the target and the single circuit ID")
	}
	_ = c
}

// A target that answers ACCEPT for another circuit ID does not get A connected.
func TestAcceptWithWrongCIDIsRefused(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	w.raw[b.node] = func(st *mux.Stream) {
		st.SetLimiter(limiter())
		f, err := st.ReadFrame()
		if err != nil {
			return
		}
		in, _ := ParseIncoming(f.Payload)
		_ = st.WriteFrame(TypeAccept, 0, Accept{CID: in.CID + 1}.Append(nil))
	}
	_, err := a.dial(b)
	var re *RefusedError
	if !errors.As(err, &re) || re.Reason != ReasonUnavailable {
		t.Fatalf("got %v, want REFUSED unavailable", err)
	}
}

// A node that guesses the circuit ID of two other nodes cannot inject datagrams into it, and the
// real ends still talk (route capture).
func TestDatagramCircuitCannotBeHijacked(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a, b, c := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
	ctx := w.ctx
	da, err := DialDatagram(ctx, a.open, a.hub, func(typ uint64, p []byte) error { return a.toR.SendDatagram(typ, p) }, w.rNode, b.node, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	in := <-b.in
	if in.Datagram == nil {
		t.Fatal("no datagram circuit at B")
	}
	// The attacker C sends RELAY_DGRAM with the right cid.
	evil := frame.AppendVarint(nil, da.CID())
	evil = append(evil, "forged"...)
	if err := c.toR.SendDatagram(frame.DgramRelay, evil); err != nil {
		t.Fatal(err)
	}
	if err := da.Send([]byte("genuine")); err != nil {
		t.Fatal(err)
	}
	got, err := in.Datagram.Receive(ctx, nil)
	if err != nil || string(got) != "genuine" {
		t.Fatalf("B got %q, %v; the forged datagram must not arrive first", got, err)
	}
	waitFor(t, func() bool { return w.srv.Stats().DatagramsDropped.Load() >= 1 })
	if w.srv.Stats().Forwarded.Load() != 1 {
		t.Fatalf("forwarded %d, want 1", w.srv.Stats().Forwarded.Load())
	}
	// And back.
	if err := in.Datagram.Send([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if got, err := da.Receive(ctx, nil); err != nil || string(got) != "reply" {
		t.Fatalf("A got %q, %v", got, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// Unknown, unlisted, refusing and silent targets are indistinguishable: the same reason, at the
// same time from the CONNECT (§6.3).
func TestRefusalsAreUniform(t *testing.T) {
	const delay = 400 * time.Millisecond
	w := newWorld(t, ServerConfig{Delay: func() time.Duration { return delay }})
	a := w.join(newTNode(t), "10.0.0.1", true)
	unlisted := w.join(newTNode(t), "10.0.0.2", false)
	refusing := w.join(newTNode(t), "10.0.0.3", true)
	refusing.pol = func(Incoming) bool { return false }
	silent := w.join(newTNode(t), "10.0.0.4", true)
	w.silent[silent.node] = true
	var unknown [32]byte
	unknown[0] = 9

	cases := map[string][32]byte{"unknown": unknown, "unlisted": unlisted.node, "refusing": refusing.node,
		"silent": silent.node, "self": a.node}
	for name, target := range cases {
		start := time.Now()
		_, err := Dial(w.ctx, a.open, w.rNode, target, ConnectOptions{})
		took := time.Since(start)
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != ReasonUnavailable {
			t.Fatalf("%s: got %v, want REFUSED unavailable", name, err)
		}
		if took < delay-20*time.Millisecond || took > delay+250*time.Millisecond {
			t.Fatalf("%s: refused after %v, want about %v", name, took, delay)
		}
	}
}

func TestCircuitLimits(t *testing.T) {
	t.Run("per session is a limit refusal", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Limits: Limits{PerSession: 2}})
		a := w.join(newTNode(t), "10.0.0.1", true)
		b, c, d := w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true), w.join(newTNode(t), "10.0.0.4", true)
		for _, x := range []*side{b, c} {
			if _, err := a.dial(x); err != nil {
				t.Fatal(err)
			}
		}
		_, err := a.dial(d)
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != ReasonLimit {
			t.Fatalf("got %v, want REFUSED limit", err)
		}
	})
	t.Run("per source IP", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Limits: Limits{PerIP: 1}})
		a1, a2 := w.join(newTNode(t), "10.0.0.9", true), w.join(newTNode(t), "10.0.0.9", true)
		b, c := w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
		if _, err := a1.dial(b); err != nil {
			t.Fatal(err)
		}
		_, err := a2.dial(c)
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != ReasonLimit {
			t.Fatalf("got %v, want REFUSED limit", err)
		}
	})
	t.Run("total", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Limits: Limits{Total: 1}})
		a, c := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.5", true)
		b, d := w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
		if _, err := a.dial(b); err != nil {
			t.Fatal(err)
		}
		_, err := c.dial(d)
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != ReasonLimit {
			t.Fatalf("got %v, want REFUSED limit", err)
		}
	})
	t.Run("full target looks like an absent one", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Limits: Limits{PerSession: 1}})
		a, c := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.5", true)
		b := w.join(newTNode(t), "10.0.0.2", true)
		if _, err := a.dial(b); err != nil {
			t.Fatal(err)
		}
		_, err := c.dial(b)
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != ReasonUnavailable {
			t.Fatalf("got %v, want REFUSED unavailable", err)
		}
	})
	t.Run("slots come back", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Limits: Limits{PerSession: 1}})
		a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
		circ, err := a.dial(b)
		if err != nil {
			t.Fatal(err)
		}
		in := <-b.in
		_ = circ.Close()
		_ = in.Stream.Close()
		waitFor(t, func() bool { return w.srv.ActiveCircuits() == 0 })
		if _, err := a.dial(b); err != nil {
			t.Fatalf("slot not released: %v", err)
		}
	})
}

func TestCircuitByteAndTimeCaps(t *testing.T) {
	junk := make([]byte, 4096)
	for name, tc := range map[string]struct {
		lim  Limits
		mode uint32
	}{
		"per circuit": {Limits{PerCircuit: 3000}, ModeStream},
		"signal":      {Limits{SignalBytes: 3000}, ModeSignal},
		"idle":        {Limits{Idle: 200 * time.Millisecond}, ModeStream},
		"signal time": {Limits{SignalTime: 200 * time.Millisecond}, ModeSignal},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, ServerConfig{Limits: tc.lim})
			a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
			circ, err := Dial(w.ctx, a.open, w.rNode, b.node, ConnectOptions{Mode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			<-b.in
			if name == "per circuit" || name == "signal" {
				_, _ = circ.pipe.Write(junk)
			}
			waitFor(t, func() bool { return w.srv.ActiveCircuits() == 0 })
		})
	}
}

func TestMalformedFramesAreViolations(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a := w.join(newTNode(t), "10.0.0.1", true)
	for name, f := range map[string]struct {
		typ     uint64
		payload []byte
	}{
		"truncated connect": {TypeConnect, []byte{0x12, 0x20, 1}},
		"short target":      {TypeConnect, Connect{Req: 1, Target: []byte{1, 2}, Mode: 1}.Append(nil)},
		"bad mode":          {TypeConnect, Connect{Req: 1, Target: make([]byte, 32), Mode: 9}.Append(nil)},
		"accept from A":     {TypeAccept, Accept{CID: 1}.Append(nil)},
	} {
		st, err := a.open(w.ctx)
		if err != nil {
			t.Fatal(err)
		}
		st.SetLimiter(func(uint64) (int, bool) { return 1 << 16, true })
		before := w.srv.Stats().Violations.Load()
		_ = st.WriteFrame(f.typ, 0, f.payload)
		waitFor(t, func() bool { return w.srv.Stats().Violations.Load() > before })
		_ = name
	}
}

func TestMemCapLimitsCircuits(t *testing.T) {
	l := Limits{MemCapMB: 40}
	l.fill()
	if l.Total != 16 || l.PerSession != 16 {
		t.Fatalf("mem_cap_mb node: total %d per session %d, want at most 16", l.Total, l.PerSession)
	}
	var d Limits
	d.fill()
	if d.Total != 200 || d.PerSession != 16 || d.PerIP != 32 || d.MaxRendezvous != 256 {
		t.Fatalf("defaults %+v", d)
	}
}

// H1 (accept 1): the requester's own limits are checked and reserved before the target is
// looked up, so a requester at a limit gets the same answer, at the same time, whether the target
// exists or not. The prober learns nothing about presence (§6.3).
func TestLimitRefusalDoesNotDependOnTarget(t *testing.T) {
	probe := func(t *testing.T, a *side, target [32]byte) (uint32, time.Duration) {
		t.Helper()
		start := time.Now()
		_, err := Dial(a.w.ctx, a.open, a.w.rNode, target, ConnectOptions{})
		var re *RefusedError
		if !errors.As(err, &re) {
			t.Fatalf("got %v, want a REFUSED", err)
		}
		return re.Reason, time.Since(start)
	}
	var unknown [32]byte
	unknown[0] = 7
	check := func(t *testing.T, a, exists *side) {
		t.Helper()
		r1, d1 := probe(t, a, exists.node)
		r2, d2 := probe(t, a, unknown)
		if r1 != r2 {
			t.Fatalf("existing target: reason %d, absent target: reason %d", r1, r2)
		}
		if d1 > d2+150*time.Millisecond || d2 > d1+150*time.Millisecond {
			t.Fatalf("existing target answered after %v, absent after %v", d1, d2)
		}
	}
	const delay = 600 * time.Millisecond
	t.Run("per session", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Delay: func() time.Duration { return delay }, Limits: Limits{PerSession: 1}})
		a, b, c := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
		if _, err := a.dial(b); err != nil {
			t.Fatal(err)
		}
		check(t, a, c)
	})
	t.Run("per ip", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Delay: func() time.Duration { return delay }, Limits: Limits{PerIP: 1}})
		a1, a2 := w.join(newTNode(t), "10.0.0.9", true), w.join(newTNode(t), "10.0.0.9", true)
		b, c := w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
		if _, err := a1.dial(b); err != nil {
			t.Fatal(err)
		}
		check(t, a2, c)
	})
	t.Run("total", func(t *testing.T) {
		w := newWorld(t, ServerConfig{Delay: func() time.Duration { return delay }, Limits: Limits{Total: 1}})
		x, a := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.5", true)
		b, c := w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true)
		if _, err := x.dial(b); err != nil {
			t.Fatal(err)
		}
		check(t, a, c)
	})
}
