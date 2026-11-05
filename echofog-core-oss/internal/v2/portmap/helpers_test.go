package portmap_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/portmap"
)

var (
	gwAddr    = netip.MustParseAddr("192.168.1.1")
	localAddr = netip.MustParseAddr("192.168.1.10")
	extAddr   = netip.MustParseAddr("8.8.4.4")
)

const (
	udpPort uint16 = 4433
	tcpPort uint16 = 4434
	descURL        = "http://192.168.1.1:5000/rootDesc.xml"
)

func vec(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".hex") {
		b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}
	return raw
}

// fakeClock is a manually advanced carrier.Clock (AD13).
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
	c       *fakeClock
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f, c: c}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	rest := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.stopped || t.fired:
		case !t.at.After(c.now):
			t.fired = true
			due = append(due, t)
		default:
			rest = append(rest, t)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.f()
	}
}

// harness is a fake home network: one gateway that answers PCP/NAT-PMP from a script of
// request→response vectors and UPnP from an HTTP handler.
type harness struct {
	t     *testing.T
	clock *fakeClock
	cfg   portmap.Config

	mu         sync.Mutex
	script     map[string][][]byte // request hex → responses (empty: silence)
	udpReqs    [][]byte
	unexpected [][]byte
	dials      int
	httpReqs   []string // "METHOD url SOAPAction"
	httpBodies []string
	httpFn     func(req *http.Request, body []byte) (int, []byte)
	ssdp       []string

	gwCalls, localCalls, ssdpCalls atomic.Int32
	changes                        atomic.Int32
	changed                        chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		clock:   &fakeClock{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)},
		script:  map[string][][]byte{},
		changed: make(chan struct{}, 256),
	}
	nonces := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, 16)
	h.cfg = portmap.Config{
		Enabled: true,
		Clock:   h.clock,
		Gateway: func() (netip.Addr, error) { h.gwCalls.Add(1); return gwAddr, nil },
		Local:   func(netip.Addr) (netip.Addr, error) { h.localCalls.Add(1); return localAddr, nil },
		Dial:    h.dial,
		HTTP:    doerFunc(h.do),
		SSDP: func(context.Context) ([]string, error) {
			h.ssdpCalls.Add(1)
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.ssdp, nil
		},
		Rand:     func() float64 { return 0.5 },
		Nonce:    bytes.NewReader(nonces),
		Lifetime: time.Hour,
		OnChange: func() {
			h.changes.Add(1)
			select {
			case h.changed <- struct{}{}:
			default:
			}
		},
	}
	return h
}

func (h *harness) start() *portmap.Manager {
	m := portmap.New(h.cfg)
	h.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m
}

// on scripts the gateway: request vector → response vectors (none: silence).
func (h *harness) on(req string, resp ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var rs [][]byte
	for _, r := range resp {
		rs = append(rs, vec(h.t, r))
	}
	h.script[hex.EncodeToString(vec(h.t, req))] = rs
}

func (h *harness) sent() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.udpReqs...)
}

func (h *harness) count(req string) int {
	want := vec(h.t, req)
	n := 0
	for _, r := range h.sent() {
		if bytes.Equal(r, want) {
			n++
		}
	}
	return n
}

func (h *harness) noUnexpected() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range h.unexpected {
		h.t.Errorf("unexpected packet to the gateway: %x", u)
	}
}

// traffic is every packet and request that reached the network.
func (h *harness) traffic() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dials + len(h.udpReqs) + len(h.httpReqs)
}

func (h *harness) dial(_ context.Context, remote netip.AddrPort) (portmap.PacketConn, error) {
	if remote != netip.AddrPortFrom(gwAddr, 5351) {
		h.t.Errorf("dialed %v, want the gateway PCP/NAT-PMP port", remote)
		return nil, errors.New("unexpected remote")
	}
	h.mu.Lock()
	h.dials++
	h.mu.Unlock()
	return &fakeConn{h: h, in: make(chan []byte, 16), done: make(chan struct{})}, nil
}

type fakeConn struct {
	h    *harness
	in   chan []byte
	done chan struct{}
	once sync.Once
}

