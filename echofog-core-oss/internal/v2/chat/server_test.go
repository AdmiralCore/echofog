package chat

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type bundler struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (b *bundler) Bundle() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	return []byte("prekeys-envelope"), nil
}

// fakeStream feeds the handler frames and records what it writes.
type fakeStream struct {
	in     chan frame.Frame
	mu     sync.Mutex
	out    []frame.Frame
	code   uint64
	closed bool
	done   chan struct{}
}

func newStream() *fakeStream {
	return &fakeStream{in: make(chan frame.Frame, 16), done: make(chan struct{})}
}

// ReadFrame returns when a frame comes, when the test closes in, or when the stream is closed: a
// closed mux stream wakes its reader the same way.
func (s *fakeStream) ReadFrame() (frame.Frame, error) {
	select {
	case f, ok := <-s.in:
		if !ok {
			return frame.Frame{}, io.EOF
		}
		return f, nil
	case <-s.done:
		return frame.Frame{}, io.EOF
	}
}

func (s *fakeStream) WriteFrame(typ uint64, flags byte, payload []byte) error {
	s.mu.Lock()
	s.out = append(s.out, frame.Frame{Type: typ, Flags: flags, Payload: payload})
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) finish(code uint64) {
	s.mu.Lock()
	if !s.closed {
		s.closed, s.code = true, code
		close(s.done)
	}
	s.mu.Unlock()
}

func (s *fakeStream) CloseWithCode(code uint64, reason string) error { s.finish(code); return nil }
func (s *fakeStream) Close() error                                   { s.finish(0); return nil }

func (s *fakeStream) frames() []frame.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]frame.Frame(nil), s.out...)
}

func get(t *testing.T, req uint64) frame.Frame {
	t.Helper()
	b, err := objects.Marshal(&chatpb.BundleGet{Req: req})
	if err != nil {
		t.Fatal(err)
	}
	return frame.Frame{Type: FrameBundleGet, Payload: b}
}

// run starts Handle on a fresh stream and returns the stream; the caller closes st.in.
func run(t *testing.T, srv *Server, p Peer) (*fakeStream, func()) {
	t.Helper()
	st := newStream()
	done := make(chan struct{})
	go func() { srv.Handle(context.Background(), p, st); close(done) }()
	return st, func() {
		close(st.in)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Handle did not return")
		}
	}
}

func wait(t *testing.T, st *fakeStream, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(st.frames()) < n {
		select {
		case <-st.done:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no answer: %d frames", len(st.frames()))
		}
		time.Sleep(time.Millisecond)
	}
}

func waitDone(t *testing.T, st *fakeStream) {
	t.Helper()
	select {
	case <-st.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream was not closed")
	}
}

func direct(addr string) Peer {
	return Peer{SID: [16]byte{1}, Path: mux.PathDirect, Remote: netip.MustParseAddrPort(addr)}
}

func TestBundleGetAnswers(t *testing.T) {
	t.Parallel()
	b, c := &bundler{}, &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Bundles: b, Now: c.Now})
	st, stop := run(t, srv, direct("203.0.113.5:4430"))
	defer stop()
	st.in <- get(t, 77)
	wait(t, st, 1)
	out := st.frames()[0]
	if out.Type != FrameBundle {
		t.Fatalf("type %#x", out.Type)
	}
	m, err := chatpb.ParseBundle(out.Payload)
	if err != nil || m.Req != 77 || string(m.Prekeys) != "prekeys-envelope" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestBundleGetRateLimitPerSession(t *testing.T) {
	t.Parallel()
	b, c := &bundler{}, &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Bundles: b, Now: c.Now})
	st, stop := run(t, srv, direct("203.0.113.5:4430"))
	defer stop()
	st.in <- get(t, 1)
	st.in <- get(t, 2)
	wait(t, st, 2)
	st.in <- get(t, 3) // the third within a minute
	waitDone(t, st)
	if st.code != 5 { // frame.CodeRefused
		t.Fatalf("code %d", st.code)
	}
	if b.calls != 2 {
		t.Fatalf("a refused request made %d one-time prekeys, want 2", b.calls)
	}
}

func TestBundleGetRateLimitRecovers(t *testing.T) {
	t.Parallel()
	b, c := &bundler{}, &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Bundles: b, Now: c.Now})
	st, stop := run(t, srv, direct("203.0.113.5:4430"))
	defer stop()
	st.in <- get(t, 1)
	st.in <- get(t, 2)
	wait(t, st, 2)
	c.Add(61 * time.Second)
	st.in <- get(t, 3)
	wait(t, st, 3)
	if len(st.frames()) != 3 {
		t.Fatal("not served after the window")
	}
}

// New sessions do not reset the quota: it also holds per network diversity unit, with a factor of 3.
func TestBundleGetRateLimitPerUnit(t *testing.T) {
	t.Parallel()
	b, c := &bundler{}, &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Bundles: b, Now: c.Now})
	served := 0
	for i := 0; i < 5; i++ { // five sessions from one /24
		p := Peer{SID: [16]byte{byte(i + 1)}, Path: mux.PathDirect, Remote: netip.MustParseAddrPort("198.51.100." + string(rune('1'+i)) + ":4430")}
		st, stop := run(t, srv, p)
		st.in <- get(t, 1)
		st.in <- get(t, 2)
		select {
		case <-st.done:
		case <-time.After(200 * time.Millisecond):
		}
		served += len(st.frames())
		stop()
	}
	if want := BundleGetLimit * UnitFactor; served != want {
		t.Fatalf("a /24 got %d bundles in a minute, want %d", served, want)
	}
}

// Over a relay circuit, from a stranger's address: refused (services.md 4.4: direct sessions only).
func TestBundleGetRefusalsAreAlike(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	cases := []struct {
		name string
		p    Peer
		b    *bundler
	}{
		{"relay path", Peer{SID: [16]byte{9}, Path: mux.PathRelay}, &bundler{}},
		{"internal error", direct("203.0.113.9:1"), &bundler{err: errors.New("disk full")}},
	}
	for _, tc := range cases {
		srv := New(Config{Bundles: tc.b, Now: c.Now})
		st, stop := run(t, srv, tc.p)
		st.in <- get(t, 1)
		waitDone(t, st)
		stop()
		if st.code != 5 || len(st.frames()) != 0 {
			t.Errorf("%s: code %d, frames %d", tc.name, st.code, len(st.frames()))
		}
		if tc.name == "relay path" && tc.b.calls != 0 {
			t.Errorf("a one-time prekey was made for a relayed request")
		}
	}
}

func TestMalformedAndUnknownFrames(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	for name, f := range map[string]frame.Frame{
		"non-canonical BUNDLE_GET": {Type: FrameBundleGet, Payload: []byte{0x08, 0x80, 0x00}},
		"garbage":                  {Type: FrameBundleGet, Payload: []byte{0xff, 0xff, 0xff}},
		"BUNDLE sent to a server":  {Type: FrameBundle, Payload: nil},
	} {
		b := &bundler{}
		st, stop := run(t, New(Config{Bundles: b, Now: c.Now}), direct("203.0.113.5:4430"))
		st.in <- f
		waitDone(t, st)
		stop()
		if st.code == 0 || b.calls != 0 {
			t.Errorf("%s: code %d, calls %d", name, st.code, b.calls)
		}
	}
}
