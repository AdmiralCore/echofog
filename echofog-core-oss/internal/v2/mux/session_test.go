package mux

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// testPolicy selects (service, version) pairs; each has a limiter of service-local types.
type testPolicy map[[2]uint64]map[uint64]int

func (p testPolicy) Limiter(svc, ver uint64) (frame.Limiter, bool) {
	types, ok := p[[2]uint64{svc, ver}]
	if !ok {
		return nil, false
	}
	return func(typ uint64) (int, bool) { m, ok := types[typ]; return m, ok }, true
}

// chat/2 with DELIVER (0x40, max 61504) and DELIVERED (0x41, max 32), like services.md §4.1.
var chatPolicy = testPolicy{{3, 2}: {0x40: 61504, 0x41: 32}}

// tap records the bytes written to a carrier.
type tap struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
	// frozen: writes are swallowed (the peer hears nothing), as on a path that died silently.
	frozen atomic.Bool
}

func (t *tap) Write(p []byte) (int, error) {
	if t.frozen.Load() {
		return len(p), nil
	}
	t.mu.Lock()
	t.buf.Write(p)
	t.mu.Unlock()
	return t.Conn.Write(p)
}

func (t *tap) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte{}, t.buf.Bytes()...)
}

type carrierEnds struct {
	ga, gb   *Guard
	ca, cb   *Conn
	ta, tb   *tap
	binding  [32]byte
	sealed   bool
	isClient bool
}

// newCarrier connects side A (carrier client) to side B over a pipe with a fresh channel binding.
func newCarrier(t testing.TB, sealed bool) *carrierEnds {
	a, b := net.Pipe()
	e := &carrierEnds{ta: &tap{Conn: a}, tb: &tap{Conn: b}, sealed: sealed}
	rand.Read(e.binding[:])
	e.ca = NewConn(e.ta, ConnConfig{Client: true, ChannelBinding: e.binding, Sealed: sealed})
	e.cb = NewConn(e.tb, ConnConfig{Client: false, ChannelBinding: e.binding, Sealed: sealed})
	e.ga = NewGuard(e.ca)
	e.gb = NewGuard(e.cb)
	t.Cleanup(func() { e.ca.Close(); e.cb.Close() })
	return e
}

type pair struct {
	a, b   *Session // a: session initiator, carrier client of path 0
	binder *Binder  // b's binder
	path0  *carrierEnds
	abuseA atomic.Int32
	abuseB atomic.Int32
}

func fastTimers() Timers {
	return Timers{Tick: 10 * time.Millisecond, KeepaliveMin: 50 * time.Millisecond, KeepaliveMax: 80 * time.Millisecond,
		PongMin: 100 * time.Millisecond, FailSilence: 400 * time.Millisecond, PathDead: 700 * time.Millisecond, GoAwayDrain: 2 * time.Second,
		BindTimeout: 2 * time.Second, NoPathGrace: 2 * time.Second}
}

// sessionPair runs a fake handshake (one HS_HELLO frame on stream 0) and starts both sessions
// with the kdf.json keys. mod may adjust both configs.
func sessionPair(t testing.TB, sealed bool, mod func(a, b *Config)) *pair {
	t.Helper()
	ik, rk := vectorKeys(t)
	pr := &pair{binder: NewBinder()}
	ca := Config{Keys: ik, Policy: chatPolicy, Features: []uint16{frame.FeaturePathMigration},
		OnAbuse: func(Abuse) { pr.abuseA.Add(1) }}
	cb := Config{Keys: rk, Policy: chatPolicy, Features: []uint16{frame.FeaturePathMigration},
		OnAbuse: func(Abuse) { pr.abuseB.Add(1) }}
	if mod != nil {
		mod(&ca, &cb)
	}
	e := newCarrier(t, sealed)
	pr.path0 = e
	ctx := ctxT(t)
	raw0, err := e.ca.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}})
	if _, err := raw0.Write(hello); err != nil {
		t.Fatal(err)
	}
	_, pend, err := pr.binder.Accept(ctx, e.gb, PathDirect)
	if !errors.Is(err, ErrNewSession) || pend.First.Type != frame.TypeHsHello {
		t.Fatalf("binder did not hand over the handshake: %v", err)
	}
	if pr.a, err = NewSession(ca, e.ga, raw0, PathDirect); err != nil {
		t.Fatal(err)
	}
	if pr.b, err = NewSession(cb, pend.Guard, pend.Stream, PathDirect); err != nil {
		t.Fatal(err)
	}
	pr.binder.Add(pr.b)
	t.Cleanup(func() { pr.a.Close(); pr.b.Close() })
	return pr
}

