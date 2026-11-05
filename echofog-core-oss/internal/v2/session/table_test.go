package session_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// cardPushFeature offers the CARD_UPDATE feature (objects.md §3.6) in these tests.
var cardPushFeature = []uint32{uint32(frame.FeatureCardPush)}

func localWithFeatures(n *testNode, nc nodeCards) session.LocalIdentity {
	l := n.local(nc.peer, nc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	l.Features = cardPushFeature
	return l
}

// establishWith is establish with Dial options and AcceptConfig fields for CARD_UPDATE and timers.
// cfg.Local is the accepting identity.
func establishWith(t *testing.T, pair *memcarrier.Pair, card *objects.Card, cli session.LocalIdentity,
	cfg session.AcceptConfig, opts ...session.DialOption) (*session.Established, *session.Established) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	dialed := make(chan runResult, 1)
	dOpts := append([]session.DialOption{session.WithCarrier(pair.Client)}, opts...)
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
	t.Cleanup(func() {
		_ = srvEst.Session.Close()
		_ = r.est.Session.Close()
	})
	return r.est, srvEst
}

func waitDone(t *testing.T, s *mux.Session, within time.Duration) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(within):
		t.Fatalf("session did not close within %v", within)
	}
}

func isClosed(s *mux.Session) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

func TestMaxSessionsForProfile(t *testing.T) {
	if got := session.MaxSessionsFor(128); got != 32 {
		t.Fatalf("mobile limit %d, want 32 (platforms.md §2)", got)
	}
	if got := session.MaxSessionsFor(40); got != 32 {
		t.Fatalf("iOS extension limit %d, want 32", got)
	}
	if got := session.MaxSessionsFor(0); got <= 32 {
		t.Fatalf("desktop limit %d, want more than the mobile 32", got)
	}
}

// TestTableGetCoalesces proves one Get per key while the dial is in flight.
func TestTableGetCoalesces(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	pair := memcarrier.NewQUIC()
	est, _ := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal, session.AcceptConfig{Local: bLocal})

	tbl, err := session.NewTable(session.TableConfig{MaxSessions: 8})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var opens atomic.Int32
	open := func(context.Context, ...session.DialOption) (*session.Established, error) {
		opens.Add(1)
		time.Sleep(25 * time.Millisecond) // keep the flight open while the others arrive
		return est, nil
	}

	const n = 8
	got := make([]*session.Established, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = tbl.Get(ctx, aLocal, b.device.ID(), open)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := opens.Load(); got != 1 {
		t.Fatalf("the dial ran %d times, want 1 (single flight)", got)
	}
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("Get %d: %v", i, errs[i])
		}
		if got[i] != est {
			t.Fatalf("Get %d returned a different session", i)
		}
	}
	if tbl.Len() != 1 {
		t.Fatalf("table holds %d sessions, want 1", tbl.Len())
	}
}

