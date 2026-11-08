package carrier

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestNarrowMarksTTLAndForget(t *testing.T) {
	clock := newFakeClock()
	var marks []NarrowReason
	n := NewNarrowPath(clock, func(_ string, why NarrowReason) { marks = append(marks, why) })
	if n.Marked("net-a") {
		t.Fatal("marked before any evidence")
	}
	n.Mark("net-a", ReasonMSS)
	n.Mark("net-a", ReasonMSS) // a repeat is not a new mark
	if !n.Marked("net-a") || n.Marked("net-b") {
		t.Fatal("the mark belongs to its own network")
	}
	if len(marks) != 1 {
		t.Fatalf("onMark called %d times, want 1", len(marks))
	}
	clock.Advance(NarrowTTL - time.Minute)
	if !n.Marked("net-a") {
		t.Fatal("mark gone before 24 h")
	}
	clock.Advance(2 * time.Minute)
	if n.Marked("net-a") {
		t.Fatal("mark outlived 24 h")
	}
	n.Mark("net-a", ReasonNoise3Silent)
	n.Forget("net-a")
	if n.Marked("net-a") {
		t.Fatal("Forget kept the mark")
	}
	for i := 0; i < 200; i++ { // bounded
		n.Mark(string(rune('A'+i%50))+string(rune('a'+i/50)), ReasonMSS)
	}
}

// noise3 stays silent; the fallback starts within a second, and when another suite wins after the
// 1 s window the network is marked (wire.md section 2.4 path requirement, T3.12).
func TestDialMarksNarrowWhenNoise3StaysSilent(t *testing.T) {
	clock := newFakeClock()
	narrow := NewNarrowPath(clock, nil)
	started := make(chan uint32, 4)
	release := make(chan struct{})
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(ctx context.Context, _ DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteNoise3
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(ctx context.Context, _ DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteReality2QUIC
		select {
		case <-release:
			return newTestCarrier(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	cfg := DialConfig{Registry: r, Clock: clock, Jitter: func() time.Duration { return 0 }, Narrow: narrow, NetworkID: func() string { return "n1" }}
	res := runDial(context.Background(), DialRequest{}, []Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)}, cfg)
	if got := waitStarted(t, started); got != objects.SuiteNoise3 {
		t.Fatalf("first started: %d", got)
	}
	clock.Advance(NoAnswerAfter - time.Millisecond) // the fallback must have started by now
	if got := waitStarted(t, started); got != objects.SuiteReality2QUIC {
		t.Fatalf("fallback started: %d", got)
	}
	clock.Advance(time.Second) // noise3 has now been silent for over a second
	close(release)
	if got := waitDial(t, res); got.e != nil || got.a.Suite != objects.SuiteReality2QUIC {
		t.Fatalf("Dial: %+v", got)
	}
	if !narrow.Marked("n1") {
		t.Fatal("a silent noise3 with a live fallback did not mark the network")
	}
}

// A fallback that wins before the 1 s window proves nothing about the path.
func TestDialDoesNotMarkWhenTheFallbackWinsFast(t *testing.T) {
	clock := newFakeClock()
	narrow := NewNarrowPath(clock, nil)
	started := make(chan uint32, 4)
	release := make(chan struct{})
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(ctx context.Context, _ DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteNoise3
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(ctx context.Context, _ DialRequest) (mux.Carrier, error) {
		started <- objects.SuiteReality2QUIC
		<-release
		return newTestCarrier(), nil
	}))
	cfg := DialConfig{Registry: r, Clock: clock, Jitter: func() time.Duration { return 0 }, Narrow: narrow, NetworkID: func() string { return "n1" }}
	res := runDial(context.Background(), DialRequest{}, []Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)}, cfg)
	waitStarted(t, started)
	clock.Advance(400 * time.Millisecond)
	waitStarted(t, started)
	close(release)
	if got := waitDial(t, res); got.e != nil {
		t.Fatalf("Dial: %v", got.e)
	}
	if narrow.Marked("n1") {
		t.Fatal("a fast fallback marked the network")
	}
}

