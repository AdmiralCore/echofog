package chat

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// ---- finding 1: the outbox has limits and a lifetime ------------------------------------------

func TestOutboxLimits(t *testing.T) {
	w := newWorld(t)
	w.tune = func(c *ServiceConfig) { c.MaxOutbox, c.MaxOutboxPerPeer = 6, 4 }
	a, b, c := w.newPeer(), w.newPeer(), w.newPeer()
	w.down[b.self.Device], w.down[c.self.Device] = true, true
	w.mailDown = true
	for i := 0; i < 4; i++ {
		if _, err := a.svc.Send(context.Background(), b.self.Node, text("x")); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if _, err := a.svc.Send(context.Background(), b.self.Node, text("x")); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("the fifth message for one account: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := a.svc.Send(context.Background(), c.self.Node, text("y")); err != nil {
			t.Fatalf("message to c %d: %v", i, err)
		}
	}
	if _, err := a.svc.Send(context.Background(), c.self.Node, text("y")); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("the seventh message in all: %v", err)
	}
	if st := a.svc.Stats(); st.Outbox != 6 || st.PerPeer[b.self.Node] != 4 {
		t.Fatalf("counters %+v", st)
	}
}

func TestOutboxByteLimit(t *testing.T) {
	w := newWorld(t)
	w.tune = func(c *ServiceConfig) { c.MaxOutboxBytes = 3000 }
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	w.mailDown = true
	var full bool
	for i := 0; i < 20; i++ {
		if _, err := a.svc.Send(context.Background(), b.self.Node, text("z")); errors.Is(err, ErrOutboxFull) {
			full = true
			break
		}
	}
	if !full {
		t.Fatal("no limit on the bytes of the outbox")
	}
	if st := a.svc.Stats(); st.Bytes > 3000 {
		t.Fatalf("%d bytes kept, the limit is 3000", st.Bytes)
	}
}

// Receipts are bounded too: a sender that cannot be reached does not fill the outbox of the receiver.
func TestReceiptsAreBounded(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	for i := 0; i < 3; i++ {
		w.send(a, b, "m")
		w.tick(a)
	}
	w.down[a.self.Device] = true
	w.mailDown = true
	for i := 0; i < 2*MaxReceiptsPerPeer; i++ {
		w.send(a, b, "m")
		// a is "down" only for b's links: deliver by hand
		w.down[a.self.Device] = false
		w.down[b.self.Device] = false
		w.tick(a)
		w.down[a.self.Device] = true
	}
	if got := len(b.texts()); got != 3+2*MaxReceiptsPerPeer {
		t.Fatalf("b got %d messages", got)
	}
	if n := b.svc.Stats().PerPeer[a.self.Node]; n == 0 {
		t.Fatal("no receipts counted")
	} else if n > MaxReceiptsPerPeer {
		t.Fatalf("%d receipts queued for one account, the limit is %d", n, MaxReceiptsPerPeer)
	}
}

func TestPendingMessagesExpire(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	w.mailDown = true
	id := w.send(a, b, "never arrives")
	w.tick(a)
	w.clock.Add(7*24*time.Hour + time.Minute)
	w.tick(a)
	if st, _ := a.svc.Status(id); st != StateFailed {
		t.Fatalf("a message pending for more than 7 days is %v", st)
	}
	if a.status[id] != StateFailed {
		t.Fatal("no status event for the expiry")
	}
	w.clock.Add(8 * 24 * time.Hour)
	w.tick(a)
	if st := a.svc.Stats(); st.Outbox != 0 || st.Bytes != 0 {
		t.Fatalf("expired items are still counted: %+v", st)
	}
}

func TestReceiptsExpireAfterADay(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "m")
	w.tick(a)
	w.down[a.self.Device] = true
	w.mailDown = true
	w.send(a, b, "m2") // b is reachable for this one; a is not for b's receipts
	w.down[a.self.Device] = false
	w.tick(a)
	w.down[a.self.Device] = true
	if b.svc.Stats().Outbox == 0 {
		t.Fatal("setup: no receipt is waiting")
	}
	w.clock.Add(25 * time.Hour)
	w.tick(b)
	if st := b.svc.Stats(); st.Outbox != 0 {
		t.Fatalf("a receipt older than a day is still queued: %+v", st)
	}
}