// TestTableSimultaneousDialKeepsOneSession starts a dial and an accept from both sides at
// once: exactly one session must survive on each side, and both sides must keep the session
// whose initiator has the smaller device ID (decisions.md T3.2).
func TestTableSimultaneousDialKeepsOneSession(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aCard := verifyCard(t, ac.peer, now)
	bCard := verifyCard(t, bc.peer, now)
	aLocal := localWithFeatures(a, ac)
	bLocal := localWithFeatures(b, bc)
	aID, bID := a.device.ID(), b.device.ID()

	tA := newTable(t)
	tB := newTable(t)

	pairAB := memcarrier.NewQUIC() // A dials, B accepts
	pairBA := memcarrier.NewQUIC() // B dials, A accepts
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	type side struct {
		kept, dialed, accepted *session.Established
		err                    error
	}

	acceptB := make(chan side, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pairAB.Server}, session.AcceptConfig{Local: bLocal})
		if err != nil {
			acceptB <- side{err: err}
			return
		}
		kept, aerr := tB.Add(bLocal, est)
		acceptB <- side{kept: kept, accepted: est, err: aerr}
	}()
	acceptA := make(chan side, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pairBA.Server}, session.AcceptConfig{Local: aLocal})
		if err != nil {
			acceptA <- side{err: err}
			return
		}
		kept, aerr := tA.Add(aLocal, est)
		acceptA <- side{kept: kept, accepted: est, err: aerr}
	}()

	dialA := make(chan side, 1)
	go func() {
		var dialed *session.Established
		open := func(dctx context.Context, opts ...session.DialOption) (*session.Established, error) {
			opts = append(opts, session.WithCarrier(pairAB.Client))
			var err error
			dialed, err = session.Dial(dctx, bCard, dialAttempt(), aLocal, opts...)
			return dialed, err
		}
		kept, err := tA.Get(ctx, aLocal, bID, open)
		dialA <- side{kept: kept, dialed: dialed, err: err}
	}()
	dialB := make(chan side, 1)
	go func() {
		var dialed *session.Established
		open := func(dctx context.Context, opts ...session.DialOption) (*session.Established, error) {
			opts = append(opts, session.WithCarrier(pairBA.Client))
			var err error
			dialed, err = session.Dial(dctx, aCard, dialAttempt(), bLocal, opts...)
			return dialed, err
		}
		kept, err := tB.Get(ctx, bLocal, aID, open)
		dialB <- side{kept: kept, dialed: dialed, err: err}
	}()

	aD, bD := <-dialA, <-dialB
	aA, bA := <-acceptA, <-acceptB
	for _, s := range []side{aD, bD, aA, bA} {
		if s.err != nil {
			t.Fatalf("simultaneous dial: %v", s.err)
		}
	}

	if tA.Len() != 1 || tB.Len() != 1 {
		t.Fatalf("tables hold %d/%d sessions, want 1/1", tA.Len(), tB.Len())
	}

	// The caller of Get may hold a session that Add evicted a moment later; the survivor is
	// what the table returns now.
	keptA, err := tA.Get(ctx, aLocal, bID, func(context.Context, ...session.DialOption) (*session.Established, error) {
		t.Fatal("Get dialed again although a session is live")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Get on A: %v", err)
	}
	keptB, err := tB.Get(ctx, bLocal, aID, func(context.Context, ...session.DialOption) (*session.Established, error) {
		t.Fatal("Get dialed again although a session is live")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Get on B: %v", err)
	}
	if keptA.SID != keptB.SID {
		t.Fatalf("opposite sides kept different sessions: %x vs %x", keptA.SID, keptB.SID)
	}

	wantA, wantB := session.RoleResponder, session.RoleInitiator
	if bytes.Compare(aID[:], bID[:]) < 0 {
		wantA, wantB = session.RoleInitiator, session.RoleResponder
	}
	if keptA.Role != wantA || keptB.Role != wantB {
		t.Fatalf("kept roles %v/%v, want %v/%v (smaller initiator wins)", keptA.Role, keptB.Role, wantA, wantB)
	}

	// Exactly one of the two sessions on each side is alive, and both sides agree on which.
	alive := func(x, y *session.Established) *session.Established {
		if x != nil && !isClosed(x.Session) {
			return x
		}
		if y != nil && !isClosed(y.Session) {
			return y
		}
		return nil
	}
	var aliveA, aliveB *session.Established
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		aliveA, aliveB = alive(aD.dialed, aA.accepted), alive(bD.dialed, bA.accepted)
		if aliveA != nil && aliveB != nil && aliveA.SID == aliveB.SID {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if aliveA == nil || aliveB == nil || aliveA.SID != aliveB.SID {
		t.Fatalf("the surviving sessions disagree: %v %v", aliveA, aliveB)
	}

	// The loser left with GOAWAY{no-error} (wire.md §7.3).
	loserA := aD.dialed
	if loserA == aliveA {
		loserA = aA.accepted
	}
	waitDone(t, loserA.Session, 5*time.Second)
	var ga *mux.GoAwayError
	if err := loserA.Session.Err(); !errors.As(err, &ga) || ga.Code != frame.CodeNoError {
		t.Fatalf("loser ended with %v, want GOAWAY{no-error}", err)
	}
}

// TestTableClosesIdleSessions closes a session with no streams and no traffic after the
// profile's idle time.
func TestTableClosesIdleSessions(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	pair := memcarrier.NewQUIC()
	est, _ := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal, session.AcceptConfig{Local: bLocal})

	tbl, err := session.NewTable(session.TableConfig{
		MaxSessions:   8,
		IdleTimeout:   100 * time.Millisecond,
		SweepInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	if _, err := tbl.Add(aLocal, est); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitDone(t, est.Session, 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for tbl.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tbl.Len() != 0 {
		t.Fatalf("idle session stayed in the table")
	}
}

// TestGoAwayWaitsForBothSidesStreams: after GOAWAY the session closes only when the streams of
// both sides have ended; retry_after reaches the peer.
func TestGoAwayWaitsForBothSidesStreams(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	timers := mux.Timers{Tick: 10 * time.Millisecond, GoAwayDrain: 3 * time.Second}
	pair := memcarrier.NewQUIC()
	estA, estB := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal,
		session.AcceptConfig{Local: bLocal, Timers: timers}, session.WithTimers(timers))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stXA, err := estA.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("A OpenStream: %v", err)
	}
	stXB, err := estB.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("B AcceptStream: %v", err)
	}
	stYB, err := estB.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("B OpenStream: %v", err)
	}
	stYA, err := estA.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("A AcceptStream: %v", err)
	}

	go func() { _ = estA.GoAway(context.Background(), frame.CodeGoingAway, "restart", 30) }()
	time.Sleep(150 * time.Millisecond)
	if isClosed(estA.Session) {
		t.Fatal("A closed with streams of both sides still open")
	}

	// End A's stream. B's stream (the peer side) is still open, so the session must stay.
	_ = stXA.Close()
	_ = stXB.Close()
	time.Sleep(150 * time.Millisecond)
	if isClosed(estA.Session) {
		t.Fatal("A closed although the peer's stream is still open")
	}

	// End B's stream too; now both sides have no streams and the session drains.
	_ = stYA.Close()
	_ = stYB.Close()
	waitDone(t, estA.Session, 3*time.Second)
	waitDone(t, estB.Session, 3*time.Second)

	var ga *mux.GoAwayError
	if err := estB.Session.Err(); !errors.As(err, &ga) || ga.Code != frame.CodeGoingAway || ga.RetryAfter != 30 {
		t.Fatalf("peer saw %v, want GOAWAY{going-away, retry_after=30}", err)
	}
}

