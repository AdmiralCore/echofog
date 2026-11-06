package call

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

type guardClock struct {
	mu     sync.Mutex
	t      time.Time
	delays []time.Duration
	chans  []chan time.Time
}

func newGuardClock() *guardClock { return &guardClock{t: time.Unix(1_800_000_000, 0)} }

func (c *guardClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *guardClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// After records the delay; the timer fires when the test calls release.
func (c *guardClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.delays = append(c.delays, d)
	c.chans = append(c.chans, ch)
	return ch
}

func (c *guardClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.chans)
}

func (c *guardClock) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.chans {
		ch <- c.t
	}
	c.chans = nil
}

func testJoin(t testing.TB, salt byte) *pb.Join {
	t.Helper()
	s, err := keys.NewEd25519Signer(bytes.Repeat([]byte{salt}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &pb.Join{MediaKey: append([]byte(nil), s.Public().Key()...), JoinSalt: bytes.Repeat([]byte{salt}, 16)}
}

func newGuard(c *guardClock, allowed func(string, [32]byte) bool) *JoinGuard {
	return NewJoinGuard(JoinGuardConfig{Now: c.Now, After: c.After, Allowed: allowed, MaxPending: 2})
}

func TestJoinGuardAdmitsFreshJoinOnce(t *testing.T) {
	c := newGuardClock()
	g := newGuard(c, nil)
	dev := [32]byte{1}
	j := testJoin(t, 1)
	ts := c.Now().UnixMilli()
	if err := g.Admit(context.Background(), "room", dev, j, ts); err != nil {
		t.Fatalf("fresh join: %v", err)
	}
	// the same signed join again (a replay after the 30 s signal dedupe window) is refused.
	c.Advance(40 * time.Second)
	if err, _ := refuse(t, c, g, "room", dev, j, ts); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("replayed join: %v, want ErrJoinRefused", err)
	}
	// a join with another salt from the same device is fine.
	if err := g.Admit(context.Background(), "room", dev, testJoin(t, 2), c.Now().UnixMilli()); err != nil {
		t.Fatalf("new join: %v", err)
	}
}

// refusalCase runs one refusing Admit to completion and returns its error and the delay used.
func refuse(t *testing.T, c *guardClock, g *JoinGuard, call string, dev [32]byte, j *pb.Join, ts int64) (error, time.Duration) {
	t.Helper()
	before := c.pending()
	done := make(chan error, 1)
	go func() { done <- g.Admit(context.Background(), call, dev, j, ts) }()
	deadline := time.Now().Add(2 * time.Second)
	for c.pending() == before {
		select {
		case err := <-done:
			return err, 0 // returned without waiting: no delay
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("refusal neither returned nor waited")
		}
		time.Sleep(time.Millisecond)
	}
	c.mu.Lock()
	d := c.delays[len(c.delays)-1]
	c.mu.Unlock()
	c.release()
	return <-done, d
}

func TestJoinGuardRefusalsAreIndistinguishable(t *testing.T) {
	c := newGuardClock()
	allowed := func(call string, dev [32]byte) bool { return call == "room" && dev[0] != 0xff }
	g := newGuard(c, allowed)
	dev := [32]byte{1}
	if err := g.Admit(context.Background(), "room", dev, testJoin(t, 1), c.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	now := c.Now().UnixMilli()
	badSalt := testJoin(t, 5)
	badSalt.JoinSalt = badSalt.JoinSalt[:8]
	cases := []struct {
		name string
		call string
		dev  [32]byte
		j    *pb.Join
		ts   int64
	}{
		{"no such call", "other", dev, testJoin(t, 2), now},
		{"kicked", "room", [32]byte{0xff}, testJoin(t, 3), now},
		{"stale", "room", dev, testJoin(t, 4), now - int64(10*time.Minute/time.Millisecond)},
		{"from the future", "room", dev, testJoin(t, 6), now + int64(10*time.Minute/time.Millisecond)},
		{"bad salt", "room", dev, badSalt, now},
		{"nil join", "room", dev, nil, now},
		{"replayed salt", "room", dev, testJoin(t, 1), now},
	}
	var wantMsg string
	var wantDelay time.Duration
	for i, tc := range cases {
		err, d := refuse(t, c, g, tc.call, tc.dev, tc.j, tc.ts)
		if !errors.Is(err, ErrJoinRefused) {
			t.Fatalf("%s: %v, want ErrJoinRefused", tc.name, err)
		}
		if i == 0 {
			wantMsg, wantDelay = err.Error(), d
			if d == 0 {
				t.Fatal("a refusal is held back for a fixed delay")
			}
			continue
		}
		if err.Error() != wantMsg || d != wantDelay {
			t.Errorf("%s: %q after %v, differs from %q after %v", tc.name, err.Error(), d, wantMsg, wantDelay)
		}
	}
}

func TestJoinGuardParallelRefusalsHoldTheSameResources(t *testing.T) {
	c := newGuardClock()
	g := newGuard(c, func(string, [32]byte) bool { return false })
	dev := [32]byte{1}
	const n = 5 // MaxPending is 2: the 3rd..5th see the limit, and must look the same
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			errs <- g.Admit(context.Background(), "room", dev, testJoin(t, byte(10+i)), c.Now().UnixMilli())
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.pending() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d refusals waited: those over the limit answer faster than the rest", c.pending(), n)
		}
		time.Sleep(time.Millisecond)
	}
	for _, d := range c.delays {
		if d != c.delays[0] {
			t.Fatalf("delays differ: %v", c.delays)
		}
	}
	c.release()
	for i := 0; i < n; i++ {
		if err := <-errs; !errors.Is(err, ErrJoinRefused) || err.Error() != ErrJoinRefused.Error() {
			t.Fatalf("refusal %d: %v", i, err)
		}
	}
}

func instantAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func saltJoin(t testing.TB, n int) *pb.Join {
	j := testJoin(t, 1)
	j.JoinSalt = []byte{byte(n), byte(n >> 8), byte(n >> 16), 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7}
	return j
}

func TestJoinGuardOtherDeviceCannotMakeItForgetReplays(t *testing.T) {
	c := newGuardClock()
	g := NewJoinGuard(JoinGuardConfig{Now: c.Now, After: instantAfter, MaxSalts: 4096})
	victim, attacker := [32]byte{1}, [32]byte{2}
	vj := saltJoin(t, 0)
	if err := g.Admit(context.Background(), "room", victim, vj, c.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// another member sends 5000 fresh joins inside the window to push the victim's pair out.
	for i := 1; i <= 5000; i++ {
		_ = g.Admit(context.Background(), "room", attacker, saltJoin(t, i), c.Now().UnixMilli())
	}
	if err := g.Admit(context.Background(), "room", victim, vj, c.Now().UnixMilli()); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("replay of the victim's join after a flood: %v, want ErrJoinRefused", err)
	}
}

func TestJoinGuardFullTableRefusesInsteadOfForgetting(t *testing.T) {
	c := newGuardClock()
	g := NewJoinGuard(JoinGuardConfig{Now: c.Now, After: instantAfter, MaxSalts: 16, MaxSaltsPerDevice: 4})
	first := saltJoin(t, 0)
	for i := 0; i < 16; i++ { // 16 devices fill the table
		j := first
		if i > 0 {
			j = saltJoin(t, i)
		}
		if err := g.Admit(context.Background(), "room", [32]byte{byte(i)}, j, c.Now().UnixMilli()); err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
	}
	if err := g.Admit(context.Background(), "room", [32]byte{99}, saltJoin(t, 99), c.Now().UnixMilli()); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("join into a full table: %v, want a refusal (not forgetting an old pair)", err)
	}
	if err := g.Admit(context.Background(), "room", [32]byte{0}, first, c.Now().UnixMilli()); !errors.Is(err, ErrJoinRefused) {
		t.Fatalf("replay with a full table: %v", err)
	}
	// once the pairs are older than the retention, room is made again.
	c.Advance(3 * time.Minute)
	if err := g.Admit(context.Background(), "room", [32]byte{99}, saltJoin(t, 99), c.Now().UnixMilli()); err != nil {
		t.Fatalf("after the retention: %v", err)
	}
}

func TestJoinGuardPerDeviceLimit(t *testing.T) {
	c := newGuardClock()
	g := NewJoinGuard(JoinGuardConfig{Now: c.Now, After: instantAfter, MaxSaltsPerDevice: 3})
	dev := [32]byte{5}
	var ok int
	for i := 0; i < 10; i++ {
		if g.Admit(context.Background(), "room", dev, saltJoin(t, i), c.Now().UnixMilli()) == nil {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("%d joins of one device admitted, want 3", ok)
	}
	if err := g.Admit(context.Background(), "room", [32]byte{6}, saltJoin(t, 0), c.Now().UnixMilli()); err != nil {
		t.Fatalf("another device is not limited by the first: %v", err)
	}
}

func TestDedupeEvictsOldestNotEverything(t *testing.T) {
	signer := testSigner(t, 40)
	s := newCallService(t, Config{Origin: signer.Public(), Sign: signFn(signer)})
	sender := bytes.Repeat([]byte{3}, 32)
	for id := uint64(1); id <= dedupeMax+50; id++ {
		if s.duplicate(sender, id) {
			t.Fatalf("id %d is new", id)
		}
	}
	// the most recent ids are still remembered: a full table must not forget the signals that
	// were just seen (T9.1 finding 2: an insider could clear the table and replay old signals).
	for id := uint64(dedupeMax); id <= dedupeMax+50; id++ {
		if !s.duplicate(sender, id) {
			t.Fatalf("id %d forgotten after the table filled", id)
		}
	}
}

func TestForgedSignalOnRealStreamThenValidOneArrives(t *testing.T) {
	aSigner, bSigner := testSigner(t, 41), testSigner(t, 42)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	got := make(chan *pb.CallSignal, 4)
	b := newCallService(t, Config{
		Origin: bSigner.Public(), Sign: signFn(bSigner),
		OnSignal: func(_ context.Context, _ Peer, m *pb.CallSignal, _ []byte) { got <- m },
	})
	a := newCallService(t, Config{Origin: aSigner.Public(), Sign: signFn(aSigner), Open: pairOpen(t, b, bID, aID)})
	peer := Peer{Device: deviceID(bSigner.Public())}

	m := testPayload(ActionStreamAnnounce)
	m.Sender = aID
	valid, err := Encode(m, aSigner.Public(), signFn(aSigner))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := Parse(valid)
	body := append([]byte(nil), s.Body...)
	body[bytes.Index(body, []byte("screen"))] = 'S'
	forged, err := marshalSignal(&pb.Signal{Origin: s.Origin, Body: body, Sig: s.Sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Forward(context.Background(), peer, forged); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(context.Background(), peer, testPayload(ActionStreamAnnounce)); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-got:
		if rec.GetStreamAnnounce().GetMediaType() != "screen" {
			t.Fatalf("delivered %+v", rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the valid signal after a forged one never arrived: the stream died")
	}
	select {
	case rec := <-got:
		t.Fatalf("a second signal was delivered: %+v", rec)
	case <-time.After(50 * time.Millisecond):
	}
}