func deliver(req byte, body string) []byte {
	return append([]byte{0x08, req, 0x12, byte(len(body))}, body...)
}

func TestSessionStreamRoundTrip(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		pr := sessionPair(t, sealed, nil)
		ctx := ctxT(t)
		st, err := pr.a.OpenStream(ctx, 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Selected() {
			t.Fatal("chat/2 not selected")
		}
		st.WriteFrame(0x40, 0, deliver(1, "hello"))
		rs, err := pr.b.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if o := rs.Open(); o.Service != 3 || o.Version != 2 || !rs.Selected() {
			t.Fatalf("open %+v selected %v", o, rs.Selected())
		}
		f, err := rs.ReadFrame()
		if err != nil || f.Type != 0x40 || !bytes.Equal(f.Payload, deliver(1, "hello")) {
			t.Fatalf("%+v %v", f, err)
		}
		rs.WriteFrame(0x41, 0, []byte{0x08, 1})
		rs.CloseWrite()
		f, err = st.ReadFrame()
		if err != nil || f.Type != 0x41 {
			t.Fatalf("%+v %v", f, err)
		}
		if _, err := st.ReadFrame(); err != io.EOF {
			t.Fatalf("end: %v", err)
		}
		if pr.abuseA.Load()+pr.abuseB.Load() != 0 {
			t.Fatal("abuse reported on a clean exchange")
		}
	}
}

func TestSessionUnknownFramesOnServiceStream(t *testing.T) {
	pr := sessionPair(t, false, nil)
	ctx := ctxT(t)
	st, _ := pr.a.OpenStream(ctx, 3, 2)
	st.WriteGrease()
	st.WritePadding(500)
	st.WriteFrame(0x7f, frame.FlagIgnorable, []byte("from a later chat/2"))
	st.WriteFrame(0x30, frame.FlagIgnorable, []byte("a later session-level frame"))
	st.WriteFrame(0x40, 0, deliver(1, "x"))
	st.WriteFrame(0x7e, 0, []byte("must be understood"))
	rs, _ := pr.b.AcceptStream(ctx)
	f, err := rs.ReadFrame()
	if err != nil || f.Type != 0x40 {
		t.Fatalf("ignorable frames not skipped: %+v %v", f, err)
	}
	if _, err := rs.ReadFrame(); !frame.IsUnknown(err) {
		t.Fatalf("unknown non-ignorable: %v", err)
	}
	// The sender gets a polite CLOSE{unknown-frame}.
	_, err = st.ReadFrame()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != frame.CodeUnknownFrame || !ce.Remote {
		t.Fatalf("sender saw %v, want CLOSE{unknown-frame}", err)
	}
	// Never a penalty, and the session lives on.
	if pr.abuseA.Load()+pr.abuseB.Load() != 0 {
		t.Fatal("unknown frames counted as abuse")
	}
	st2, err := pr.a.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	st2.WriteFrame(0x40, 0, deliver(2, "still here"))
	rs2, _ := pr.b.AcceptStream(ctx)
	if f, err := rs2.ReadFrame(); err != nil || f.Type != 0x40 {
		t.Fatalf("%v", err)
	}
}

