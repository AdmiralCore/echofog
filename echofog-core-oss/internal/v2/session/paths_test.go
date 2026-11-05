package session_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// tapCarrier records every byte this side writes to its carrier streams.
type tapCarrier struct {
	mux.Carrier
	mu  sync.Mutex
	buf bytes.Buffer
}

type tapStream struct {
	mux.RawStream
	c *tapCarrier
}

func (s *tapStream) Write(p []byte) (int, error) {
	s.c.mu.Lock()
	s.c.buf.Write(p)
	s.c.mu.Unlock()
	return s.RawStream.Write(p)
}

func (c *tapCarrier) OpenStream(ctx context.Context) (mux.RawStream, error) {
	s, err := c.Carrier.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return &tapStream{RawStream: s, c: c}, nil
}

func (c *tapCarrier) AcceptStream(ctx context.Context) (mux.RawStream, error) {
	s, err := c.Carrier.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &tapStream{RawStream: s, c: c}, nil
}

func (c *tapCarrier) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

// paddingFrames counts complete PADDING frames in captured plaintext.
func paddingFrames(b []byte) int {
	n := 0
	for len(b) > 0 {
		f, used, err := frame.ParseFrame(b, nil)
		if err != nil {
			return n
		}
		if f.Type == frame.TypePadding {
			n++
		}
		b = b[used:]
	}
	return n
}

func hasFeature(est *session.Established, id uint16) bool {
	for _, f := range est.Features {
		if uint16(f) == id {
			return true
		}
	}
	return false
}

func waitPath(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// chatPolicy is the stream policy of the tests: chat/2 with DELIVER (0x40) and DELIVERED (0x41).
type chatPolicy struct{}

func (chatPolicy) Limiter(service, version uint64) (frame.Limiter, bool) {
	if service != 3 || version != 2 {
		return nil, false
	}
	return func(typ uint64) (int, bool) {
		switch typ {
		case 0x40:
			return 61504, true
		case 0x41:
			return 32, true
		}
		return 0, false
	}, true
}

func chatPolicyFunc() session.PolicyFunc {
	return func([]handshake.ServiceChoice) mux.StreamPolicy { return chatPolicy{} }
}

// establishWithBinder establishes a session whose responder accepts later PATH_BIND frames
// (wire.md Â§8.2). The first carrier is a normal handshake.
func establishWithBinder(t *testing.T, pair *memcarrier.Pair, card *objects.Card,
	cli, srv session.LocalIdentity, cfg session.AcceptConfig,
	opts ...session.DialOption) (*session.Established, *session.Established, *mux.Binder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	binder := mux.NewBinder()
	cfg.Binder = binder
	cfg.Local = srv
	if cfg.Policy == nil {
		cfg.Policy = chatPolicyFunc()
	}

	dialed := make(chan runResult, 1)
	dOpts := append([]session.DialOption{session.WithCarrier(pair.Client), session.WithPolicy(chatPolicyFunc())}, opts...)
	go func() {
		est, err := session.Dial(ctx, card, dialAttempt(), cli, dOpts...)
		dialed <- runResult{est, err}
	}()
	srvEst, err := session.Accept(ctx, &oneListener{c: pair.Server}, cfg)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	r := <-dialed
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	binder.Add(srvEst.Session)
	t.Cleanup(func() {
		_ = srvEst.Session.Close()
		_ = r.est.Session.Close()
	})
	return r.est, srvEst, binder
}

// bindSecondPath opens another carrier from a to b and binds it. It returns the path ID.
func bindSecondPath(t *testing.T, a *session.Established, binder *mux.Binder,
	pair *memcarrier.Pair, kind mux.PathKind) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := make(chan resultUint64, 1)
	go func() {
		id, err := a.BindCarrier(ctx, pair.Client, kind)
		ch <- resultUint64{id, err}
	}()
	if s, pend, err := binder.Accept(ctx, mux.NewGuard(pair.Server), kind); err != nil || pend != nil || s == nil {
		t.Fatalf("binder.Accept: session %v pending %v err %v", s, pend, err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("BindCarrier: %v", r.err)
	}
	return r.id
}

type resultUint64 struct {
	id  uint64
	err error
}

// TestAdditionalCarrierBindsAsPath pins wire.md Â§8.2: a second carrier to the same peer becomes
// a path of the same session, without a new handshake.
func TestAdditionalCarrierBindsAsPath(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	aLocal.Features = []uint32{uint32(frame.FeaturePathMigration)}
	bLocal.Features = aLocal.Features

	est, srv, binder := establishWithBinder(t, memcarrier.NewQUIC(), verifyCard(t, bc.peer, now),
		aLocal, bLocal, session.AcceptConfig{})

	pair := memcarrier.NewQUIC()
	id := bindSecondPath(t, est, binder, pair, mux.PathDirect)
	if id != 2 {
		t.Fatalf("first path created by the initiator is %d, want 2 (wire.md Â§8.1)", id)
	}
	for name, e := range map[string]*session.Established{"initiator": est, "responder": srv} {
		ids, _, ok := e.Paths()
		if !ok || len(ids) != 2 {
			t.Fatalf("%s paths %v ok %v", name, ids, ok)
		}
	}
	if est.SID != srv.SID {
		t.Fatal("a bound path changed the session ID")
	}
	// Streams work over the new path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := est.Session.SetActivePath(id); err != nil {
		t.Fatal(err)
	}
	st, err := est.Session.OpenStream(ctx, 3, 2)
	if err != nil || st.PathID() != 2 {
		t.Fatalf("stream on path %d: %v", st.PathID(), err)
	}
	_ = st.Close()
}

