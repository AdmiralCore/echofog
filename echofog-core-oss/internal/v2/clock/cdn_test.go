package clock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHTTP records requests and answers them from fn.
type fakeHTTP struct {
	mu    sync.Mutex
	calls []httpCall
	fn    func(*http.Request) (*http.Response, error)
}

type httpCall struct {
	method string
	url    string
	body   bool
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, httpCall{method: req.Method, url: req.URL.String(), body: req.Body != nil})
	f.mu.Unlock()
	return f.fn(req)
}

func (f *fakeHTTP) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeHTTP) recordedCalls() []httpCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]httpCall(nil), f.calls...)
}

// dateAt returns an HTTPS response whose Date header is t.
func dateAt(t time.Time) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Date": []string{t.UTC().Format(http.TimeFormat)}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func noDate() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func badDate() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Date": []string{"not a date"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func endpoints(n int) []string {
	eps := make([]string, n)
	for i := range eps {
		eps[i] = fmt.Sprintf("https://clock%d.test/", i)
	}
	return eps
}

func newCDNClock(t *testing.T, fc *fakeClock, client HTTPClient, eps []string) *Clock {
	t.Helper()
	c, err := New(Config{Now: fc.Now, HTTP: client, Endpoints: eps})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// deltasByHost answers every endpoint with its documented delta from the raw local clock.
func deltasByHost(fc *fakeClock, deltas map[string]time.Duration) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		d, ok := deltas[req.URL.Host]
		if !ok {
			return nil, fmt.Errorf("unexpected host %q", req.URL.Host)
		}
		return dateAt(fc.Now().Add(d)), nil
	}
}

func TestDefaultEndpoints(t *testing.T) {
	want := []string{
		"https://www.cloudflare.com/",
		"https://www.akamai.com/",
		"https://www.fastly.com/",
		"https://aws.amazon.com/",
		"https://www.microsoft.com/",
		"https://www.apple.com/",
		"https://www.google.com/",
	}
	if got := DefaultEndpoints(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultEndpoints() = %v, want %v", got, want)
	}

	got := DefaultEndpoints()
	got[0] = "https://example.test/"
	if DefaultEndpoints()[0] != want[0] {
		t.Fatal("DefaultEndpoints returns the package's mutable slice")
	}
}

func TestNewValidation(t *testing.T) {
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("https://clock%d.test/", i)
	}

	cases := []struct {
		name      string
		endpoints []string
		want      error
	}{
		{"too few", endpoints(2), ErrTooFewEndpoints},
		{"plain http", []string{"http://a.test/", "https://b.test/", "https://c.test/"}, ErrInvalidEndpoint},
		{"relative", []string{"https://a.test/", "relative", "https://c.test/"}, ErrInvalidEndpoint},
		{"missing host", []string{"https://a.test/", "https:///x", "https://c.test/"}, ErrInvalidEndpoint},
		{"too many", many, ErrTooManyEndpoints},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Config{Endpoints: tc.endpoints}); !errors.Is(err, tc.want) {
				t.Fatalf("New() = %v, want %v", err, tc.want)
			}
		})
	}

	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New(defaults): %v", err)
	}
	if got := len(c.endpoints); got != len(DefaultEndpoints()) {
		t.Fatalf("default endpoint count = %d, want %d", got, len(DefaultEndpoints()))
	}
}

func TestMaybeCheckCDNRequiresHTTPClient(t *testing.T) {
	fc := newFakeClock(base)
	c := newClock(t, fc.Now, endpoints(3))
	res, err := c.MaybeCheckCDN(t.Context(), false)
	if !errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("MaybeCheckCDN() error = %v, want ErrNoHTTPClient", err)
	}
	if res.Ran {
		t.Fatal("MaybeCheckCDN reported a run without an HTTP client")
	}
}

