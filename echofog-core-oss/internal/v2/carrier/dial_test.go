package carrier

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// ---- test doubles

type testCarrier struct {
	name     string
	closed   atomic.Bool
	closedCh chan struct{}
}

func newTestCarrier() *testCarrier { return &testCarrier{closedCh: make(chan struct{})} }

func (c *testCarrier) OpenStream(context.Context) (mux.RawStream, error) {
	return nil, errors.New("unused")
}
func (c *testCarrier) AcceptStream(context.Context) (mux.RawStream, error) {
	return nil, errors.New("unused")
}
func (c *testCarrier) SendDatagram([]byte) error { return nil }
func (c *testCarrier) ReceiveDatagram(context.Context) ([]byte, error) {
	return nil, errors.New("unused")
}
func (c *testCarrier) ChannelBinding() [32]byte { return [32]byte{} }
func (c *testCarrier) Sealed() bool             { return false }
func (c *testCarrier) IsClient() bool           { return true }
func (c *testCarrier) LastReceive() time.Time   { return time.Time{} }
func (c *testCarrier) Done() <-chan struct{}    { return nil }
func (c *testCarrier) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.closedCh)
	}
	return nil
}

type dialFunc func(context.Context, DialRequest) (mux.Carrier, error)

func (f dialFunc) Dial(ctx context.Context, req DialRequest) (mux.Carrier, error) {
	return f(ctx, req)
}

type testListener struct{}

func (testListener) Accept(context.Context) (mux.Carrier, error) { return nil, errors.New("unused") }
func (testListener) Addr() net.Addr                              { return nil }
func (testListener) Close() error                                { return nil }

// fakeClock is a manually advanced Clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves the clock forward, running every due callback in time order.
func (c *fakeClock) Advance(d time.Duration) {
	target := c.now.Add(d)
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, tm := range c.timers {
			if tm.stopped || tm.fired || tm.at.After(target) {
				continue
			}
			if next == nil || tm.at.Before(next.at) {
				next = tm
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.fired = true
		c.now = next.at
		f := next.f
		c.mu.Unlock()
		f()
	}
}

func dialAttempt(suite uint32) Attempt {
	return Attempt{Kind: AttemptCarrier, Suite: suite, Addr: &pb.Addr{Suite: suite, Host: ipLocal.String(), Port: 443, Mask: 1}}
}

func waitStarted(t *testing.T, ch <-chan uint32) uint32 {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("dial attempt did not start")
		return 0
	}
}

func assertNotStarted(t *testing.T, ch <-chan uint32) {
	t.Helper()
	select {
	case s := <-ch:
		t.Fatalf("suite %d started too early", s)
	default:
	}
}

type dialResult struct {
	c mux.Carrier
	a Attempt
	e error
}

func runDial(ctx context.Context, req DialRequest, atts []Attempt, cfg DialConfig) <-chan dialResult {
	out := make(chan dialResult, 1)
	go func() {
		c, a, err := Dial(ctx, req, atts, cfg)
		out <- dialResult{c, a, err}
	}()
	return out
}

func waitDial(t *testing.T, ch <-chan dialResult) dialResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return")
		return dialResult{}
	}
}

// ---- tests

func TestDialFirstSuccessWins(t *testing.T) {
	want := newTestCarrier()
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return want, nil
	}))
	got, a, err := Dial(context.Background(), DialRequest{}, []Attempt{dialAttempt(objects.SuiteNoise3)}, DialConfig{Registry: r, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if got != want || a.Suite != objects.SuiteNoise3 {
		t.Fatalf("Dial = (%v, %+v), want the first attempt", got, a)
	}
}

func TestDialStartsNextImmediatelyOnFailure(t *testing.T) {
	r := NewRegistry()
	fail := errors.New("first failed")
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return nil, fail
	}))
	want := newTestCarrier()
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return want, nil
	}))
	clock := newFakeClock() // never advanced: only the failure may start attempt 2
	got, a, err := Dial(context.Background(), DialRequest{},
		[]Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)},
		DialConfig{Registry: r, Clock: clock, Jitter: func() time.Duration { return 0 }})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if got != want || a.Suite != objects.SuiteReality2QUIC {
		t.Fatalf("Dial = (%v, %+v), want the second attempt", got, a)
	}
}