// TestReplayedPathBindOnAnotherTLSIsSilent pins wire.md Â§8.2: the MAC covers the channel binding
// of the new carrier, so a PATH_BIND captured on one TLS session is refused on another with no
// bytes in reply.
func TestReplayedPathBindOnAnotherTLSIsSilent(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	aLocal.Features = []uint32{uint32(frame.FeaturePathMigration)}
	bLocal.Features = aLocal.Features

	est, _, binder := establishWithBinder(t, memcarrier.NewQUIC(), verifyCard(t, bc.peer, now),
		aLocal, bLocal, session.AcceptConfig{})

	// A valid bind, captured at the carrier level.
	pair := memcarrier.NewQUIC()
	tap := &tapCarrier{Carrier: pair.Client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := make(chan error, 1)
	go func() {
		_, _, err := binder.Accept(ctx, mux.NewGuard(pair.Server), mux.PathDirect)
		ch <- err
	}()
	if _, err := est.BindCarrier(ctx, tap, mux.PathDirect); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	captured := tap.Bytes()
	if len(captured) == 0 {
		t.Fatal("PATH_BIND not captured")
	}

	// Replay it on another carrier (another TLS session, another channel binding).
	other := memcarrier.NewQUIC()
	raw, err := other.Client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write(captured); err != nil {
		t.Fatal(err)
	}
	counted := &countingCarrier{Carrier: other.Server}
	_, err = session.Accept(ctx, &oneListener{c: counted}, session.AcceptConfig{Local: bLocal, Binder: binder})
	if !errors.Is(err, session.ErrSilent) {
		t.Fatalf("replayed PATH_BIND: %v, want ErrSilent", err)
	}
	if n := counted.Bytes(); n != 0 {
		t.Fatalf("refusal wrote %d bytes", n)
	}
	// The session survived the attempt.
	if isClosed(est.Session) {
		t.Fatal("a refused PATH_BIND ended the session")
	}
}

// TestRelayChainAsPath pins wire.md Â§8 and Â§8.4 over a fake relay circuit: the session runs on a
// path whose kind is relay and whose records are sealed.
func TestRelayChainAsPath(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	pair := memcarrier.NewQUIC()
	tap := &tapCarrier{Carrier: pair.Client}
	cfg := session.AcceptConfig{Kind: mux.PathRelay}
	est, srv, _ := establishWithBinder(t, pair, verifyCard(t, bc.peer, now), aLocal, bLocal, cfg,
		session.WithPathKind(mux.PathRelay))

	if est.Kind != mux.PathRelay || srv.Kind != mux.PathRelay {
		t.Fatalf("path kinds %v / %v, want relay", est.Kind, srv.Kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := est.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(0x40, 0, []byte("RELAY-SECRET")); err != nil {
		t.Fatal(err)
	}
	rs, err := srv.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := rs.ReadFrame(); err != nil || !bytes.Contains(f.Payload, []byte("RELAY-SECRET")) {
		t.Fatalf("relay stream: %+v %v", f, err)
	}
	if bytes.Contains(tap.Bytes(), []byte("RELAY-SECRET")) {
		t.Fatal("plaintext on a relay path")
	}
}

// TestServiceStreamsSurvivePathDeath is the task's TDD line: when a path dies, its service
// streams get path-failed; the service reopens a stream on the surviving path and the session,
// its keys and its authentication stay (wire.md Â§8).
func TestServiceStreamsSurvivePathDeath(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	aLocal.Features = []uint32{uint32(frame.FeaturePathMigration)}
	bLocal.Features = aLocal.Features

	first := memcarrier.NewQUIC()
	est, srv, binder := establishWithBinder(t, first, verifyCard(t, bc.peer, now),
		aLocal, bLocal, session.AcceptConfig{})
	second := memcarrier.NewQUIC()
	bindSecondPath(t, est, binder, second, mux.PathDirect)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc, err := est.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteFrame(0x40, 0, []byte("before")); err != nil {
		t.Fatal(err)
	}
	rs, err := srv.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.ReadFrame(); err != nil {
		t.Fatal(err)
	}

	first.Drop()
	if _, err := svc.ReadFrame(); !errors.Is(err, mux.ErrPathFailed) {
		t.Fatalf("service stream on the dead path: %v, want path-failed", err)
	}
	// The service reopens on the surviving path; same session, same keys.
	reopened, err := est.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.PathID() != 2 {
		t.Fatalf("reopened on path %d, want 2", reopened.PathID())
	}
	if err := reopened.WriteFrame(0x40, 0, []byte("after")); err != nil {
		t.Fatal(err)
	}
	rs2, err := srv.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := rs2.ReadFrame(); err != nil || !bytes.Contains(f.Payload, []byte("after")) {
		t.Fatalf("after reopen: %+v %v", f, err)
	}
	select {
	case <-est.Session.Done():
		t.Fatal("the session ended with its first path")
	default:
	}
}

// TestSessionWithoutPathsLivesTheGraceTime pins wire.md Â§8/Â§10: after the last path dies the
// session waits for a PATH_BIND (60 s in the specification; shortened here) and only then ends.
func TestSessionWithoutPathsLivesTheGraceTime(t *testing.T) {
	if got := mux.DefaultTimers().NoPathGrace; got != 60*time.Second {
		t.Fatalf("default no-path grace %v, want 60 s (wire.md §10)", got)
	}
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	grace := 300 * time.Millisecond
	timers := mux.Timers{Tick: 5 * time.Millisecond, NoPathGrace: grace}
	pair := memcarrier.NewQUIC()
	est, _, _ := establishWithBinder(t, pair, verifyCard(t, bc.peer, now), aLocal, bLocal,
		session.AcceptConfig{}, session.WithTimers(timers))

	pair.Drop()
	waitPath(t, "no paths", func() bool {
		ids, _, _ := est.Paths()
		return len(ids) == 0
	})
	select {
	case <-est.Session.Done():
		t.Fatal("the session ended before the no-path grace time")
	case <-time.After(grace / 2):
	}
	select {
	case <-est.Session.Done():
		if !errors.Is(est.Session.Err(), mux.ErrNoPath) {
			t.Fatalf("session error %v, want ErrNoPath", est.Session.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session did not end after the no-path grace time")
	}
}

// TestIdlePaddingWhenNegotiated pins wire.md Â§6.4/Â§9 and registries.md Â§5: with the feature
// agreed, an idle sender writes PADDING frames at the gap drawn from shape.Sender.IdlePad.
func TestIdlePaddingWhenNegotiated(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	aLocal.Features = []uint32{uint32(frame.FeatureIdlePadding)}
	bLocal.Features = aLocal.Features

	profile := &shape.Profile{IdlePad: &shape.Range{Min: 5, Max: 5}}
	timers := mux.Timers{Tick: 2 * time.Millisecond, KeepaliveMin: 30 * time.Second,
		KeepaliveMax: 30 * time.Second, PathDead: 30 * time.Second}
	pair := memcarrier.NewQUIC()
	tap := &tapCarrier{Carrier: pair.Client}
	est, _, _ := establishWithBinder(t, pair, verifyCard(t, bc.peer, now), aLocal, bLocal,
		session.AcceptConfig{IdlePadding: profile, Timers: timers},
		session.WithCarrier(tap), session.WithTimers(timers),
		session.WithIdlePadding(profile, nil))

	if !hasFeature(est, frame.FeatureIdlePadding) {
		t.Fatal("idle-padding was not negotiated")
	}
	waitPath(t, "idle PADDING", func() bool { return paddingFrames(tap.Bytes()) > 0 })
}

// TestNoIdlePaddingUnderLowPower pins platforms.md Â§2: a low_power platform does not offer the
// idle-padding feature, so it is never negotiated and no PADDING-only frames are sent.
func TestNoIdlePaddingUnderLowPower(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	aLocal.Features = []uint32{uint32(frame.FeatureIdlePadding)}
	bLocal.Features = aLocal.Features

	profile := &shape.Profile{IdlePad: &shape.Range{Min: 1, Max: 1}}
	timers := mux.Timers{Tick: 2 * time.Millisecond, KeepaliveMin: 30 * time.Second,
		KeepaliveMax: 30 * time.Second, PathDead: 30 * time.Second}
	pair := memcarrier.NewQUIC()
	tap := &tapCarrier{Carrier: pair.Client}
	est, _, _ := establishWithBinder(t, pair, verifyCard(t, bc.peer, now), aLocal, bLocal,
		session.AcceptConfig{IdlePadding: profile, Timers: timers, LowPower: true},
		session.WithCarrier(tap), session.WithTimers(timers),
		session.WithIdlePadding(profile, nil), session.WithLowPower(true))

	if hasFeature(est, frame.FeatureIdlePadding) {
		t.Fatal("a low_power node offered idle-padding")
	}
	if got := session.FeaturesFor([]uint32{uint32(frame.FeatureIdlePadding), 0x0001}, true); len(got) != 1 || got[0] != 0x0001 {
		t.Fatalf("FeaturesFor low_power = %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if n := paddingFrames(tap.Bytes()); n != 0 {
		t.Fatalf("%d PADDING frames under low_power", n)
	}
}

// fakeClock is a manually advanced carrier.Clock for the path policy.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) AfterFunc(time.Duration, func()) carrier.Timer { return fakeTimer{} }

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeTimer struct{}

func (fakeTimer) Stop() bool { return true }

// TestPathPolicyPairLimitAndBackoff pins H11 and H15 (decisions.md, 2026-11-05): at most two
// client carriers to one (address, SNI), and an exponential backoff after a penalty signal.
func TestPathPolicyPairLimitAndBackoff(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := session.NewPathPolicy(session.PathPolicyConfig{Clock: clk})
	pair := session.Pair{Host: "203.0.113.10", SNI: "example.com"}
	other := session.Pair{Host: "203.0.113.10", SNI: "other.example"}

	if err := p.Acquire(pair); err != nil {
		t.Fatal(err)
	}
	if err := p.Acquire(pair); err != nil {
		t.Fatal(err)
	}
	var limit *session.PathLimitError
	if err := p.Acquire(pair); !errors.As(err, &limit) || limit.Limit != 2 {
		t.Fatalf("third carrier to one pair: %v, want PathLimitError{2}", err)
	}
	if err := p.Acquire(other); err != nil {
		t.Fatalf("another SNI is another pair: %v", err)
	}
	p.Release(pair)
	p.Release(pair)
	if err := p.Acquire(pair); err != nil {
		t.Fatalf("released slot: %v", err)
	}

	// A penalty releases the slot and starts the backoff at about 120 s.
	p.Penalize(pair)
	var backoff *session.PathBackoffError
	if err := p.Acquire(pair); !errors.As(err, &backoff) {
		t.Fatalf("during backoff: %v", err)
	}
	clk.advance(119 * time.Second)
	if err := p.Acquire(pair); !errors.As(err, &backoff) {
		t.Fatalf("119 s after the penalty: %v", err)
	}
	clk.advance(2 * time.Second)
	if err := p.Acquire(pair); err != nil {
		t.Fatalf("after the backoff: %v", err)
	}
	// A second penalty doubles the window (240 s).
	p.Penalize(pair)
	clk.advance(121 * time.Second)
	if err := p.Acquire(pair); !errors.As(err, &backoff) {
		t.Fatalf("121 s into the doubled backoff: %v", err)
	}
	clk.advance(120 * time.Second)
	if err := p.Acquire(pair); err != nil {
		t.Fatalf("after the doubled backoff: %v", err)
	}
	// A successful carrier resets the failure count.
	p.Succeeded(pair)
	p.Penalize(pair)
	clk.advance(119 * time.Second)
	if err := p.Acquire(pair); !errors.As(err, &backoff) {
		t.Fatalf("the failure count did not reset: %v", err)
	}
}