func TestCDNMajority(t *testing.T) {
	fc := newFakeClock(base)
	var warned []time.Duration
	client := &fakeHTTP{fn: deltasByHost(fc, map[string]time.Duration{
		"clock0.test": 120 * time.Second,
		"clock1.test": 120 * time.Second,
		"clock2.test": 120 * time.Second,
		"clock3.test": -240 * time.Second,
		"clock4.test": -240 * time.Second,
	})}
	c, err := New(Config{
		Now:       fc.Now,
		HTTP:      client,
		Endpoints: endpoints(5),
		Warn:      func(off time.Duration) { warned = append(warned, off) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil {
		t.Fatalf("MaybeCheckCDN: %v", err)
	}
	if !res.Ran || res.Answers != 5 || res.Agreeing != 3 {
		t.Fatalf("Result = %+v, want ran, 5 answers, 3 agreeing", res)
	}
	if res.Offset != 120*time.Second || !res.Changed || !res.Warning {
		t.Fatalf("Result = %+v, want a changed offset of 120s with a warning", res)
	}
	if got := c.Offset(); got != 120*time.Second {
		t.Fatalf("Offset() = %v, want 120s", got)
	}
	if got, want := c.Now(), base.Add(120*time.Second); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
	if len(warned) != 1 || warned[0] != 120*time.Second {
		t.Fatalf("warned = %v, want exactly one 120s warning", warned)
	}
}

func TestCDNNoMajority(t *testing.T) {
	cases := []struct {
		name    string
		deltas  []time.Duration
		answers int
	}{
		{"two against two", []time.Duration{100 * time.Second, 100 * time.Second, -100 * time.Second, -100 * time.Second}, 4},
		{"a plurality is not a majority",
			[]time.Duration{100 * time.Second, 100 * time.Second, -100 * time.Second, -100 * time.Second, 1000 * time.Second}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClock(base)
			m := make(map[string]time.Duration, len(tc.deltas))
			eps := endpoints(len(tc.deltas))
			for i, d := range tc.deltas {
				m[fmt.Sprintf("clock%d.test", i)] = d
			}
			client := &fakeHTTP{fn: deltasByHost(fc, m)}
			c := newCDNClock(t, fc, client, eps)

			res, err := c.MaybeCheckCDN(t.Context(), false)
			if err != nil {
				t.Fatalf("MaybeCheckCDN: %v", err)
			}
			if !res.Ran || res.Answers != tc.answers || res.Agreeing != 0 {
				t.Fatalf("Result = %+v, want ran with %d answers and no majority", res, tc.answers)
			}
			if res.Changed || c.Offset() != 0 {
				t.Fatalf("offset changed to %v without a majority", c.Offset())
			}
		})
	}
}

func TestCDNTooFewAnswers(t *testing.T) {
	cases := []struct {
		name   string
		broken int // endpoint index that does not produce an answer
		how    func() (*http.Response, error)
	}{
		{"transport error", 0, func() (*http.Response, error) { return nil, errors.New("dial failed") }},
		{"missing Date", 0, func() (*http.Response, error) { return noDate(), nil }},
		{"invalid Date", 0, func() (*http.Response, error) { return badDate(), nil }},
		{"all errors", -1, func() (*http.Response, error) { return nil, errors.New("dial failed") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClock(base)
			client := &fakeHTTP{fn: func(req *http.Request) (*http.Response, error) {
				if tc.broken < 0 || req.URL.Host == fmt.Sprintf("clock%d.test", tc.broken) {
					return tc.how()
				}
				return dateAt(fc.Now().Add(120 * time.Second)), nil
			}}
			c := newCDNClock(t, fc, client, endpoints(3))

			res, err := c.MaybeCheckCDN(t.Context(), false)
			if err != nil {
				t.Fatalf("MaybeCheckCDN: %v", err)
			}
			if !res.Ran {
				t.Fatal("Result.Ran = false, want true")
			}
			if tc.broken >= 0 && res.Answers != 2 {
				t.Fatalf("Answers = %d, want 2", res.Answers)
			}
			if tc.broken < 0 && res.Answers != 0 {
				t.Fatalf("Answers = %d, want 0", res.Answers)
			}
			if res.Changed || c.Offset() != 0 {
				t.Fatalf("offset changed to %v with fewer than 3 answers", c.Offset())
			}
		})
	}
}

// TestCDNTwoSecondAgreement checks the 2 s window itself: two of three answers form a
// strict majority only when they agree within 2 s.
func TestCDNTwoSecondAgreement(t *testing.T) {
	cases := []struct {
		name   string
		deltas []time.Duration
		want   time.Duration
	}{
		{"pair within the window", []time.Duration{100 * time.Second, 101 * time.Second, 105 * time.Second}, 100 * time.Second},
		{"pair beyond the window from the first answer", []time.Duration{100 * time.Second, 103 * time.Second, 103 * time.Second}, 103 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClock(base)
			m := make(map[string]time.Duration, len(tc.deltas))
			for i, d := range tc.deltas {
				m[fmt.Sprintf("clock%d.test", i)] = d
			}
			client := &fakeHTTP{fn: deltasByHost(fc, m)}
			c := newCDNClock(t, fc, client, endpoints(len(tc.deltas)))

			res, err := c.MaybeCheckCDN(t.Context(), false)
			if err != nil {
				t.Fatalf("MaybeCheckCDN: %v", err)
			}
			if res.Offset != tc.want || res.Agreeing != 2 {
				t.Fatalf("Result = %+v, want offset %v agreed by 2 answers", res, tc.want)
			}
		})
	}
}

// TestCDNNoMajorityKeepsOffset: a later check without a majority must not undo a good
// offset learned earlier.
func TestCDNNoMajorityKeepsOffset(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return dateAt(fc.Now().Add(120 * time.Second)), nil
	}}
	c := newCDNClock(t, fc, client, endpoints(3))

	if _, err := c.MaybeCheckCDN(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if got := c.Offset(); got != 120*time.Second {
		t.Fatalf("first check offset = %v, want 120s", got)
	}

	fc.Advance(10 * time.Minute)
	deltas := []time.Duration{100 * time.Second, -100 * time.Second, 1000 * time.Second}
	client.fn = func(req *http.Request) (*http.Response, error) {
		var index int
		_, _ = fmt.Sscanf(req.URL.Host, "clock%d.test", &index)
		return dateAt(fc.Now().Add(deltas[index])), nil
	}
	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agreeing != 0 || res.Offset != 120*time.Second || res.Changed {
		t.Fatalf("Result = %+v, want the previous 120s offset", res)
	}
}