// On a marked network noise3 is not tried at all.
func TestDialSkipsNoise3OnAMarkedNetwork(t *testing.T) {
	clock := newFakeClock()
	narrow := NewNarrowPath(clock, nil)
	narrow.Mark("n1", ReasonMSS)
	var noise atomic.Int32
	r := NewRegistry()
	r.RegisterDialer(objects.SuiteNoise3, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) {
		noise.Add(1)
		return newTestCarrier(), nil
	}))
	want := newTestCarrier()
	r.RegisterDialer(objects.SuiteReality2QUIC, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) { return want, nil }))
	cfg := DialConfig{Registry: r, Clock: clock, Jitter: func() time.Duration { return 0 }, Narrow: narrow, NetworkID: func() string { return "n1" }}
	got, a, err := Dial(context.Background(), DialRequest{}, []Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)}, cfg)
	if err != nil || got != want || a.Suite != objects.SuiteReality2QUIC {
		t.Fatalf("Dial = %v %+v %v", got, a, err)
	}
	if noise.Load() != 0 {
		t.Fatal("noise3 was tried on a marked network")
	}
	// A different network still tries it.
	cfg.NetworkID = func() string { return "n2" }
	if _, a, err := Dial(context.Background(), DialRequest{}, []Attempt{dialAttempt(objects.SuiteNoise3), dialAttempt(objects.SuiteReality2QUIC)}, cfg); err != nil || a.Suite != objects.SuiteNoise3 {
		t.Fatalf("other network: %+v %v", a, err)
	}
}

// ---- the TCP path model

// pathModel is a network path with a given PMTU. A TCP segment of payload s travels in s+40 bytes
// (IPv4) or s+60 (IPv6); one that does not fit is dropped, and without ICMP nobody tells the
// sender. With ICMP the sender's PMTU discovery lowers the segment size by itself.
type pathModel struct {
	mtu  int
	icmp bool
	v6   bool

	mu    sync.Mutex
	calls []int // the MSS of every opened socket, 0 = not limited
}

func (m *pathModel) hdr() int {
	if m.v6 {
		return 60
	}
	return 40
}

func (m *pathModel) dialMSS(_ context.Context, _, _ string, mss int) (net.Conn, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mss)
	m.mu.Unlock()
	eff := mss
	if eff == 0 {
		eff = 1500 - m.hdr()
	}
	c, s := net.Pipe()
	go func() { // the server answers one byte once it has the whole hello
		buf := make([]byte, 4096)
		got := 0
		for got < helloSize {
			n, err := s.Read(buf)
			if err != nil {
				return
			}
			got += n
		}
		_, _ = s.Write([]byte{1})
		_ = s.Close()
	}()
	return &modelConn{Conn: c, m: m, mss: eff}, nil
}

const helloSize = 1900

// testV6 makes helloAttempt dial an IPv6 literal (set by the table test; the subtests run in turn).
var testV6 bool

type modelConn struct {
	net.Conn
	m   *pathModel
	mss int
}

func (c *modelConn) Write(p []byte) (int, error) {
	seg := c.mss
	if seg+c.m.hdr() > c.m.mtu && !c.m.icmp {
		return len(p), nil // black hole: dropped, no ICMP
	}
	return c.Conn.Write(p)
}

func (m *pathModel) callsCopy() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.calls...)
}

// helloAttempt writes the hello through the opener of req and waits for the answer.
func helloAttempt(ctx context.Context, req DialRequest) (mux.Carrier, error) {
	addr := "192.0.2.1:443"
	if testV6 {
		addr = "[2001:db8::1]:443"
	}
	c, err := req.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if _, err := c.Write(make([]byte, helloSize)); err != nil {
		_ = c.Close()
		return nil, err
	}
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		_ = c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return newTestCarrier(), nil
}

func quickNarrow(clock Clock) *NarrowPath {
	n := NewNarrowPath(clock, nil)
	n.FirstTry, n.Rung = 80*time.Millisecond, 80*time.Millisecond
	return n
}

func TestMSSRungs(t *testing.T) {
	if MSSFor(false, 0) != 1240 || MSSFor(true, 0) != 1220 {
		t.Fatalf("rung 0: v4 %d v6 %d, want 1240 and 1220", MSSFor(false, 0), MSSFor(true, 0))
	}
	if MSSFor(false, 1) != 1160 || MSSFor(true, 1) != 1140 {
		t.Fatalf("rung 1: v4 %d v6 %d, want 1160 and 1140", MSSFor(false, 1), MSSFor(true, 1))
	}
	if MSSFor(false, 2) != 0 {
		t.Fatal("no rung beyond the ladder")
	}
}