// TestGoAwayDrainDeadline: a stream that never ends cannot hold the session past the 15 s
// equivalent drain time (the test injects a shorter one).
func TestGoAwayDrainDeadline(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false)

	timers := mux.Timers{Tick: 10 * time.Millisecond, GoAwayDrain: 300 * time.Millisecond}
	pair := memcarrier.NewQUIC()
	estA, estB := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal,
		session.AcceptConfig{Local: bLocal, Timers: timers}, session.WithTimers(timers))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := estA.Session.OpenStream(ctx, 3, 2); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := estB.Session.AcceptStream(ctx); err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}

	start := time.Now()
	go func() { _ = estA.GoAway(context.Background(), frame.CodeGoingAway, "", 0) }()
	waitDone(t, estA.Session, 5*time.Second)
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("closed after %v, before the drain deadline", d)
	}
}

func newTable(t *testing.T) *session.Table {
	t.Helper()
	tbl, err := session.NewTable(session.TableConfig{MaxSessions: 8})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	return tbl
}

// TestCardUpdateAppliesNewerCardAndIgnoresOldSeq receives CARD_UPDATE into the peer's card
// book: a higher seq replaces the stored card, an older seq is ignored (objects.md §3.6).
func TestCardUpdateAppliesNewerCardAndIgnoresOldSeq(t *testing.T) {
	book, err := session.OpenCardBook(newMemKV())
	if err != nil {
		t.Fatalf("OpenCardBook: %v", err)
	}
	var results []objects.AddResult
	var resultsMu sync.Mutex
	ctl := session.NewControl(session.ControlConfig{
		Cards: book,
		OnCard: func(_ *objects.Card, res objects.AddResult, _ error) {
			resultsMu.Lock()
			results = append(results, res)
			resultsMu.Unlock()
		},
	})

	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := localWithFeatures(a, ac)
	bLocal := localWithFeatures(b, bc)

	pair := memcarrier.NewQUIC()
	_, estB := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal,
		session.AcceptConfig{Local: bLocal}, session.WithControl(ctl))

	notAfter := uint64(now.Add(24 * time.Hour).Unix())
	suites := []uint16{handshake.SuiteXK, handshake.SuiteXX}
	newer := b.card(t, objects.ScopePeer, suites, 20, notAfter)
	older := b.card(t, objects.ScopePeer, suites, 15, notAfter)

	if err := estB.PushCard(newer); err != nil {
		t.Fatalf("PushCard newer: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := book.Get(b.device.ID(), objects.ScopePeer, nil)
		return c != nil && c.Env.Seq == 20
	})

	if err := estB.PushCard(older); err != nil {
		t.Fatalf("PushCard older: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		resultsMu.Lock()
		defer resultsMu.Unlock()
		return len(results) >= 2
	})
	resultsMu.Lock()
	got := append([]objects.AddResult(nil), results...)
	resultsMu.Unlock()
	if got[1] != objects.CardIgnored {
		t.Fatalf("old seq result %v, want CardIgnored", got[1])
	}
	if c := book.Get(b.device.ID(), objects.ScopePeer, nil); c == nil || c.Env.Seq != 20 {
		t.Fatalf("old seq replaced the card: %+v", c)
	}
}

