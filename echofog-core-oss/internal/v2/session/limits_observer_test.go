package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// T3.4 inheritance: a peer cannot flood Observe. A token bucket of ObservePeerBurst reports per
// peer, refilled one per second; the excess is ignored without a verdict of suspicion.
func TestObservePerPeerRateLimit(t *testing.T) {
	r := newObsRig(t)
	accepted := 0
	for i := 0; i < 100; i++ {
		if r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s")) == session.ObsAccepted {
			accepted++
		}
	}
	if accepted != session.ObservePeerBurst {
		t.Fatalf("accepted %d reports at one instant, want %d", accepted, session.ObservePeerBurst)
	}
	// Another peer has its own bucket.
	if v := r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s")); v != session.ObsAccepted {
		t.Fatalf("a second peer was limited by the first: %v", v)
	}
	r.clk.advance(1100 * time.Millisecond)
	if v := r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s")); v != session.ObsAccepted {
		t.Fatalf("no refill after a second: %v", v)
	}
	if v := r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s")); v == session.ObsAccepted {
		t.Fatal("refill is faster than one report per second")
	}
}

// bothControl runs a handshake in which the responder has a Control with an Observer, and
// returns the initiator, which may then send control frames to the responder.
func bothControl(t *testing.T, srvCtl *session.Control, kind mux.PathKind, seen string) (*session.Established, *session.Established) {
	t.Helper()
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 3, uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	features := []uint32{uint32(frame.FeatureObservedAddr)}
	cliLocal := cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false)
	cliLocal.Features = features
	srvLocal := srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false)
	srvLocal.Features = features
	pair := memcarrier.NewQUIC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	srvC := addrCarrier{Carrier: pair.Server, local: "192.0.2.9:443", remote: seen}
	cliC := addrCarrier{Carrier: pair.Client, local: "10.0.0.2:5000", remote: "192.0.2.9:443"}
	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, remote, dialAttempt(), cliLocal, session.WithCarrier(cliC))
		dialed <- runResult{est, err}
	}()
	srv, err := session.Accept(ctx, &oneListener{c: srvC}, session.AcceptConfig{Local: srvLocal, Kind: kind, Control: srvCtl})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	r := <-dialed
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	t.Cleanup(func() { _ = srv.Session.Close(); _ = r.est.Session.Close() })
	return r.est, srv
}

func waitObsLen(t *testing.T, o *session.Observer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for o.Len() != want {
		if time.Now().After(deadline) {
			t.Fatalf("observer holds %d reports, want %d", o.Len(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// T3.4 inheritance: OBSERVED_ADDR arriving from an incoming session (responder side) feeds the
// Observer with the peer's authenticated identity, but one peer decides nothing and a reserved
// address is never recorded.
func TestIncomingSessionObservedFrame(t *testing.T) {
	r := newObsRig(t)
	ctl := session.NewControl(session.ControlConfig{Observer: r.o})
	cli, _ := bothControl(t, ctl, mux.PathDirect, "198.51.100.1:4000")

	// a reserved address is dropped
	if err := cli.Session.SendControl(frame.TypeObservedAddr, 0, frame.AppendObservedAddr(nil, ap("10.1.2.3:4000"))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if r.o.Len() != 0 {
		t.Fatalf("a private address was recorded: %d", r.o.Len())
	}

	if err := cli.Session.SendControl(frame.TypeObservedAddr, 0, frame.AppendObservedAddr(nil, ap("8.8.8.8:4000"))); err != nil {
		t.Fatal(err)
	}
	waitObsLen(t, r.o, 1)
	wantPublic(t, r.o, "") // one liar is not a majority
}

// Over a relay circuit the carrier's peer is the relay: the frame says nothing about the peer's
// network and is not recorded.
func TestIncomingRelayedSessionObservedFrameIgnored(t *testing.T) {
	r := newObsRig(t)
	ctl := session.NewControl(session.ControlConfig{Observer: r.o})
	cli, _ := bothControl(t, ctl, mux.PathRelay, "198.51.100.1:4000")
	if err := cli.Session.SendControl(frame.TypeObservedAddr, 0, frame.AppendObservedAddr(nil, ap("8.8.8.8:4000"))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if r.o.Len() != 0 {
		t.Fatalf("a relayed session fed the observer: %d", r.o.Len())
	}
}
