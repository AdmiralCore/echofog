package session_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

func netipAddrOf(_ netip.Addr, i int) netip.Addr {
	return netip.AddrFrom4([4]byte{10 + byte(i>>16), byte(i >> 8), byte(i), 1})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// acceptRig runs one Dial against one Accept configured by cfg.
type acceptRig struct {
	cli, srv *session.Established
	srvErr   error
	cliErr   error
	cliNode  *testNode
}

func runAccept(t *testing.T, cfg session.AcceptConfig, hidden bool) *acceptRig {
	t.Helper()
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 7, uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	cfg.Local = srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false)
	cfg.Hidden = hidden
	if hidden {
		cfg.Rand = zeroReader{}
	}
	pair := memcarrier.NewQUIC()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, remote, dialAttempt(),
			cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false), session.WithCarrier(pair.Client))
		dialed <- runResult{est, err}
	}()
	srv, srvErr := session.Accept(ctx, &oneListener{c: pair.Server}, cfg)
	if srvErr != nil {
		_ = pair.Server.Close()
	}
	r := <-dialed
	rig := &acceptRig{cli: r.est, srv: srv, srvErr: srvErr, cliErr: r.err, cliNode: cliNode}
	t.Cleanup(func() {
		if rig.srv != nil {
			_ = rig.srv.Session.Close()
		}
		if rig.cli != nil {
			_ = rig.cli.Session.Close()
		}
	})
	return rig
}

// A public node over a per-source limit answers HS_RETRY with a harder puzzle, and the handshake
// still completes once the initiator solves it. The slot is released afterwards.
func TestAcceptRaisesPuzzleForASourceOverTheLimit(t *testing.T) {
	clk := &obsClock{t: time.Now()}
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	held := holdN(l, session.MaxPreAuthPerIP, func(int) netip.Addr { return ip }, false)
	defer releaseAll(held)
	rig := runAccept(t, session.AcceptConfig{Limits: l, Source: ip}, false)
	if rig.srvErr != nil || rig.cliErr != nil {
		t.Fatalf("Accept: %v, Dial: %v", rig.srvErr, rig.cliErr)
	}
	if got := l.Stats().Retries; got != 1 {
		t.Fatalf("HS_RETRY sent %d times, want 1", got)
	}
	if got := l.InFlight(); got != session.MaxPreAuthPerIP {
		t.Fatalf("in flight = %d, want %d: Accept leaked its slot", got, session.MaxPreAuthPerIP)
	}
}

func TestAcceptWithoutLoadSendsNoPuzzle(t *testing.T) {
	clk := &obsClock{t: time.Now()}
	l := newLimits(t, clk, nil)
	rig := runAccept(t, session.AcceptConfig{Limits: l, Source: addr("198.51.100.7")}, false)
	if rig.srvErr != nil || rig.cliErr != nil {
		t.Fatalf("Accept: %v, Dial: %v", rig.srvErr, rig.cliErr)
	}
	if got := l.Stats().Retries; got != 0 {
		t.Fatalf("HS_RETRY sent %d times without load", got)
	}
	if got := l.InFlight(); got != 0 {
		t.Fatalf("in flight = %d after Accept, want 0", got)
	}
}

// A hidden node over a limit closes silently after the uniform delay and never answers.
func TestAcceptHiddenOverLimitClosesSilently(t *testing.T) {
	clk := &obsClock{t: time.Now()}
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	held := holdN(l, session.MaxPreAuthPerIP, func(int) netip.Addr { return ip }, true)
	defer releaseAll(held)
	start := time.Now()
	rig := runAccept(t, session.AcceptConfig{Limits: l, Source: ip}, true)
	if !errors.Is(rig.srvErr, session.ErrSilent) {
		t.Fatalf("Accept: %v, want ErrSilent", rig.srvErr)
	}
	if rig.cliErr == nil {
		t.Fatal("the initiator was served by a hidden node over its limit")
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("closed after %v: the uniform delay of handshake §5 was skipped", d)
	}
	if l.InFlight() != session.MaxPreAuthPerIP {
		t.Fatalf("in flight = %d: Accept leaked its slot", l.InFlight())
	}
}

// Only an authenticated device is banned; the ban is applied once the handshake names it.
func TestAcceptClosesABannedDevice(t *testing.T) {
	clk := &obsClock{t: time.Now()}
	log, err := session.NewAbuseLog(newMemKV())
	if err != nil {
		t.Fatal(err)
	}
	l := newLimits(t, clk, func(c *session.LimitsConfig) { c.Abuse = log; c.BanAfter = 1; c.BanFor = time.Hour })

	// First session: a protocol violation on the control stream (PING with a bad length).
	first := runAccept(t, session.AcceptConfig{Limits: l, Source: addr("198.51.100.7")}, false)
	if first.srvErr != nil || first.cliErr != nil {
		t.Fatalf("setup: %v / %v", first.srvErr, first.cliErr)
	}
	if err := first.cli.Session.SendControl(frame.TypePing, 0, []byte{1}); err != nil {
		t.Fatal(err)
	}
	peer := first.cliNode.device.ID()
	deadline := time.Now().Add(5 * time.Second)
	for !l.Banned(peer) {
		if time.Now().After(deadline) {
			t.Fatal("a malformed frame of a known type from an authenticated device did not count towards a ban")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l.Banned([32]byte{}) {
		t.Fatal("the unauthenticated zero ID is banned")
	}
}

func TestAcceptRefusesPeerByVersionGate(t *testing.T) {
	rig := runAccept(t, session.AcceptConfig{AdmitVersion: func(peer uint32) bool { return false }}, false)
	var rej *session.ErrRejected
	if !errors.As(rig.cliErr, &rej) || rej.Reason != handshake.ReasonVersion {
		t.Fatalf("Dial: %v, want REJECT version", rig.cliErr)
	}
	if rig.srvErr == nil {
		t.Fatal("the responder completed a handshake with a refused peer")
	}
}

func TestAcceptAdmitsPeerByVersionGate(t *testing.T) {
	var seen uint32
	rig := runAccept(t, session.AcceptConfig{AdmitVersion: func(peer uint32) bool { seen = peer; return true }}, false)
	if rig.srvErr != nil || rig.cliErr != nil {
		t.Fatalf("Accept: %v, Dial: %v", rig.srvErr, rig.cliErr)
	}
	if seen != handshake.ProtoVer {
		t.Fatalf("the gate saw peer version %d, want %d", seen, handshake.ProtoVer)
	}
}

type stubPolicy struct {
	got struct {
		component string
		own, peer uint32
	}
}

func (s *stubPolicy) PeerRefused(component string, own, peer uint32, _ time.Time) bool {
	s.got.component, s.got.own, s.got.peer = component, own, peer
	return peer < own
}

func TestReleaseAdmitAsksThePolicy(t *testing.T) {
	p := &stubPolicy{}
	admit := session.ReleaseAdmit(p, "proto", 200, nil)
	if admit(199) || !admit(200) || !admit(201) {
		t.Fatal("ReleaseAdmit does not follow PeerRefused")
	}
	if p.got.component != "proto" || p.got.own != 200 {
		t.Fatalf("policy asked %+v", p.got)
	}
}
