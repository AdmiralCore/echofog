package mux

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Path liveness and switching (wire.md §7.1, §8.3, §10).

func fastPair(t *testing.T, mod func(a, b *Config)) *pair {
	return sessionPair(t, false, func(a, b *Config) {
		a.Timers, b.Timers = fastTimers(), fastTimers()
		if mod != nil {
			mod(a, b)
		}
	})
}

func pathIDs(s *Session) []uint64 {
	ids, _, _ := s.Paths()
	slices.Sort(ids)
	return ids
}

func activeOf(s *Session) uint64 { _, a, _ := s.Paths(); return a }

func isFailing(s *Session, id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.paths[id]
	return p != nil && p.failing
}

func TestStandbyPathKeptAliveByPing(t *testing.T) {
	pr := fastPair(t, nil)
	bindNewPath(t, pr, false)
	// Idle for several path-dead times: both paths ping and stay alive; idleness never abandons.
	time.Sleep(4 * fastTimers().PathDead)
	if got := pathIDs(pr.a); !slices.Equal(got, []uint64{0, 2}) {
		t.Fatalf("paths %v, want the active and the standby path", got)
	}
	if got := pathIDs(pr.b); !slices.Equal(got, []uint64{0, 2}) {
		t.Fatalf("peer paths %v", got)
	}
}

func TestActiveFailingOnMissingPongSwapsToStandby(t *testing.T) {
	pr := fastPair(t, nil)
	bindNewPath(t, pr, false)
	if activeOf(pr.a) != 0 {
		t.Fatal("path 0 should be active")
	}
	// Path 0 goes silent from B to A: A's PINGs on it get no PONG.
	pr.path0.tb.frozen.Store(true)
	waitFor(t, func() bool { return activeOf(pr.a) == 2 })
	// The failing path stays a standby path (not dead yet).
	if !slices.Contains(pathIDs(pr.a), 0) || !isFailing(pr.a, 0) {
		t.Fatalf("paths %v: path 0 should still be a failing standby", pathIDs(pr.a))
	}
	// New streams go to the new active path.
	st, err := pr.a.OpenStream(ctxT(t), 3, 2)
	if err != nil || st.PathID() != 2 {
		t.Fatalf("stream on path %d: %v", st.PathID(), err)
	}
	// It recovers when traffic flows again, and stays a standby path.
	pr.path0.tb.frozen.Store(false)
	waitFor(t, func() bool { return !isFailing(pr.a, 0) })
	time.Sleep(2 * fastTimers().PathDead)
	if !slices.Contains(pathIDs(pr.a), 0) || activeOf(pr.a) != 2 {
		t.Fatalf("paths %v active %d", pathIDs(pr.a), activeOf(pr.a))
	}
}

func TestFailingStandbyDiesAfterPathDead(t *testing.T) {
	pr := fastPair(t, nil)
	bindNewPath(t, pr, false)
	pr.path0.tb.frozen.Store(true)
	waitFor(t, func() bool { return activeOf(pr.a) == 2 })
	waitFor(t, func() bool { return slices.Equal(pathIDs(pr.a), []uint64{2}) })
	// The peer learns it through PATH_ABANDON on the live path.
	waitFor(t, func() bool { return slices.Equal(pathIDs(pr.b), []uint64{2}) })
}