func TestSessionUnknownFramesOnControlStream(t *testing.T) {
	pr := sessionPair(t, false, nil)
	// IGNORABLE unknown and GREASE on stream 0: skipped.
	if err := pr.a.SendControl(0x15, frame.FlagIgnorable, []byte("v2.1 control frame")); err != nil {
		t.Fatal(err)
	}
	pr.a.SendGrease()
	if _, err := pr.a.Ping(ctxT(t)); err != nil {
		t.Fatalf("session broken by ignorable control frames: %v", err)
	}
	// A feature frame of a feature that was not negotiated, with IGNORABLE: skipped too.
	pr.a.path0Ctrl().WriteFrame(frame.TypeCardUpdate, frame.FlagIgnorable, []byte("card"))
	if _, err := pr.a.Ping(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	// Unknown without IGNORABLE on stream 0: GOAWAY{unknown-frame}, session ends, no abuse.
	pr.a.SendControl(0x15, 0, []byte("x"))
	select {
	case <-pr.b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session not ended")
	}
	select {
	case <-pr.a.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not get GOAWAY")
	}
	var ga *GoAwayError
	if err := pr.a.Err(); !errors.As(err, &ga) || ga.Code != frame.CodeUnknownFrame || !ga.Remote {
		t.Fatalf("sender ended with %v, want GOAWAY{unknown-frame}", err)
	}
	if pr.abuseA.Load()+pr.abuseB.Load() != 0 {
		t.Fatal("unknown control frame counted as abuse")
	}
}

func (s *Session) path0Ctrl() *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paths[0].ctrl
}

func TestSessionFeatureFramesRequireFeature(t *testing.T) {
	if err := sessionPair(t, false, nil).a.SendControl(frame.TypeObservedAddr, 0, []byte{4, 1, 2, 3, 4, 0, 1}); !errors.Is(err, ErrFeatureNotSelected) {
		t.Fatalf("sent a frame of an unnegotiated feature: %v", err)
	}
	var got atomic.Int32
	pr := sessionPair(t, false, func(a, b *Config) {
		a.Features = append(a.Features, frame.FeatureObservedAddr)
		b.Features = append(b.Features, frame.FeatureObservedAddr)
		b.OnControl = func(id uint64, f frame.Frame) {
			if f.Type == frame.TypeObservedAddr {
				got.Add(1)
			}
		}
	})
	if err := pr.a.SendControl(frame.TypeObservedAddr, 0, []byte{4, 192, 0, 2, 33, 0xc8, 0x22}); err != nil {
		t.Fatal(err)
	}
	pr.a.Ping(ctxT(t))
	if got.Load() != 1 {
		t.Fatal("OBSERVED_ADDR not delivered")
	}
}

