package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// slowVerifier widens the window between message 3 and the attachment of the session on the
// responder, as a loaded node does.
type slowVerifier struct{ d time.Duration }

func (v slowVerifier) VerifyCard(env []byte, now time.Time) (*handshake.CardInfo, error) {
	time.Sleep(v.d)
	return handshake.DefaultCardVerifier{}.VerifyCard(env, now)
}

// T3.11: an initiator may open a service stream the moment its Dial returns, without any PING
// barrier; the responder still finishing message 3 must not refuse it.
func TestStreamOpenedRightAfterDialReachesTheResponder(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 7, uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	pair := memcarrier.NewQUIC()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type res struct {
		est *session.Established
		err error
	}
	accepted := make(chan res, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
			Local:    srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false),
			Verifier: slowVerifier{d: 400 * time.Millisecond},
		})
		accepted <- res{est, err}
	}()
	cli, err := session.Dial(ctx, remote, dialAttempt(),
		cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false), session.WithCarrier(pair.Client))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Session.Close() })
	st, err := cli.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	_ = st.WriteFrame(0x40, 0, []byte{0x08, 1})

	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	t.Cleanup(func() { _ = r.est.Session.Close() })
	rs, err := r.est.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("the responder lost the stream opened right after message 3: %v", err)
	}
	if o := rs.Open(); o.Service != 3 || o.Version != 2 {
		t.Fatalf("stream open %+v", o)
	}
}