func TestActiveFailingOnSilenceWithDataOutstanding(t *testing.T) {
	// Keepalives far apart: only outstanding data can reveal the dead path.
	pr := fastPair(t, func(a, b *Config) {
		for _, c := range []*Config{a, b} {
			c.Timers.KeepaliveMin, c.Timers.KeepaliveMax = 30*time.Second, 30*time.Second
			c.Timers.PathDead = 30 * time.Second
		}
	})
	bindNewPath(t, pr, false)
	st, _ := pr.a.OpenStream(ctxT(t), 3, 2)
	// A round trip after the OPEN: nothing is outstanding when the path goes silent.
	if _, err := pr.a.Ping(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	pr.path0.tb.frozen.Store(true)
	time.Sleep(fastTimers().FailSilence * 2)
	if activeOf(pr.a) != 0 {
		t.Fatal("an idle path without outstanding data was switched")
	}
	st.WriteFrame(0x40, 0, deliver(1, "nobody answers"))
	start := time.Now()
	waitFor(t, func() bool { return activeOf(pr.a) == 2 })
	if d := time.Since(start); d > 3*fastTimers().FailSilence {
		t.Fatalf("switched after %v", d)
	}
}

func TestStandbyLimit(t *testing.T) {
	// Spec-default liveness timers: binding trims standby paths synchronously. With the fast
	// timers used by the liveness tests above, a loaded scheduler can declare the active path
	// failing and make another standby active; the limit then drops that path (wire.md §8.3
	// keeps the oldest *standby*), so the set of IDs is no longer fixed. The limit itself is
	// what this test checks, and it is checked here without timing interference.
	pr := sessionPair(t, false, nil)
	for i := 0; i < 3; i++ {
		bindNewPath(t, pr, false)
	}
	// Active path 0 plus at most 2 standby paths: the oldest standby (2) was abandoned.
	waitFor(t, func() bool { return slices.Equal(pathIDs(pr.a), []uint64{0, 4, 6}) })
	waitFor(t, func() bool { return slices.Equal(pathIDs(pr.b), []uint64{0, 4, 6}) })

	// low_power: no standby paths at all.
	lp := sessionPair(t, false, func(a, b *Config) { a.LowPower = true })
	if lp.a.cfg.Timers.KeepaliveMin != 40*time.Second || lp.a.cfg.Timers.KeepaliveMax != 55*time.Second {
		t.Fatalf("low_power keepalive %v..%v", lp.a.cfg.Timers.KeepaliveMin, lp.a.cfg.Timers.KeepaliveMax)
	}
	bindNewPath(t, lp, false)
	waitFor(t, func() bool { return slices.Equal(pathIDs(lp.a), []uint64{0}) })
}

func TestDirectPathPreferredOverRelay(t *testing.T) {
	pr := sessionPair(t, false, nil)
	// Rebuild the picture: path 0 is direct; a relay path bound later does not take over.
	e := newCarrier(t, false)
	ch := make(chan error, 1)
	go func() { _, err := pr.a.BindPath(ctxT(t), e.ga, PathRelay); ch <- err }()
	if _, _, err := pr.binder.Accept(ctxT(t), e.gb, PathRelay); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	if activeOf(pr.a) != 0 {
		t.Fatal("relay path replaced the direct path")
	}
}

// Sealed mode on a byte-stream carrier: after the handshake, the whole carrier is one pipe of
// records (wire.md §8.5).
func TestSealedCarrierIsOnePipeOfRecords(t *testing.T) {
	pr := sessionPair(t, true, nil)
	ctx := ctxT(t)
	st, _ := pr.a.OpenStream(ctx, 3, 2)
	st.WriteFrame(0x40, 0, deliver(1, "SECRET"))
	rs, _ := pr.b.AcceptStream(ctx)
	if f, err := rs.ReadFrame(); err != nil || !bytes.Contains(f.Payload, []byte("SECRET")) {
		t.Fatal(err)
	}
	pr.a.Ping(ctx)
	wire := pr.path0.ta.Bytes()
	// Plain prefix: the announcement of stream 0 and the HS_HELLO frame in MUX_STREAM.
	hello := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}})
	var prefix []byte
	prefix = frame.AppendFrame(prefix, frame.Frame{Type: frame.TypeMuxStream, Payload: frame.MuxStream{StreamID: 0}.Append(nil)})
	prefix = frame.AppendFrame(prefix, frame.Frame{Type: frame.TypeMuxStream, Payload: frame.MuxStream{StreamID: 0, Data: hello}.Append(nil)})
	if !bytes.HasPrefix(wire, prefix) {
		t.Fatalf("plain prefix %x", wire[:min(len(wire), len(prefix))])
	}
	// The rest is kind-1 records of path 0, pipe 0, with growing counters; their plaintext is
	// connection-level mux frames.
	_, rk := vectorKeys(t)
	rcv, _ := NewSealer(rk)
	var ps PipeState
	var plain []byte
	rest := bytes.NewReader(wire[len(prefix):])
	nrec := 0
	for rest.Len() > 0 {
		kind, body, err := readRecord(rest, nil)
		if err != nil || kind != RecordSealed {
			t.Fatalf("record %d: kind %d %v", nrec, kind, err)
		}
		pt, err := rcv.OpenRecord(nil, body, 0, 0, &ps)
		if err != nil {
			t.Fatalf("record %d: %v", nrec, err)
		}
		plain = append(plain, pt...)
		nrec++
	}
	if nrec == 0 || bytes.Contains(wire, []byte("SECRET")) {
		t.Fatalf("%d records; secret visible: %v", nrec, bytes.Contains(wire, []byte("SECRET")))
	}
	for len(plain) > 0 {
		f, n, err := frame.ParseFrame(plain, nil)
		if err != nil {
			t.Fatalf("record plaintext is not mux frames: %v", err)
		}
		if info, _ := frame.SessionType(f.Type); info.Where&frame.WhereConn == 0 {
			t.Fatalf("frame %s inside the records is not connection level", info.Name)
		}
		plain = plain[n:]
	}
}