func TestSessionAbuseOnlyForKnownMalformed(t *testing.T) {
	// A malformed OPEN (a known type) is abuse and closes the stream with protocol-violation.
	pr := sessionPair(t, false, nil)
	ctx := ctxT(t)
	raw, _ := pr.path0.ca.OpenStream(ctx)
	raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: []byte{3}}))
	f, err := frame.NewReader(raw, nil).ReadFrame()
	c, _ := frame.ParseClose(f.Payload)
	if err != nil || f.Type != frame.TypeClose || c.Code != frame.CodeProtocolViolation {
		t.Fatalf("%+v %v", f, err)
	}
	waitFor(t, func() bool { return pr.abuseB.Load() == 1 })
	// A first frame that is not OPEN: protocol violation, abuse.
	raw2, _ := pr.path0.ca.OpenStream(ctx)
	raw2.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypePing, Payload: make([]byte, 8)}))
	waitFor(t, func() bool { return pr.abuseB.Load() == 2 })
	// A known service type over its maximum: CLOSE{frame-too-large}, abuse.
	st, _ := pr.a.OpenStream(ctx, 3, 2)
	st.WriteFrame(0x41, 0, make([]byte, 33))
	rs, _ := pr.b.AcceptStream(ctx)
	if _, err := rs.ReadFrame(); !frame.IsTooLarge(err) {
		t.Fatalf("%v", err)
	}
	_, err = st.ReadFrame()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != frame.CodeFrameTooLarge {
		t.Fatalf("%v", err)
	}
	waitFor(t, func() bool { return pr.abuseB.Load() == 3 })
	// An unknown type over 1 MiB is too large, but not abuse.
	st2, _ := pr.a.OpenStream(ctx, 3, 2)
	st2.w.Write(frame.AppendFrameHeader(nil, 0x7d, frame.FlagIgnorable, frame.MaxPayload+1))
	rs2, _ := pr.b.AcceptStream(ctx)
	if _, err := rs2.ReadFrame(); !frame.IsTooLarge(err) {
		t.Fatalf("%v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if pr.abuseB.Load() != 3 {
		t.Fatal("unknown oversized frame counted as abuse")
	}
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSessionStreamLimit(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) { a.MaxStreams = 1000; b.MaxStreams = 3 })
	ctx := ctxT(t)
	var sts []*Stream
	for i := 0; i < 4; i++ {
		st, err := pr.a.OpenStream(ctx, 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		sts = append(sts, st)
	}
	_, err := sts[3].ReadFrame()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != frame.CodeRefused {
		t.Fatalf("fourth stream: %v, want CLOSE{refused}", err)
	}
	if pr.abuseB.Load() != 0 {
		t.Fatal("refusal counted as abuse")
	}
	// The local limit refuses before sending.
	pr2 := sessionPair(t, false, func(a, b *Config) { a.MaxStreams = 2 })
	pr2.a.OpenStream(ctx, 3, 2)
	pr2.a.OpenStream(ctx, 3, 2)
	if _, err := pr2.a.OpenStream(ctx, 3, 2); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("%v", err)
	}
}

func TestSessionControlLimits(t *testing.T) {
	// PING must carry 8 bytes: 9 is frame-too-large (abuse), GOAWAY ends the session.
	pr := sessionPair(t, false, nil)
	pr.a.path0Ctrl().w.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypePing, Payload: make([]byte, 9)}))
	<-pr.b.Done()
	if pr.abuseB.Load() != 1 {
		t.Fatal("oversized PING not abuse")
	}
	// A GOAWAY that is not a valid protobuf: protocol violation.
	pr = sessionPair(t, false, nil)
	pr.a.path0Ctrl().WriteFrame(frame.TypeGoAway, 0, []byte{0x0a, 0x05})
	<-pr.b.Done()
	if pr.abuseB.Load() != 1 {
		t.Fatal("malformed GOAWAY not abuse")
	}
	// OPEN on the control stream: protocol violation.
	pr = sessionPair(t, false, nil)
	pr.a.path0Ctrl().WriteFrame(frame.TypeOpen, 0, []byte{3, 2, 0})
	<-pr.b.Done()
}

func TestSessionPreHandshakeStreamsRejected(t *testing.T) {
	e := newCarrier(t, false)
	ctx := ctxT(t)
	// The carrier client opens stream 2 before stream 0 is bound.
	rawX, _ := e.ca.OpenStream(ctx)
	raw0 := rawX
	rawX, _ = e.ca.OpenStream(ctx)
	rawX.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: []byte{3, 2, 0}}))
	f, err := frame.NewReader(rawX, nil).ReadFrame()
	c, _ := frame.ParseClose(f.Payload)
	if err != nil || f.Type != frame.TypeClose || c.Code != frame.CodeProtocolViolation {
		t.Fatalf("early stream: %+v %v", f, err)
	}
	if e.gb.Rejected() != 1 {
		t.Fatalf("rejected %d", e.gb.Rejected())
	}
	// Stream 0 is still served.
	raw0.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}}))
	if _, _, err := NewBinder().Accept(ctx, e.gb, PathDirect); !errors.Is(err, ErrNewSession) {
		t.Fatal(err)
	}
}