// After any sequence of operations the maintained counters equal a count from the store (AD19).
func TestOutboxCountersAgreeWithTheStore(t *testing.T) {
	w := newWorld(t)
	w.tune = func(c *ServiceConfig) { c.MaxOutbox, c.MaxOutboxPerPeer = 12, 5 }
	a, b, c := w.newPeer(), w.newPeer(), w.newPeer()
	pick := func(n int) int { v, _ := rand.Int(rand.Reader, big.NewInt(int64(n))); return int(v.Int64()) }
	check := func(step string) {
		t.Helper()
		for _, p := range []*peer{a, b, c} {
			got, want := p.svc.Stats(), p.svc.Recount()
			if got.Outbox != want.Outbox || got.Bytes != want.Bytes || got.Seen != want.Seen || len(got.PerPeer) != len(want.PerPeer) {
				t.Fatalf("after %s: maintained %+v, counted %+v", step, got, want)
			}
			for k, v := range want.PerPeer {
				if got.PerPeer[k] != v {
					t.Fatalf("after %s: per-peer %d, counted %d", step, got.PerPeer[k], v)
				}
			}
		}
	}
	peers := []*peer{a, b, c}
	var counted int
	defer func() {
		if counted == 0 {
			t.Error("the counters never counted anything")
		}
	}()
	for i := 0; i < 60; i++ {
		counted += a.svc.Stats().Outbox + b.svc.Stats().Outbox + c.svc.Stats().Outbox
		switch pick(6) {
		case 0, 1:
			from, to := peers[pick(3)], peers[pick(3)]
			if from != to {
				_, _ = from.svc.Send(context.Background(), to.self.Node, text("m"))
			}
			check("send")
		case 2:
			w.down[peers[pick(3)].self.Device] = pick(2) == 0
			w.mailDown = pick(2) == 0
			w.tick(peers[pick(3)])
			check("tick")
		case 3:
			w.clock.Add(time.Duration(pick(72)) * time.Hour)
			w.tick(a, b, c)
			check("time")
		case 4:
			peers[pick(3)].restart()
			check("restart")
		case 5:
			w.clock.Add(time.Minute)
			w.tick(a, b, c)
			check("minute")
		}
	}
}

// ---- finding 2: the remembered message IDs are bounded ----------------------------------------

func TestSeenIsBoundedPerDevice(t *testing.T) {
	w := newWorld(t)
	w.tune = func(c *ServiceConfig) { c.MaxSeen, c.MaxSeenPerDevice = 100, 3 }
	a, b := w.newPeer(), w.newPeer()
	for i := 0; i < 8; i++ {
		w.send(a, b, "m")
		w.tick(a)
	}
	if got := len(b.texts()); got != 8 {
		t.Fatalf("b got %d messages", got)
	}
	if n := b.svc.Stats().Seen; n == 0 || n > 3 {
		t.Fatalf("%d IDs of one device remembered, want 1..3", n)
	}
	// the newest are kept: a repeat of the last one is still a duplicate
	last := w.sent[len(w.sent)-1].env
	if st := b.svc.Receive(context.Background(), last); st != chatpb.StatusDuplicate {
		t.Fatalf("a repeat of the newest message: %d", st)
	}
}

func TestSeenIsBoundedInAll(t *testing.T) {
	w := newWorld(t)
	w.tune = func(c *ServiceConfig) { c.MaxSeen, c.MaxSeenPerDevice = 4, 100 }
	a, b, c := w.newPeer(), w.newPeer(), w.newPeer()
	for i := 0; i < 4; i++ {
		w.send(a, b, "a")
		w.send(c, b, "c")
		w.tick(a, c)
	}
	if n := b.svc.Stats().Seen; n == 0 || n > 4 {
		t.Fatalf("%d IDs remembered, want 1..4", n)
	}
}

func TestSeenExpiresAfterFourteenDays(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "once")
	w.tick(a)
	env := w.sent[0].env
	if b.svc.Stats().Seen != 1 {
		t.Fatalf("seen %d", b.svc.Stats().Seen)
	}
	w.clock.Add(13 * 24 * time.Hour)
	w.tick(b)
	if b.svc.Stats().Seen != 1 {
		t.Fatal("forgotten before 14 days")
	}
	if st := b.svc.Receive(context.Background(), env); st != chatpb.StatusDuplicate {
		t.Fatalf("a repeat after 13 days: %d", st)
	}
	w.clock.Add(25 * time.Hour)
	w.tick(b)
	if st, want := b.svc.Stats(), b.svc.Recount(); st.Seen != 0 || want.Seen != 0 {
		t.Fatalf("after 14 days: maintained %d, counted %d", st.Seen, want.Seen)
	}
	if st := b.svc.Receive(context.Background(), env); st == chatpb.StatusOK {
		t.Fatal("a repeat after the record expired was delivered again")
	}
	if len(b.texts()) != 1 {
		t.Fatalf("b shows %d messages", len(b.texts()))
	}
}

// ---- finding 3: no oracle for sessions -----------------------------------------------------------

func TestSessionOracleIsClosed(t *testing.T) {
	w := newWorld(t)
	a, b, c := w.newPeer(), w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	// anybody can seal an envelope to b with a ratchet body that names a device
	withSession := craft(t, b, a, c.self.Account, a.self.DeviceKey, mustEnc(t, a, b, "x"), false)
	noSession := craft(t, b, c, c.self.Account, c.self.DeviceKey, []byte("some ratchet bytes of enough length to parse...."), false)
	unknown := craft(t, b, c, c.self.Account, fakeDevice(), []byte("some ratchet bytes of enough length to parse...."), false)
	var seen []uint32
	for _, e := range [][]byte{withSession, noSession, unknown} {
		seen = append(seen, b.svc.Receive(context.Background(), e))
	}
	for i, st := range seen {
		if st != chatpb.StatusRefused {
			t.Fatalf("case %d answered %d: the answers tell whether b has a session with the device", i, st)
		}
	}
}