func TestSealedCarrierRefusesReplayedRecord(t *testing.T) {
	pr := sessionPair(t, true, nil)
	ctx := ctxT(t)
	pr.a.Ping(ctx)
	wire := pr.path0.ta.Bytes()
	// The last record A sent, sent again by an attacker who can write into the carrier.
	rest := bytes.NewReader(wire[bytes.LastIndex(wire, []byte{0x01, 0, 0, 0, 0, 0, 0, 0})-2:])
	_, body, err := readRecord(rest, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rec bytes.Buffer
	writeRecord(&rec, RecordSealed, body)
	pr.path0.ta.Conn.Write(rec.Bytes())
	waitClosed(t, pr.path0.cb)
	if err := pr.path0.cb.Err(); !errors.Is(err, ErrReplay) {
		t.Fatalf("carrier ended with %v, want ErrReplay", err)
	}
}

// nativeCarrier stands for a carrier with native streams (QUIC): it reports sealed mode but
// cannot seal the whole connection, so each stream is a pipe of its own.
type nativeCarrier struct{ Carrier }

func (nativeCarrier) Sealed() bool { return true }

func TestSealedModePerStreamPipes(t *testing.T) {
	ik, rk := vectorKeys(t)
	a, b := pipeRWTap()
	ca := NewConn(a, ConnConfig{Client: true})
	cb := NewConn(b, ConnConfig{})
	t.Cleanup(func() { ca.Close(); cb.Close() })
	ga, gb := NewGuard(nativeCarrier{ca}), NewGuard(nativeCarrier{cb})
	ctx := ctxT(t)
	raw0, _ := ca.OpenStream(ctx)
	raw0.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}}))
	_, pend, err := NewBinder().Accept(ctx, gb, PathDirect)
	if !errors.Is(err, ErrNewSession) {
		t.Fatal(err)
	}
	sa, _ := NewSession(Config{Keys: ik, Policy: chatPolicy}, ga, raw0, PathDirect)
	sb, _ := NewSession(Config{Keys: rk, Policy: chatPolicy}, pend.Guard, pend.Stream, PathDirect)
	t.Cleanup(func() { sa.Close(); sb.Close() })
	st, _ := sa.OpenStream(ctx, 3, 2)
	st.WriteFrame(0x40, 0, deliver(1, "SECRET"))
	rs, err := sb.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := rs.ReadFrame(); err != nil || !bytes.Contains(f.Payload, []byte("SECRET")) {
		t.Fatal(err)
	}
	if _, err := sa.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(a.Bytes(), []byte("SECRET")) {
		t.Fatal("plaintext on the wire")
	}
}

func pipeRWTap() (*tap, *tap) {
	x, y := pipeRW()
	return &tap{Conn: x}, &tap{Conn: y}
}