func TestCDNRateLimit(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return dateAt(fc.Now().Add(120 * time.Second)), nil
	}}
	c := newCDNClock(t, fc, client, endpoints(3))

	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil || !res.Ran {
		t.Fatalf("first check: res=%+v err=%v", res, err)
	}
	if n := client.callCount(); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}

	res, err = c.MaybeCheckCDN(t.Context(), false)
	if err != nil || res.Ran {
		t.Fatalf("second check within 10 minutes: res=%+v err=%v", res, err)
	}
	if res.Offset != 120*time.Second {
		t.Fatalf("gated call Offset = %v, want the current 120s", res.Offset)
	}
	if n := client.callCount(); n != 3 {
		t.Fatalf("requests after a gated call = %d, want 3", n)
	}

	fc.Advance(9 * time.Minute)
	if res, _ := c.MaybeCheckCDN(t.Context(), false); res.Ran {
		t.Fatal("check ran after 9 minutes")
	}
	if n := client.callCount(); n != 3 {
		t.Fatalf("requests after 9 minutes = %d, want 3", n)
	}

	fc.Advance(time.Minute)
	if res, _ := c.MaybeCheckCDN(t.Context(), false); !res.Ran {
		t.Fatal("check did not run after 10 minutes")
	}
	if n := client.callCount(); n != 6 {
		t.Fatalf("requests after 10 minutes = %d, want 6", n)
	}
}

// TestCDNCheckAfterClockJumpBack: fixing the system clock backwards must not leave the
// rate limit looking like it expires in the future.
func TestCDNCheckAfterClockJumpBack(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return dateAt(fc.Now()), nil
	}}
	c := newCDNClock(t, fc, client, endpoints(3))

	if res, err := c.MaybeCheckCDN(t.Context(), false); err != nil || !res.Ran {
		t.Fatalf("first check: res=%+v err=%v", res, err)
	}
	fc.Advance(-time.Hour)
	if res, err := c.MaybeCheckCDN(t.Context(), false); err != nil || !res.Ran {
		t.Fatalf("check after a backwards clock jump: res=%+v err=%v", res, err)
	}
}