func TestDialSecondAttemptAfterDelay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		jitter time.Duration
		delay  time.Duration
	}{
		{name: "without_jitter", jitter: 0, delay: 300 * time.Millisecond},
		{name: "with_jitter", jitter: 100 * time.Millisecond, delay: 400 * time.Millisecond},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan uint32, 4)
			r := NewRegistry()
			r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(ctx context.Context, _ DialRequest) (mux.Carrier, error) {
				started <- objects.SuiteNoise3
				<-ctx.Done()
				return nil, ctx.Err()
			}))
			want := newTestCarrier()
			r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
				started <- objects.SuiteReality2QUIC
				return want, nil
			}))
			clock := newFakeClock()
			res := runDial(context.Background(), DialRequest{},
				[]Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)},
				DialConfig{Registry: r, Clock: clock, Jitter: func() time.Duration { return tc.jitter }})
			if s := waitStarted(t, started); s != objects.SuiteNoise3 {
				t.Fatalf("first started suite = %d", s)
			}
			clock.Advance(tc.delay - time.Millisecond)
			assertNotStarted(t, started)
			clock.Advance(time.Millisecond)
			if s := waitStarted(t, started); s != objects.SuiteReality2QUIC {
				t.Fatalf("second started suite = %d", s)
			}
			got := waitDial(t, res)
			if got.e != nil || got.c != want || got.a.Suite != objects.SuiteReality2QUIC {
				t.Fatalf("Dial = (%v, %+v, %v), want the delayed attempt", got.c, got.a, got.e)
			}
		})
	}
}

func TestDialTimeoutUsesInjectedClock(t *testing.T) {
	started := make(chan uint32, 1)
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(ctx context.Context, _ DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteNoise3
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	clock := newFakeClock()
	res := runDial(context.Background(), DialRequest{}, []Attempt{dialAttempt(objects.SuiteNoise3)},
		DialConfig{Registry: r, Clock: clock})
	waitStarted(t, started)
	clock.Advance(4*time.Second - time.Millisecond)
	select {
	case got := <-res:
		t.Fatalf("Dial returned before the timeout: %+v", got)
	default:
	}
	clock.Advance(time.Millisecond)
	got := waitDial(t, res)
	if !errors.Is(got.e, context.DeadlineExceeded) {
		t.Fatalf("Dial error = %v, want context.DeadlineExceeded", got.e)
	}
	if got.c != nil {
		t.Fatal("Dial returned a carrier on timeout")
	}
}

func TestDialTimeouts(t *testing.T) {
	cases := []struct {
		suite uint32
		want  time.Duration
	}{
		{objects.SuiteNoise3, 4 * time.Second},
		{objects.SuiteReality2QUIC, 4 * time.Second},
		{objects.SuiteReality2TCP, 6 * time.Second},
		{objects.SuiteWS2, 8 * time.Second},
		{999, 0},
	}
	for _, c := range cases {
		if got := DialTimeout(c.suite); got != c.want {
			t.Errorf("DialTimeout(%d) = %v, want %v", c.suite, got, c.want)
		}
	}
}

func TestDialAllAttemptsFail(t *testing.T) {
	r := NewRegistry()
	errA := errors.New("noise3 down")
	errB := errors.New("quic down")
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return nil, errA
	}))
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return nil, errB
	}))
	got := waitDial(t, runDial(context.Background(), DialRequest{},
		[]Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)},
		DialConfig{Registry: r, Clock: newFakeClock(), Jitter: func() time.Duration { return 0 }}))
	if got.e == nil || !errors.Is(got.e, errA) || !errors.Is(got.e, errB) {
		t.Fatalf("Dial error = %v, want both attempt errors", got.e)
	}
}

func TestDialRelayAndUnknownSuite(t *testing.T) {
	if _, _, err := Dial(context.Background(), DialRequest{}, nil, DialConfig{}); !errors.Is(err, ErrNeedRelay) {
		t.Fatalf("Dial(no attempts) error = %v, want ErrNeedRelay", err)
	}
	if _, _, err := Dial(context.Background(), DialRequest{},
		[]Attempt{{Kind: AttemptRelay}}, DialConfig{}); !errors.Is(err, ErrNeedRelay) {
		t.Fatalf("Dial(relay) error = %v, want ErrNeedRelay", err)
	}
	if _, _, err := Dial(context.Background(), DialRequest{},
		[]Attempt{dialAttempt(999)}, DialConfig{Registry: NewRegistry(), Clock: newFakeClock()}); !errors.Is(err, ErrNoDialer) {
		t.Fatalf("Dial(unknown suite) error = %v, want ErrNoDialer", err)
	}
}