func fakeDevice() keys.AKey {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return keys.New(keys.Ed25519, k)
}

// ---- finding 4: time limits on the receiving side ---------------------------------------------------

func TestIdleStreamIsClosed(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	fire := make(chan time.Time, 1)
	var asked time.Duration
	srv := New(Config{Bundles: &bundler{}, Receiver: &receiver{}, Now: c.Now, After: func(d time.Duration) <-chan time.Time {
		asked = d
		return fire
	}})
	st, stop := run(t, srv, direct("203.0.113.5:4430"))
	defer stop()
	fire <- time.Time{}
	waitDone(t, st)
	if st.code != frame.CodeTimeout {
		t.Fatalf("code %d, want timeout", st.code)
	}
	if asked != DeliverTimeout {
		t.Fatalf("idle timer of %v, want %v", asked, DeliverTimeout)
	}
}

type blockingReceiver struct {
	started chan struct{}
	ctxDone chan struct{}
}

func (r *blockingReceiver) Receive(ctx context.Context, env []byte) uint32 {
	close(r.started)
	<-ctx.Done()
	close(r.ctxDone)
	return chatpb.StatusRefused
}

func TestReceiveHasATimeLimit(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	r := &blockingReceiver{started: make(chan struct{}), ctxDone: make(chan struct{})}
	timers := make(chan chan time.Time, 8)
	srv := New(Config{Receiver: r, Now: c.Now, After: func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		timers <- ch
		return ch
	}})
	st, stop := run(t, srv, direct("203.0.113.5:4430"))
	defer stop()
	st.in <- deliver(t, 1, []byte("e"))
	<-r.started
	// fire every timer the handler asked for
	for {
		select {
		case ch := <-timers:
			ch <- time.Time{}
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	waitDone(t, st)
	select {
	case <-r.ctxDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the receiver was not cancelled")
	}
	if st.code != frame.CodeTimeout {
		t.Fatalf("code %d, want timeout", st.code)
	}
}

// ---- finding 5: the failure tables are bounded -------------------------------------------------------

func TestUnauthenticatedTableIsBounded(t *testing.T) {
	w := newWorld(t)
	a := w.newPeer()
	for i := 0; i < 3*maxUnauthDevices; i++ {
		var dev, node [32]byte
		var id [16]byte
		_, _ = rand.Read(dev[:])
		_, _ = rand.Read(node[:])
		_, _ = rand.Read(id[:])
		a.svc.noteUnauthenticated(dev, node, id)
	}
	a.svc.mu.Lock()
	n := len(a.svc.unauth)
	a.svc.mu.Unlock()
	if n == 0 || n > maxUnauthDevices {
		t.Fatalf("%d entries in the unauthenticated table, the limit is %d", n, maxUnauthDevices)
	}
	w.clock.Add(time.Hour)
	var dev, node [32]byte
	a.svc.noteUnauthenticated(dev, node, [16]byte{1})
	a.svc.mu.Lock()
	n = len(a.svc.unauth)
	a.svc.mu.Unlock()
	if n > 1 {
		t.Fatalf("%d entries survive an hour, expired ones must go", n)
	}
}

// ---- finding 6: a failed lookup is not repeated for every init --------------------------------------

func TestFailedLookupIsRemembered(t *testing.T) {
	w := newWorld(t)
	b := w.newPeer()
	ghost := fakeAccount()
	node := ghost.ID()
	dev := fakeDevice()
	for i := 0; i < 5; i++ {
		env := craft(t, b, b, ghost, dev, []byte("init bytes"), true)
		if st := b.svc.Receive(context.Background(), env); st != chatpb.StatusRefused {
			t.Fatalf("init %d: %d", i, st)
		}
	}
	if n := w.resolves[node]; n != 1 {
		t.Fatalf("%d lookups for five inits of an unknown account", n)
	}
	w.clock.Add(2 * time.Minute)
	b.svc.Receive(context.Background(), craft(t, b, b, ghost, dev, []byte("init bytes"), true))
	if n := w.resolves[node]; n != 2 {
		t.Fatalf("%d lookups after the pause, want 2", n)
	}
}

func fakeAccount() keys.AKey {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return keys.New(keys.Ed25519, k)
}

// ---- not a finding, but asked for: the service under concurrency --------------------------------------

func TestServiceUnderConcurrency(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_, _ = a.svc.Send(context.Background(), b.self.Node, text(strings.Repeat("s", i+1)))
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_ = a.svc.Tick(context.Background())
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_ = b.svc.Tick(context.Background())
				_ = b.svc.Stats()
			}
		}()
	}
	wg.Wait()
	if got, want := a.svc.Stats(), a.svc.Recount(); got.Outbox != want.Outbox || got.Bytes != want.Bytes {
		t.Fatalf("maintained %+v, counted %+v", got, want)
	}
}