// TestCardUpdateEquivocation: two different cards of the same device with the same seq are
// flagged and the signer is marked misbehaving.
func TestCardUpdateEquivocation(t *testing.T) {
	book, err := session.OpenCardBook(newMemKV())
	if err != nil {
		t.Fatalf("OpenCardBook: %v", err)
	}
	var equivocations atomic.Int32
	ctl := session.NewControl(session.ControlConfig{
		Cards: book,
		OnCard: func(_ *objects.Card, _ objects.AddResult, err error) {
			if errors.Is(err, objects.ErrEquivocation) {
				equivocations.Add(1)
			}
		},
	})

	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := localWithFeatures(a, ac)
	bLocal := localWithFeatures(b, bc)

	pair := memcarrier.NewQUIC()
	_, estB := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal,
		session.AcceptConfig{Local: bLocal}, session.WithControl(ctl))

	suites := []uint16{handshake.SuiteXK, handshake.SuiteXX}
	first := b.card(t, objects.ScopePeer, suites, 30, uint64(now.Add(24*time.Hour).Unix()))
	second := b.card(t, objects.ScopePeer, suites, 30, uint64(now.Add(48*time.Hour).Unix()))

	if err := estB.PushCard(first); err != nil {
		t.Fatalf("PushCard first: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := book.Get(b.device.ID(), objects.ScopePeer, nil)
		return c != nil && c.Env.Seq == 30
	})
	if err := estB.PushCard(second); err != nil {
		t.Fatalf("PushCard second: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return equivocations.Load() == 1 })
	if !book.Misbehaving(b.device.ID()) {
		t.Fatal("equivocating device is not marked misbehaving")
	}
}

// TestTablePushCardOnlyReachesCardPushPeers: the new card goes to sessions that negotiated
// card-push and to nobody else.
func TestTablePushCardOnlyReachesCardPushPeers(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := localWithFeatures(a, ac)
	bLocal := localWithFeatures(b, bc)

	book, err := session.OpenCardBook(newMemKV())
	if err != nil {
		t.Fatalf("OpenCardBook: %v", err)
	}
	ctl := session.NewControl(session.ControlConfig{Cards: book})

	pair := memcarrier.NewQUIC()
	estA, _ := establishWith(t, pair, verifyCard(t, bc.peer, now), aLocal,
		session.AcceptConfig{Local: bLocal, Control: ctl})

	tbl := newTable(t)
	if _, err := tbl.Add(aLocal, estA); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A pushes its own new card; B is the receiver with the card book.
	notAfter := uint64(now.Add(24 * time.Hour).Unix())
	newer := a.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK, handshake.SuiteXX}, 40, notAfter)
	sent, err := tbl.PushCard(a.device.ID(), newer)
	if err != nil {
		t.Fatalf("PushCard: %v", err)
	}
	if sent != 1 {
		t.Fatalf("PushCard sent to %d peers, want 1", sent)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := book.Get(a.device.ID(), objects.ScopePeer, nil)
		return c != nil && c.Env.Seq == 40
	})
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not reached within %v", within)
}