func TestDialClosesLosers(t *testing.T) {
	r := NewRegistry()
	started := make(chan uint32, 2)
	winnerGo := make(chan struct{})
	winner := newTestCarrier()
	winner.name = "winner"
	loser := newTestCarrier()
	loser.name = "loser"
	release := make(chan struct{})

	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteNoise3
		<-winnerGo
		return winner, nil
	}))
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteReality2QUIC
		<-release // deliberately ignores context cancellation
		return loser, nil
	}))
	clock := newFakeClock()
	res := runDial(context.Background(), DialRequest{},
		[]Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)},
		DialConfig{Registry: r, Clock: clock, Jitter: func() time.Duration { return 0 }})
	waitStarted(t, started)
	clock.Advance(300 * time.Millisecond)
	waitStarted(t, started)
	close(winnerGo)
	got := waitDial(t, res)
	if got.e != nil || got.c != winner {
		t.Fatalf("Dial = (%v, %v), want winner", got.c, got.e)
	}
	close(release)
	select {
	case <-loser.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the losing carrier was not closed")
	}
	if winner.closed.Load() {
		t.Fatal("winner was closed by Dial")
	}
}

func TestDialUsesMemory(t *testing.T) {
	kv := newFakeKV()
	mem := NewMemory(MemoryConfig{
		KV:         kv,
		LocalAddrs: func() []netip.Addr { return []netip.Addr{ipLocal} },
		Now:        func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	tcp := &pb.Addr{Suite: objects.SuiteReality2TCP, Host: ipSame.String(), Port: 443, Mask: 1}
	quic := &pb.Addr{Suite: objects.SuiteReality2QUIC, Host: ipBorder.String(), Port: 443, Mask: 1}
	if err := mem.Remember(context.Background(), AddressKey(tcp), objects.SuiteReality2TCP); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	started := make(chan uint32, 4)
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteReality2TCP, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteReality2TCP
		return newTestCarrier(), nil
	}))
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteReality2QUIC
		return newTestCarrier(), nil
	}))
	got, a, err := Dial(context.Background(), DialRequest{},
		[]Attempt{{Kind: AttemptCarrier, Suite: objects.SuiteReality2QUIC, Addr: quic},
			{Kind: AttemptCarrier, Suite: objects.SuiteReality2TCP, Addr: tcp}},
		DialConfig{Registry: r, Clock: newFakeClock(), Memory: mem})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if a.Suite != objects.SuiteReality2TCP {
		t.Fatalf("Dial chose suite %d, want the remembered TCP suite", a.Suite)
	}
	if first := waitStarted(t, started); first != objects.SuiteReality2TCP {
		t.Fatalf("first started suite = %d, want TCP", first)
	}
	if got == nil {
		t.Fatal("Dial returned no carrier")
	}
}

func TestDialRecordsWorkingSuite(t *testing.T) {
	kv := newFakeKV()
	mem := NewMemory(MemoryConfig{
		KV:         kv,
		LocalAddrs: func() []netip.Addr { return []netip.Addr{ipLocal} },
		Now:        func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	tcp := &pb.Addr{Suite: objects.SuiteReality2TCP, Host: ipSame.String(), Port: 443, Mask: 1}
	quic := &pb.Addr{Suite: objects.SuiteReality2QUIC, Host: ipBorder.String(), Port: 443, Mask: 1}
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteReality2TCP, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return newTestCarrier(), nil
	}))
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		return newTestCarrier(), nil
	}))
	if _, _, err := Dial(context.Background(), DialRequest{},
		[]Attempt{{Kind: AttemptCarrier, Suite: objects.SuiteReality2TCP, Addr: tcp},
			{Kind: AttemptCarrier, Suite: objects.SuiteReality2QUIC, Addr: quic}},
		DialConfig{Registry: r, Clock: newFakeClock(), Memory: mem}); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	suite, ok, err := mem.Lookup(context.Background(), AddressKey(tcp))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || suite != objects.SuiteReality2TCP {
		t.Fatalf("memory = (%d, %v), want (tcp, true)", suite, ok)
	}
}