func TestSessionStreamResetAndClose(t *testing.T) {
	pr := sessionPair(t, false, nil)
	ctx := ctxT(t)
	st, _ := pr.a.OpenStream(ctx, 3, 2)
	st.WriteFrame(0x40, 0, deliver(1, "a"))
	rs, _ := pr.b.AcceptStream(ctx)
	rs.ReadFrame()
	st.Reset(codeCancelled)
	_, err := rs.ReadFrame()
	var re *ResetError
	if !errors.As(err, &re) || re.Code != codeCancelled || !re.Remote {
		t.Fatalf("%v", err)
	}
	// CLOSE{code, reason}: the reason is delivered before the reset that follows it.
	st2, _ := pr.a.OpenStream(ctx, 3, 2)
	st2.WriteFrame(0x40, 0, deliver(2, "b"))
	rs2, _ := pr.b.AcceptStream(ctx)
	rs2.ReadFrame()
	rs2.CloseWithCode(0x101, "policy")
	_, err = st2.ReadFrame()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != 0x101 || ce.Reason != "policy" {
		t.Fatalf("%v", err)
	}
	// Both streams are released: the stream counts return to zero.
	waitFor(t, func() bool {
		pr.a.mu.Lock()
		defer pr.a.mu.Unlock()
		return pr.a.localStreams == 0
	})
}

func TestSessionBackpressure(t *testing.T) {
	pr := sessionPair(t, true, nil)
	ctx := ctxT(t)
	st, _ := pr.a.OpenStream(ctx, 3, 2)
	var sent atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 64; i++ { // 64 × 60 KiB ≈ 3.8 MiB, far beyond the 256 KiB stream window
			if st.WriteFrame(0x40, 0, make([]byte, 60000)) != nil {
				return
			}
			sent.Add(1)
		}
	}()
	rs, _ := pr.b.AcceptStream(ctx)
	time.Sleep(300 * time.Millisecond)
	if n := sent.Load(); n*60000 > 2*DefaultStreamWindow {
		t.Fatalf("writer was not held back: %d frames sent while the reader slept", n)
	}
	for i := 0; i < 64; i++ {
		if f, err := rs.ReadFrame(); err != nil || len(f.Payload) != 60000 {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	<-done
}

func TestSessionGoAway(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) { a.Timers = fastTimers(); b.Timers = fastTimers() })
	ctx := ctxT(t)
	st, _ := pr.a.OpenStream(ctx, 3, 2)
	st.WriteFrame(0x40, 0, deliver(1, "a"))
	rs, _ := pr.b.AcceptStream(ctx)
	rs.ReadFrame()
	pr.b.GoAway(frame.CodeGoingAway, "restart", 30)
	waitFor(t, func() bool { _, err := pr.a.OpenStream(ctx, 3, 2); return errors.Is(err, ErrGoingAway) })
	// The open stream still works until it ends.
	st.WriteFrame(0x40, 0, deliver(2, "b"))
	if f, err := rs.ReadFrame(); err != nil || f.Type != 0x40 {
		t.Fatalf("%v", err)
	}
	st.Close()
	rs.Close()
	select {
	case <-pr.b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed after its streams ended")
	}
	var ga *GoAwayError
	if err := pr.a.Err(); !errors.As(err, &ga) || ga.Reason != "restart" || ga.RetryAfter != 30 {
		<-pr.a.Done()
		if err := pr.a.Err(); !errors.As(err, &ga) {
			t.Fatalf("%v", err)
		}
	}
}