func (c *fakeConn) Write(p []byte) (int, error) {
	c.h.mu.Lock()
	c.h.udpReqs = append(c.h.udpReqs, append([]byte(nil), p...))
	resp, known := c.h.script[hex.EncodeToString(p)]
	if !known {
		c.h.unexpected = append(c.h.unexpected, append([]byte(nil), p...))
	}
	c.h.mu.Unlock()
	for _, r := range resp {
		c.in <- r
	}
	return len(p), nil
}

func (c *fakeConn) Read(p []byte) (int, error) {
	select {
	case b := <-c.in:
		return copy(p, b), nil
	case <-c.done:
		return 0, io.EOF
	}
}

func (c *fakeConn) Close() error { c.once.Do(func() { close(c.done) }); return nil }

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func (h *harness) do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	h.mu.Lock()
	h.httpReqs = append(h.httpReqs, req.Method+" "+req.URL.String()+" "+req.Header.Get("SOAPAction"))
	h.httpBodies = append(h.httpBodies, string(body))
	fn := h.httpFn
	h.mu.Unlock()
	if fn == nil {
		return nil, errors.New("no HTTP handler")
	}
	status, out := fn(req, body)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(out)), Header: http.Header{}, Request: req}, nil
}

func (h *harness) httpLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.httpReqs...)
}

func (h *harness) soapBodies(action string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for i, r := range h.httpReqs {
		if strings.HasSuffix(r, "#"+action+"\"") {
			out = append(out, h.httpBodies[i])
		}
	}
	return out
}

// upnp makes the gateway a UPnP IGD: desc is the description file, soap maps an action to its
// response files (answered in order, the last one repeats).
func (h *harness) upnp(desc string, soap map[string][]string) {
	h.mu.Lock()
	h.ssdp = []string{descURL}
	h.mu.Unlock()
	var cmu sync.Mutex
	calls := map[string]int{}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpFn = func(req *http.Request, _ []byte) (int, []byte) {
		if req.Method == http.MethodGet {
			if req.URL.String() == descURL {
				return 200, vec(h.t, desc)
			}
			return 404, nil
		}
		if req.URL.Host != "192.168.1.1:5000" || req.URL.Path != "/ctl/IPConn" {
			h.t.Errorf("SOAP to %s", req.URL)
			return 404, nil
		}
		sa := strings.Trim(req.Header.Get("SOAPAction"), `"`)
		action := sa[strings.LastIndex(sa, "#")+1:]
		files := soap[action]
		if len(files) == 0 {
			return 500, nil
		}
		cmu.Lock()
		i := calls[action]
		calls[action]++
		cmu.Unlock()
		f := files[min(i, len(files)-1)]
		status := 200
		if strings.HasPrefix(f, "upnp_err") {
			status = 500
		}
		return status, vec(h.t, f)
	}
}

// eventually waits in real time for cond, without moving the fake clock.
func (h *harness) eventually(cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatal("condition not reached")
		}
		select {
		case <-h.changed:
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// advanceUntil moves the fake clock by step until cond holds; real-time pauses let the
// manager's goroutine re-arm its timers.
func (h *harness) advanceUntil(step time.Duration, max int, cond func() bool) {
	h.t.Helper()
	for i := 0; i < max; i++ {
		if cond() {
			return
		}
		h.clock.Advance(step)
		time.Sleep(3 * time.Millisecond)
	}
	if !cond() {
		h.t.Fatal("condition not reached by advancing the fake clock")
	}
}

// settle gives the manager's goroutine real time to react to the last clock move.
func settle() { time.Sleep(60 * time.Millisecond) }

func mapped(m *portmap.Manager, n int) func() bool {
	return func() bool { return len(m.Mappings()) >= n }
}

func find(ms []portmap.Mapping, p portmap.Proto, internal uint16) (portmap.Mapping, bool) {
	for _, m := range ms {
		if m.Proto == p && m.Internal == internal {
			return m, true
		}
	}
	return portmap.Mapping{}, false
}

func ap(port uint16) netip.AddrPort { return netip.AddrPortFrom(extAddr, port) }
