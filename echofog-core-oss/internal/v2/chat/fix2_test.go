package chat

import (
	"context"
	"runtime"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// ---- finding A: the reader of a stream ends with the handler ----------------------------------------

func settledGoroutines(t *testing.T, want int) int {
	t.Helper()
	var n int
	for i := 0; i < 100; i++ {
		n = runtime.NumGoroutine()
		if n <= want {
			return n
		}
		time.Sleep(30 * time.Millisecond)
	}
	return n
}

// Every way the server ends a stream by itself leaves no goroutine behind, with a handler context
// that lives as long as the node (the runtime passes one).
func TestHandleLeavesNoGoroutineWhenItClosesTheStream(t *testing.T) {
	cases := []struct {
		name  string
		peer  Peer
		cfg   func(*Config)
		frame func() (frame.Frame, bool)
	}{
		{"idle", direct("203.0.113.5:4430"), func(c *Config) {
			c.After = func(time.Duration) <-chan time.Time { ch := make(chan time.Time, 1); ch <- time.Time{}; return ch }
		}, func() (frame.Frame, bool) { return frame.Frame{}, false }},
		{"unknown frame", direct("203.0.113.5:4430"), nil, func() (frame.Frame, bool) { return frame.Frame{Type: 0x7f}, true }},
		{"refused BUNDLE_GET", Peer{SID: [16]byte{9}, Path: mux.PathRelay}, nil, func() (frame.Frame, bool) { return get(t, 1), true }},
		{"malformed DELIVER", direct("203.0.113.5:4430"), nil, func() (frame.Frame, bool) {
			return frame.Frame{Type: FrameDeliver, Payload: []byte{0xff}}, true
		}},
	}
	for _, tc := range cases {
		cfg := Config{Bundles: &bundler{}, Receiver: &receiver{}, Now: (&clock{t: time.Unix(1_800_000_000, 0)}).Now}
		if tc.cfg != nil {
			tc.cfg(&cfg)
		}
		srv := New(cfg)
		ctx := context.Background() // never cancelled, like the context of a node
		base := settledGoroutines(t, 0)
		for i := 0; i < 100; i++ {
			st := newStream()
			if f, ok := tc.frame(); ok {
				st.in <- f
			}
			done := make(chan struct{})
			go func() { srv.Handle(ctx, tc.peer, st); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: Handle did not return", tc.name)
			}
		}
		if n := settledGoroutines(t, base+5); n > base+5 {
			t.Fatalf("%s: %d goroutines after 100 closed streams, %d before", tc.name, n, base)
		}
	}
}

func TestHandleLeavesNoGoroutineAfterTheRateLimit(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Receiver: &receiver{}, Now: c.Now})
	base := settledGoroutines(t, 0)
	for i := 0; i < 20; i++ {
		st := newStream()
		done := make(chan struct{})
		sid := [16]byte{byte(i + 1)}
		go func() { srv.Handle(context.Background(), Peer{SID: sid}, st); close(done) }()
		for j := 0; j < DeliverRate+1; j++ { // the last one is over the limit; the clock does not move
			select {
			case st.in <- deliver(t, uint64(j+1), []byte("e")):
			case <-done:
			}
		}
		<-done
		if st.code != frame.CodeOverloaded {
			t.Fatalf("code %d", st.code)
		}
	}
	if n := settledGoroutines(t, base+5); n > base+5 {
		t.Fatalf("%d goroutines after 20 limited streams, %d before", n, base)
	}
}

// ---- finding B: recovery only for devices a session was with ------------------------------------------

func bentFrom(t *testing.T, from, to *peer, claimAccount []byte, claimDevice []byte) []byte {
	t.Helper()
	m := mustEnc(t, from, to, "bent")
	m[len(m)-3] ^= 0x55
	return craft(t, to, from, claimAccount, claimDevice, m, false)
}

// A device we have a session with, but named by a different account: an impersonation. It never
// starts a recovery, however many envelopes.
func TestForgedAccountOfAKnownDeviceStartsNoRecovery(t *testing.T) {
	w := newWorld(t)
	a, b, c := w.newPeer(), w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	for i := 0; i < 8; i++ {
		b.svc.Receive(context.Background(), bentFrom(t, a, b, c.self.Account, a.self.DeviceKey))
	}
	// (b has one receipt queued for a, the honest message; nothing is queued for c)
	if st := b.svc.Stats(); st.PerPeer[c.self.Node] != 0 {
		t.Fatalf("%d messages queued for c by forged envelopes", st.PerPeer[c.self.Node])
	}
	w.tick(b)
	if len(c.ctrl) != 0 {
		t.Fatal("c was sent a reset because of forged envelopes")
	}
}