func TestSessionGoAwayDrainTimeout(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) { a.Timers = fastTimers(); b.Timers = fastTimers() })
	st, _ := pr.a.OpenStream(ctxT(t), 3, 2)
	st.WriteFrame(0x40, 0, deliver(1, "a"))
	if _, err := pr.b.AcceptStream(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	pr.b.GoAway(frame.CodeGoingAway, "", 0)
	<-pr.b.Done()
	if d := time.Since(start); d < fastTimers().GoAwayDrain-100*time.Millisecond {
		t.Fatalf("closed after %v, before the drain time with a stream open", d)
	}
}

func TestSessionKeepaliveAndDeadPath(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) { a.Timers = fastTimers(); b.Timers = fastTimers() })
	// Idle for longer than the dead-path time: keepalive PINGs keep it alive.
	time.Sleep(3 * fastTimers().PathDead / 2)
	if _, active, ok := pr.a.Paths(); !ok || active != 0 {
		t.Fatal("idle path died despite keepalives")
	}
	// The path goes silent (writes from B vanish): A declares it dead after PathDead.
	pr.path0.tb.frozen.Store(true)
	waitFor(t, func() bool { ids, _, _ := pr.a.Paths(); return len(ids) == 0 })
	// Without any path, the session ends after its grace time.
	select {
	case <-pr.a.Done():
		if !errors.Is(pr.a.Err(), ErrNoPath) {
			t.Fatalf("%v", pr.a.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session without paths not closed")
	}
}

func TestSessionDatagrams(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		pr := sessionPair(t, sealed, nil)
		ctx := ctxT(t)
		if err := pr.a.SendDatagram(frame.DgramCallAudio, []byte("media-unit")); err != nil {
			t.Fatal(err)
		}
		d, err := pr.b.ReceiveDatagram(ctx)
		if err != nil || d.Type != frame.DgramCallAudio || string(d.Payload) != "media-unit" {
			t.Fatalf("%+v %v", d, err)
		}
		// Unknown datagram types are dropped and counted, never answered.
		pr.a.SendDatagram(0x77, []byte("future"))
		pr.a.SendDatagram(frame.DgramCallVideo, []byte("v"))
		d, _ = pr.b.ReceiveDatagram(ctx)
		if d.Type != frame.DgramCallVideo || pr.b.Stats().DatagramsUnknown != 1 {
			t.Fatalf("%+v %+v", d, pr.b.Stats())
		}
		wire := pr.path0.ta.Bytes()
		if sealed == bytes.Contains(wire, []byte("media-unit")) {
			t.Fatalf("sealed=%v but plaintext on the wire=%v", sealed, !sealed)
		}
	}
}

func TestSealedModeHidesFramesAndUsesRecords(t *testing.T) {
	secret := []byte("SECRET-PAYLOAD-0123456789")
	for _, sealed := range []bool{false, true} {
		pr := sessionPair(t, sealed, nil)
		ctx := ctxT(t)
		st, _ := pr.a.OpenStream(ctx, 3, 2)
		st.WriteFrame(0x40, 0, append(deliver(1, ""), secret...))
		rs, _ := pr.b.AcceptStream(ctx)
		f, err := rs.ReadFrame()
		if err != nil || !bytes.Contains(f.Payload, secret) {
			t.Fatalf("sealed=%v: %v", sealed, err)
		}
		pr.a.Ping(ctx) // control stream 0 is sealed too
		wire := pr.path0.ta.Bytes()
		if got := bytes.Contains(wire, secret); got == sealed {
			t.Fatalf("sealed=%v: plaintext visible on the wire = %v", sealed, got)
		}
		// In sealed mode, OPEN and PING do not appear in clear inside MUX_STREAM data.
		openFrame := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeOpen, Payload: frame.Open{Service: 3, Version: 2}.Append(nil)})
		if got := bytes.Contains(wire, openFrame); got == sealed {
			t.Fatalf("sealed=%v: OPEN visible = %v", sealed, got)
		}
	}
}

// codeCancelled is a service-defined code (0x100 + n).
const codeCancelled = 0x105
