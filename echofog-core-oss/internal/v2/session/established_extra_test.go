package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// testPolicy is a stream policy that selects chat/2 and nothing else.
type testPolicy struct{}

func (testPolicy) Limiter(service, version uint64) (frame.Limiter, bool) {
	if service != 3 || version != 2 {
		return nil, false
	}
	return func(uint64) (int, bool) { return 0, false }, true
}

// TestEstablishedReportsPathAndPeerTime pins the two facts node.Peer needs from a completed
// session: the path kind of the carrier and the peer's handshake timestamp (T3.8).
func TestEstablishedReportsPathAndPeerTime(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{1, 2}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{1, 2}, false)

	est, srvEst := establish(t, memcarrier.NewQUIC(), verifyCard(t, bc.peer, now), aLocal, bLocal,
		session.AcceptConfig{})

	if est.Kind != mux.PathDirect {
		t.Fatalf("initiator path kind %v, want direct", est.Kind)
	}
	if srvEst.Kind != mux.PathDirect {
		t.Fatalf("responder path kind %v, want direct", srvEst.Kind)
	}
	if est.PeerTime == 0 {
		t.Fatal("initiator has no peer handshake time")
	}
	if srvEst.PeerTime == 0 {
		t.Fatal("responder has no peer handshake time")
	}
}

// TestInjectedPolicySelectsServiceStreams proves the runtime hook of T3.7a: the handshake result
// is filtered through a caller-supplied policy, so the service layer sees Selected()==true and
// the per-type limiter for the pairs it negotiated.
func TestInjectedPolicySelectsServiceStreams(t *testing.T) {
	now := time.Now()
	a, b := newTestNode(t), newTestNode(t)
	ac, bc := a.cards(t, now), b.cards(t, now)
	aLocal := a.local(ac.peer, ac.own, []uint16{1, 2}, false)
	bLocal := b.local(bc.peer, bc.own, []uint16{1, 2}, false)

	policy := func([]handshake.ServiceChoice) mux.StreamPolicy { return testPolicy{} }
	est, _ := establishWith(t, memcarrier.NewQUIC(), verifyCard(t, bc.peer, now), aLocal,
		session.AcceptConfig{Local: bLocal, Policy: policy}, session.WithPolicy(policy))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := est.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if !st.Selected() {
		t.Fatal("selected chat/2 stream reports Selected()==false under the injected policy")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
}