// Refusals of a device we have no session with are unauthenticated: however many arrive, they never
// start a recovery or replace a session (ЧМ-1).
func TestUnauthenticatedFailuresStartNoRecovery(t *testing.T) {
	w := newWorld(t)
	b, c := w.newPeer(), w.newPeer()
	storm := func(n int) {
		dev := fakeDevice()
		for i := 0; i < n; i++ {
			env := craft(t, b, c, c.self.Account, dev, []byte("some ratchet bytes of enough length to parse...."), false)
			b.svc.Receive(context.Background(), env)
		}
	}
	// a hundred devices with two envelopes each, then six with three: none is authenticated
	for i := 0; i < 100; i++ {
		storm(2)
	}
	for i := 0; i < 6; i++ {
		storm(3)
	}
	w.tick(b)
	if st := b.svc.Stats(); st.Outbox != 0 {
		t.Fatalf("%d recoveries from unauthenticated envelopes", st.Outbox)
	}
	w.clock.Add(61 * time.Minute)
	storm(3)
	w.tick(b)
	if st := b.svc.Stats(); st.Outbox != 0 {
		t.Fatalf("%d recoveries after the hour", st.Outbox)
	}
}

func TestUnauthenticatedEnvelopeTableIsBounded(t *testing.T) {
	w := newWorld(t)
	b, c := w.newPeer(), w.newPeer()
	for i := 0; i < 5*maxUnauthDevices; i++ {
		env := craft(t, b, c, c.self.Account, fakeDevice(), []byte("some ratchet bytes of enough length to parse...."), false)
		b.svc.Receive(context.Background(), env)
	}
	b.svc.mu.Lock()
	n := len(b.svc.unauth)
	b.svc.mu.Unlock()
	if n == 0 || n > maxUnauthDevices {
		t.Fatalf("%d devices in the unauthenticated table, want 1..%d", n, maxUnauthDevices)
	}
}

// ---- finding C: a refusal is remembered per (account, device) and only for lookup errors --------------

func TestInitWithAnUnlistedDeviceDoesNotBlockTheAccount(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	// an envelope that names a's account and a device it does not have
	for i := 0; i < 3; i++ {
		env := craft(t, b, b, a.self.Account, fakeDevice(), []byte("init bytes"), true)
		if st := b.svc.Receive(context.Background(), env); st != chatpb.StatusRefused {
			t.Fatalf("forged init %d: %d", i, st)
		}
	}
	// the real init of a still gets through
	w.send(a, b, "the real one")
	w.tick(a)
	if got := b.texts(); len(got) != 1 || got[0] != "the real one" {
		t.Fatalf("b got %v: the forged inits blocked the account", got)
	}
}

func TestLookupErrorIsRememberedPerDevice(t *testing.T) {
	w := newWorld(t)
	b := w.newPeer()
	ghost := fakeAccount()
	dev1, dev2 := fakeDevice(), fakeDevice()
	for i := 0; i < 4; i++ {
		b.svc.Receive(context.Background(), craft(t, b, b, ghost, dev1, []byte("init bytes"), true))
	}
	if n := w.resolves[ghost.ID()]; n != 1 {
		t.Fatalf("%d lookups for four inits of one (account, device)", n)
	}
	b.svc.Receive(context.Background(), craft(t, b, b, ghost, dev2, []byte("init bytes"), true))
	if n := w.resolves[ghost.ID()]; n != 2 {
		t.Fatalf("%d lookups, another device of the account is its own case", n)
	}
}

// ---- finding D: receipts do not take the room of the user's messages ----------------------------------

func TestReceiptsTakeAtMostHalfTheOutbox(t *testing.T) {
	w := newWorld(t)
	w.tune = func(c *ServiceConfig) { c.MaxOutbox = 8 }
	a, b := w.newPeer(), w.newPeer()
	// a sends; b never ticks, so its receipts stay queued
	for i := 0; i < 8; i++ {
		w.send(a, b, "m")
		w.tick(a)
	}
	if got := len(b.texts()); got != 8 {
		t.Fatalf("b got %d messages", got)
	}
	if n := b.svc.Stats().Outbox; n != 4 {
		t.Fatalf("%d receipts queued in an outbox of 8, want 4 (half)", n)
	}
	// the other half is for the user
	for i := 0; i < 4; i++ {
		if _, err := b.svc.Send(context.Background(), a.self.Node, text("mine")); err != nil {
			t.Fatalf("message %d of the user: %v", i, err)
		}
	}
	if _, err := b.svc.Send(context.Background(), a.self.Node, text("one too many")); err == nil {
		t.Fatal("a ninth item fit an outbox of 8")
	}
}
