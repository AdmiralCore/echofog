package session_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// addrCarrier gives a memory carrier a network identity.
type addrCarrier struct {
	mux.Carrier
	local, remote string
}

func (c addrCarrier) LocalAddr() net.Addr {
	return net.UDPAddrFromAddrPort(netip.MustParseAddrPort(c.local))
}
func (c addrCarrier) RemoteAddr() net.Addr {
	return net.UDPAddrFromAddrPort(netip.MustParseAddrPort(c.remote))
}

// observedSession runs one handshake in which the responder sees the initiator at seen and the
// initiator sits at server (its peer address) on socket local.
func observedSession(t *testing.T, ctl *session.Control, kind mux.PathKind, local, server, seen string) *session.Established {
	cli, _ := observedPair(t, ctl, kind, local, server, seen)
	return cli
}

// observedPair is observedSession that also returns the responder side.
func observedPair(t *testing.T, ctl *session.Control, kind mux.PathKind, local, server, seen string) (*session.Established, *session.Established) {
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
	srvC := addrCarrier{Carrier: pair.Server, local: server, remote: seen}
	cliC := addrCarrier{Carrier: pair.Client, local: local, remote: server}

	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, remote, dialAttempt(), cliLocal, session.WithCarrier(cliC),
			session.WithControl(ctl))
		dialed <- runResult{est, err}
	}()
	srvEst, err := session.Accept(ctx, &oneListener{c: srvC}, session.AcceptConfig{Local: srvLocal, Kind: kind})
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

func TestHandshakeObservedFeedsObserver(t *testing.T) {
	r := newObsRig(t)
	ctl := session.NewControl(session.ControlConfig{Observer: r.o})

	est := observedSession(t, ctl, mux.PathDirect, "10.0.0.2:5000", "192.0.2.9:443", "8.8.8.8:4000")
	if got, ok := session.ParseObserved(est.Observed); !ok || got != netip.MustParseAddrPort("8.8.8.8:4000") {
		t.Fatalf("Established.Observed = %x", est.Observed)
	}
	if r.o.Len() != 1 {
		t.Fatalf("reports = %d, want 1", r.o.Len())
	}
	wantPublic(t, r.o, "") // one peer decides nothing

	observedSession(t, ctl, mux.PathDirect, "10.0.0.2:5000", "198.51.100.77:443", "8.8.8.8:4000")
	wantPublic(t, r.o, "8.8.8.8")
}

func TestRelayedPathIsNotObserved(t *testing.T) {
	r := newObsRig(t)
	ctl := session.NewControl(session.ControlConfig{Observer: r.o})
	// The responder accepted over a relay circuit: it sends no address (the carrier's peer is the
	// relay), and the initiator's path kind is relay, so the report is not counted either.
	est := observedSession(t, ctl, mux.PathRelay, "10.0.0.2:5000", "192.0.2.9:443", "8.8.8.8:4000")
	if len(est.Observed) != 0 {
		t.Fatalf("responder over a relay reported %x", est.Observed)
	}
	if r.o.Len() != 0 {
		t.Fatalf("reports = %d, want 0", r.o.Len())
	}
}