func TestNarrowRetryOnTheModel(t *testing.T) {
	cases := []struct {
		name      string
		mtu       int
		icmp, v6  bool
		wantCalls []int
		marked    bool
	}{
		{"1280 no ICMP IPv4", 1280, false, false, []int{0, 1240}, true},
		{"1280 no ICMP IPv6", 1280, false, true, []int{0, 1220}, true},
		{"1280 with ICMP IPv4", 1280, true, false, []int{0}, false},
		{"1200 no ICMP IPv4", 1200, false, false, []int{0, 1240, 1160}, true},
		{"1200 no ICMP IPv6", 1200, false, true, []int{0, 1220, 1140}, true},
		{"1200 with ICMP IPv6", 1200, true, true, []int{0}, false},
		{"open path 1500", 1500, false, false, []int{0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClockRealTimers()
			narrow := quickNarrow(clock)
			m := &pathModel{mtu: tc.mtu, icmp: tc.icmp, v6: tc.v6}
			testV6 = tc.v6
			defer func() { testV6 = false }()
			req := DialRequest{DialMSS: m.dialMSS, Narrow: narrow, NetworkID: func() string { return "n1" }}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := WithNarrowRetry(ctx, req, helloAttempt)
			if err != nil {
				t.Fatalf("WithNarrowRetry: %v (socket MSS values %v)", err, m.callsCopy())
			}
			_ = c.Close()
			got := m.callsCopy()
			if len(got) != len(tc.wantCalls) {
				t.Fatalf("sockets opened with MSS %v, want %v", got, tc.wantCalls)
			}
			for i := range got {
				if got[i] != tc.wantCalls[i] {
					t.Fatalf("sockets opened with MSS %v, want %v", got, tc.wantCalls)
				}
			}
			if got[0] != 0 {
				t.Fatalf("the first attempt's SYN carried a limited MSS: %d", got[0])
			}
			if narrow.Marked("n1") != tc.marked {
				t.Fatalf("marked = %v, want %v", narrow.Marked("n1"), tc.marked)
			}
		})
	}
}

// On a marked network the limited MSS is used from the first socket on.
func TestMarkedNetworkLimitsTheMSSFromTheStart(t *testing.T) {
	clock := newFakeClockRealTimers()
	narrow := quickNarrow(clock)
	narrow.Mark("n1", ReasonMSS)
	m := &pathModel{mtu: 1280}
	req := DialRequest{DialMSS: m.dialMSS, Narrow: narrow, NetworkID: func() string { return "n1" }}
	c, err := WithNarrowRetry(context.Background(), req, helloAttempt)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if got := m.callsCopy(); len(got) != 1 || got[0] != 1240 {
		t.Fatalf("sockets %v, want one with MSS 1240", got)
	}
}

// An ordinary failure (the server refuses) is not a black hole: no retry, no mark.
func TestNoRetryOnAnOrdinaryFailure(t *testing.T) {
	clock := newFakeClockRealTimers()
	narrow := quickNarrow(clock)
	m := &pathModel{mtu: 1500}
	req := DialRequest{DialMSS: m.dialMSS, Narrow: narrow, NetworkID: func() string { return "n1" }}
	boom := errors.New("tls alert")
	_, err := WithNarrowRetry(context.Background(), req, func(ctx context.Context, r DialRequest) (mux.Carrier, error) {
		c, _ := r.DialContext(ctx, "tcp", "192.0.2.1:443")
		_ = c.Close()
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if len(m.callsCopy()) != 1 || narrow.Marked("n1") {
		t.Fatalf("retried or marked on an ordinary failure: %v", m.callsCopy())
	}
}

// Through a proxy the MSS cannot be limited: the black hole is reported, not worked around.
func TestProxyCannotLimitTheMSS(t *testing.T) {
	clock := newFakeClockRealTimers()
	narrow := quickNarrow(clock)
	m := &pathModel{mtu: 1280}
	req := DialRequest{
		DialContext: func(ctx context.Context, n, a string) (net.Conn, error) { return m.dialMSS(ctx, n, a, 0) },
		Narrow:      narrow, NetworkID: func() string { return "n1" },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := WithNarrowRetry(ctx, req, helloAttempt)
	if !errors.Is(err, ErrMSSUnsupported) {
		t.Fatalf("err %v, want ErrMSSUnsupported", err)
	}
	if narrow.Marked("n1") {
		t.Fatal("marked without a working retry")
	}
}

// The ladder fits the 5 s budget of the polygon (T15.0c): first try, two rungs, a connect each.
func TestNarrowBudgets(t *testing.T) {
	n := NewNarrowPath(newFakeClock(), nil)
	if n.FirstTry+2*n.Rung > 5*time.Second-500*time.Millisecond {
		t.Fatalf("budgets %v + 2 x %v leave no room for a connection within 5 s", n.FirstTry, n.Rung)
	}
	if n.FirstTry <= 0 || n.Rung <= 0 {
		t.Fatal("zero budgets")
	}
}

// newFakeClockRealTimers is a clock whose timers are real: the budgets of the model tests run in
// real, short time.
func newFakeClockRealTimers() Clock { return systemClock{} }