func TestCDNTriggers(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(c *Clock, fc *fakeClock)
		allL0Failed bool
		want        bool
	}{
		{"no peer ever", func(*Clock, *fakeClock) {}, false, true},
		{"peer seen just now", func(c *Clock, fc *fakeClock) { c.AddPeerSample(peerID(1), fc.Now()) }, false, false},
		{"peer seen just now, all L0 dials failed",
			func(c *Clock, fc *fakeClock) { c.AddPeerSample(peerID(1), fc.Now()) }, true, true},
		{"peer seen 59 minutes ago",
			func(c *Clock, fc *fakeClock) { c.AddPeerSample(peerID(1), fc.Now()); fc.Advance(59 * time.Minute) }, false, false},
		{"peer seen 61 minutes ago",
			func(c *Clock, fc *fakeClock) { c.AddPeerSample(peerID(1), fc.Now()); fc.Advance(61 * time.Minute) }, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClock(base)
			client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
				return dateAt(fc.Now()), nil
			}}
			c := newCDNClock(t, fc, client, endpoints(3))
			tc.setup(c, fc)
			res, err := c.MaybeCheckCDN(t.Context(), tc.allL0Failed)
			if err != nil {
				t.Fatalf("MaybeCheckCDN: %v", err)
			}
			if res.Ran != tc.want {
				t.Fatalf("Ran = %v, want %v", res.Ran, tc.want)
			}
			if got := client.callCount() > 0; got != tc.want {
				t.Fatalf("HTTP requests made = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCDNRequests(t *testing.T) {
	fc := newFakeClock(base)
	client := &fakeHTTP{fn: deltasByHost(fc, map[string]time.Duration{
		"clock0.test": 30 * time.Second,
		"clock1.test": 30 * time.Second,
		"clock2.test": 30 * time.Second,
	})}
	c := newCDNClock(t, fc, client, endpoints(3))

	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil {
		t.Fatalf("MaybeCheckCDN: %v", err)
	}
	if !res.Changed || res.Warning {
		t.Fatalf("Result = %+v, want a changed 30s offset without a warning", res)
	}
	calls := client.recordedCalls()
	if len(calls) != 3 {
		t.Fatalf("requests = %d, want 3", len(calls))
	}
	for _, call := range calls {
		if call.method != http.MethodHead {
			t.Fatalf("method = %q, want HEAD", call.method)
		}
		u, err := url.Parse(call.url)
		if err != nil || u.Path != "/" {
			t.Fatalf("request URL = %q, want path /", call.url)
		}
		if call.body {
			t.Fatalf("HEAD %s carried a body", call.url)
		}
	}
}

func TestCDNWarning(t *testing.T) {
	cases := []struct {
		name   string
		delta  time.Duration
		want   bool
		warned time.Duration
	}{
		{"above the threshold", 90 * time.Second, true, 90 * time.Second},
		{"below the threshold", 30 * time.Second, false, 0},
		{"negative above the threshold", -90 * time.Second, true, -90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClock(base)
			var warned []time.Duration
			client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
				return dateAt(fc.Now().Add(tc.delta)), nil
			}}
			c, err := New(Config{
				Now:       fc.Now,
				HTTP:      client,
				Endpoints: endpoints(3),
				Warn:      func(off time.Duration) { warned = append(warned, off) },
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			res, err := c.MaybeCheckCDN(t.Context(), false)
			if err != nil {
				t.Fatalf("MaybeCheckCDN: %v", err)
			}
			if res.Warning != tc.want {
				t.Fatalf("Result.Warning = %v, want %v", res.Warning, tc.want)
			}
			if tc.want {
				if len(warned) != 1 || warned[0] != tc.warned {
					t.Fatalf("warned = %v, want [%v]", warned, tc.warned)
				}
			} else if len(warned) != 0 {
				t.Fatalf("warned = %v, want none", warned)
			}
		})
	}
}

// TestCDNWarningNotRepeated: a large offset keeps warning state across checks, so the
// user is not told the same thing every 10 minutes.
func TestCDNWarningNotRepeated(t *testing.T) {
	fc := newFakeClock(base)
	var warned []time.Duration
	client := &fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return dateAt(fc.Now().Add(90 * time.Second)), nil
	}}
	c, err := New(Config{
		Now:       fc.Now,
		HTTP:      client,
		Endpoints: endpoints(3),
		Warn:      func(off time.Duration) { warned = append(warned, off) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.MaybeCheckCDN(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	fc.Advance(10 * time.Minute)
	res, err := c.MaybeCheckCDN(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning {
		t.Fatal("Result.Warning = true on a repeated large offset")
	}
	if len(warned) != 1 {
		t.Fatalf("warned = %v, want one warning", warned)
	}
}

// TestCDNParallel makes each endpoint wait until all three requests are in flight. A
// sequential implementation would never get past the first request.
func TestCDNParallel(t *testing.T) {
	fc := newFakeClock(base)
	var started atomic.Int32
	gate := make(chan struct{})
	client := &fakeHTTP{fn: func(req *http.Request) (*http.Response, error) {
		if started.Add(1) == 3 {
			close(gate)
		}
		select {
		case <-gate:
			return dateAt(fc.Now().Add(120 * time.Second)), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}}
	c := newCDNClock(t, fc, client, endpoints(3))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	res, err := c.MaybeCheckCDN(ctx, false)
	if err != nil {
		t.Fatalf("MaybeCheckCDN: %v", err)
	}
	if res.Answers != 3 || res.Offset != 120*time.Second {
		t.Fatalf("Result = %+v, want 3 parallel answers and a 120s offset", res)
	}
}

func TestNewHTTPClient(t *testing.T) {
	proxy, err := url.Parse("http://127.0.0.1:9050")
	if err != nil {
		t.Fatal(err)
	}
	client := NewHTTPClient(proxy, 3*time.Second)
	if client.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %v, want 3s", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got == nil || got.String() != proxy.String() {
		t.Fatalf("Proxy = %v, want %v", got, proxy)
	}

	direct := NewHTTPClient(nil, 0)
	if got := direct.Transport.(*http.Transport).Proxy; got != nil {
		t.Fatal("direct client has a proxy function")
	}
}
