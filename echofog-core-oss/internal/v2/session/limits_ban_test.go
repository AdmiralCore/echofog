package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// T3.5 acceptance F3: a ban follows the node ID (a new device key under the same account does not
// shake it off, handshake §11) and the abuse counters of its devices add up.
func TestBanFollowsTheNodeID(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, func(c *session.LimitsConfig) { c.BanAfter = 4; c.BanWindow = time.Minute; c.BanFor = time.Hour })
	node := [32]byte{9, 9}
	for i := 0; i < 4; i++ { // four different devices of one node
		l.AbuseHandlerFor([32]byte{byte(i + 1)}, node)(mux.Abuse{Type: 1})
	}
	if !l.BannedNode(node) {
		t.Fatal("the node is not banned after its devices added up to the threshold")
	}
	if !l.BannedPeer([32]byte{77}, node) {
		t.Fatal("a fresh device of a banned node is admitted")
	}
	if l.BannedPeer([32]byte{77}, [32]byte{8}) {
		t.Fatal("an unrelated node is banned")
	}
	clk.advance(2 * time.Hour)
	if l.BannedNode(node) {
		t.Fatal("the ban did not expire")
	}
}

// T3.5 acceptance F3: after message 3 a banned device gets REJECT{banned} (handshake §8), not a
// bare close that looks like a network failure.
func TestAcceptSendsRejectBannedAfterMessage3(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	l.BanDevice(cliNode.device.ID(), time.Hour)

	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 7, uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	pair := memcarrier.NewQUIC()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, remote, dialAttempt(),
			cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false), session.WithCarrier(pair.Client))
		dialed <- runResult{est, err}
	}()
	cfg := session.AcceptConfig{Limits: l, Local: srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false)}
	_, srvErr := session.Accept(ctx, &oneListener{c: pair.Server}, cfg)
	if !errors.Is(srvErr, session.ErrBanned) {
		t.Fatalf("Accept: %v, want ErrBanned", srvErr)
	}
	r := <-dialed
	var rej *session.ErrRejected
	if !errors.As(r.err, &rej) || rej.Reason != handshake.ReasonBanned {
		t.Fatalf("Dial: %v, want REJECT{banned}", r.err)
	}
}
