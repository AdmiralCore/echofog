package clock

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// base is the raw local time every test starts from. It is a whole second, like the
// wire timestamps the clock works with.
var base = time.Unix(1_760_000_000, 0).UTC()

// fakeClock is an injectable wall clock: tests advance it explicitly and never sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func peerID(b byte) PeerID {
	var p PeerID
	p[0] = b
	return p
}

// newClock builds a clock with the given endpoints and no HTTP client; peer-only tests
// never reach the network.
func newClock(t *testing.T, now func() time.Time, endpoints []string) *Clock {
	t.Helper()
	c, err := New(Config{Now: now, Endpoints: endpoints})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPeerOffsets(t *testing.T) {
	type sample struct {
		peer byte
		off  time.Duration
	}
	cases := []struct {
		name    string
		samples []sample
		want    time.Duration
	}{
		{"none", nil, 0},
		{"one peer", []sample{{1, 30 * time.Second}}, 30 * time.Second},
		{"two liars with different time", []sample{{1, time.Hour}, {2, -time.Hour}}, 0},
		{"two agreeing liars", []sample{{1, 2 * time.Minute}, {2, 2 * time.Minute}}, 2 * time.Minute},
		{"two values just inside the agreement window",
			[]sample{{1, 2 * time.Minute}, {2, 2*time.Minute + time.Second}}, 2*time.Minute + 500*time.Millisecond},
		{"two values outside the agreement window",
			[]sample{{1, 2 * time.Minute}, {2, 2*time.Minute + 3*time.Second}}, 0},
		{"two agreeing liars out of three",
			[]sample{{1, 3 * time.Minute}, {2, 3 * time.Minute}, {3, -4 * time.Minute}}, 3 * time.Minute},
		{"median of five with one outlier",
			[]sample{{1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, time.Hour}}, 0},
		{"median of five", []sample{{1, 10 * time.Second}, {2, 20 * time.Second}, {3, 30 * time.Second}, {4, 40 * time.Second}, {5, 0}}, 20 * time.Second},
		{"sixth sample drops the oldest",
			[]sample{{1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0}, {6, time.Hour}}, 0},
		{"a peer replaces its previous sample",
			[]sample{{1, 10 * time.Second}, {2, 20 * time.Second}, {1, -10 * time.Second}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClock(base)
			c := newClock(t, fc.Now, endpoints(3))
			for _, s := range tc.samples {
				c.AddPeerSample(peerID(s.peer), base.Add(s.off))
			}
			if got := c.Offset(); got != tc.want {
				t.Fatalf("Offset() = %v, want %v", got, tc.want)
			}
			if got, want := c.Now(), base.Add(tc.want); !got.Equal(want) {
				t.Fatalf("Now() = %v, want %v", got, want)
			}
			if got, want := c.Unix(), uint64(base.Unix()+int64(tc.want/time.Second)); got != want {
				t.Fatalf("Unix() = %d, want %d", got, want)
			}
		})
	}
}

// TestNoConsensusKeepsOffset pins the strict reading of "median": when the middle two
// samples disagree there is no estimate, and the previous offset stays in force.
func TestNoConsensusKeepsOffset(t *testing.T) {
	fc := newFakeClock(base)
	c := newClock(t, fc.Now, endpoints(3))

	c.AddPeerSample(peerID(1), base.Add(30*time.Second))
	if got := c.Offset(); got != 30*time.Second {
		t.Fatalf("Offset() = %v, want 30s", got)
	}

	c.AddPeerSample(peerID(2), base.Add(-30*time.Second))
	if got := c.Offset(); got != 30*time.Second {
		t.Fatalf("Offset() after disagreement = %v, want the previous 30s", got)
	}
}

// TestPeerSampleUsesRawClock guards against feeding the corrected time back into the
// offset calculation.
func TestPeerSampleUsesRawClock(t *testing.T) {
	fc := newFakeClock(base)
	c := newClock(t, fc.Now, endpoints(3))

	c.AddPeerSample(peerID(1), base.Add(30*time.Second))
	c.AddPeerSample(peerID(2), base.Add(30*time.Second))
	if got := c.Offset(); got != 30*time.Second {
		t.Fatalf("Offset() = %v, want 30s (not compounded)", got)
	}
}

// TestZeroPeerTimeCountsAsPeerButNotAsSample: an authenticated peer with an unusable
// timestamp still proves that a peer was seen, so it suppresses the CDN check, but it
// must not poison the offset with a 1970 value.
func TestZeroPeerTimeCountsAsPeerButNotAsSample(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return dateAt(fc.Now().Add(2 * time.Minute)), nil
	}}
	c, err := New(Config{Now: fc.Now, HTTP: client, Endpoints: endpoints(3)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.AddPeerSample(peerID(1), time.Time{})
	if got := c.Offset(); got != 0 {
		t.Fatalf("Offset() = %v, want 0", got)
	}
	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil {
		t.Fatalf("MaybeCheckCDN: %v", err)
	}
	if res.Ran || client.callCount() != 0 {
		t.Fatalf("CDN check ran %d times with a peer seen just now", client.callCount())
	}
}

func TestPeerWarning(t *testing.T) {
	var warned []time.Duration
	fc := newFakeClock(base)
	c, err := New(Config{
		Now:       fc.Now,
		Endpoints: endpoints(3),
		Warn:      func(off time.Duration) { warned = append(warned, off) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.AddPeerSample(peerID(1), base.Add(90*time.Second))
	c.AddPeerSample(peerID(2), base.Add(90*time.Second))
	if len(warned) != 1 || warned[0] != 90*time.Second {
		t.Fatalf("warned = %v, want exactly one 90s warning", warned)
	}

	c.AddPeerSample(peerID(3), base.Add(-90*time.Second))
	if len(warned) != 1 {
		t.Fatalf("warned = %v, want no repeat warning while the offset stays large", warned)
	}
}

func TestConcurrentUse(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return dateAt(fc.Now().Add(2 * time.Minute)), nil
	}}
	c, err := New(Config{Now: fc.Now, HTTP: client, Endpoints: endpoints(3)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.AddPeerSample(peerID(byte(i)), base.Add(time.Duration(i)*time.Second))
				_ = c.Now()
				_ = c.Offset()
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.MaybeCheckCDN(t.Context(), false); err != nil {
				t.Errorf("MaybeCheckCDN: %v", err)
			}
		}()
	}
	wg.Wait()
}
