package session_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestXKSessionAndServiceStream runs the full XKhfs handshake and opens a service stream in
// both directions.
func TestXKSessionAndServiceStream(t *testing.T) {
	for _, mode := range []struct {
		name string
		pair *memcarrier.Pair
	}{
		{"quic", memcarrier.NewQUIC()},
		{"bytes", memcarrier.NewBytes()},
	} {
		t.Run(mode.name, func(t *testing.T) {
			now := time.Now()
			cliNode, srvNode := newTestNode(t), newTestNode(t)
			nc := srvNode.cards(t, now)
			cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK, handshake.SuiteXX}, 7,
				uint64(now.Add(24*time.Hour).Unix()))
			remote := verifyCard(t, nc.peer, now)

			cli, srv := establish(t, mode.pair, remote,
				cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
				srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
				session.AcceptConfig{})

			if cli.Role != session.RoleInitiator || srv.Role != session.RoleResponder {
				t.Fatalf("roles: %v / %v", cli.Role, srv.Role)
			}
			if cli.Suite != handshake.SuiteXK || srv.Suite != handshake.SuiteXK {
				t.Fatalf("suite: %d / %d", cli.Suite, srv.Suite)
			}
			if cli.SID != srv.SID {
				t.Fatalf("sid mismatch: %x / %x", cli.SID, srv.SID)
			}
			if cli.Peer == nil || srv.Peer == nil {
				t.Fatal("peer card missing")
			}
			if cli.Peer.DeviceID != srvNode.device.ID() || srv.Peer.DeviceID != cliNode.device.ID() {
				t.Fatal("peer device IDs")
			}
			if len(cli.Services) != 1 || cli.Services[0].ID != 3 || cli.Services[0].Version != 2 {
				t.Fatalf("negotiated services %+v", cli.Services)
			}
			if cli.Local.Device != cliNode.device || srv.Local.Device != srvNode.device {
				t.Fatal("established local identity is not the one passed in")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st, err := cli.Session.OpenStream(ctx, 3, 2)
			if err != nil {
				t.Fatalf("OpenStream: %v", err)
			}
			if !st.Selected() {
				t.Fatal("chat/2 not selected")
			}
			rst, err := srv.Session.AcceptStream(ctx)
			if err != nil {
				t.Fatalf("AcceptStream: %v", err)
			}
			if rst.Open().Service != 3 || rst.Open().Version != 2 {
				t.Fatalf("open %+v", rst.Open())
			}
			if err := st.CloseWithCode(frame.CodeNoError, ""); err != nil {
				t.Fatalf("CloseWithCode: %v", err)
			}
			_, err = rst.ReadFrame()
			var closed *mux.CloseError
			if !errors.As(err, &closed) || closed.Code != frame.CodeNoError {
				t.Fatalf("ReadFrame: %v, want CLOSE", err)
			}

			back, err := srv.Session.OpenStream(ctx, 3, 2)
			if err != nil {
				t.Fatalf("server OpenStream: %v", err)
			}
			if _, err := cli.Session.AcceptStream(ctx); err != nil {
				t.Fatalf("client AcceptStream: %v", err)
			}
			if err := back.WriteFrame(frame.TypePadding, 0, make([]byte, 8)); err != nil {
				t.Fatalf("server WriteFrame: %v", err)
			}
		})
	}
}

// TestXXThrowawaySession bootstraps without a card under a throwaway identity (handshake §4.2).
func TestXXThrowawaySession(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	// The throwaway card names the throwaway hs key and nothing links it to a real account.
	throwaway := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXX}, 3,
		uint64(now.Add(24*time.Hour).Unix()))

	cli, srv := establish(t, memcarrier.NewQUIC(), nil,
		cliNode.local(throwaway, handshake.OwnCards{}, []uint16{handshake.SuiteXX}, true),
		srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
		session.AcceptConfig{})

	if cli.Suite != handshake.SuiteXX || srv.Suite != handshake.SuiteXX {
		t.Fatalf("suite: %d / %d", cli.Suite, srv.Suite)
	}
	if !cli.Unverified {
		t.Fatal("XX to a bare address must be unverified")
	}
	if cli.SID != srv.SID {
		t.Fatal("sid mismatch")
	}
}

// TestSealedCarrierPlaintextAfterSwitchClosesPath injects a plaintext frame after the sealing
// switch point; the path must end (wire.md §8.5).
func TestSealedCarrierPlaintextAfterSwitchClosesPath(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 8,
		uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)

	a, b := net.Pipe()
	var cb [32]byte
	if _, err := rand.Read(cb[:]); err != nil {
		t.Fatal(err)
	}
	client := mux.NewConn(a, mux.ConnConfig{Client: true, ChannelBinding: cb, Sealed: true})
	server := mux.NewConn(b, mux.ConnConfig{Client: false, ChannelBinding: cb, Sealed: true})
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, remote, dialAttempt(),
			cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false),
			session.WithCarrier(client))
		dialed <- runResult{est, err}
	}()
	srvEst, err := session.Accept(ctx, &oneListener{c: server}, session.AcceptConfig{
		Local: srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false),
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	r := <-dialed
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	t.Cleanup(func() { _ = r.est.Session.Close(); _ = srvEst.Session.Close() })

	// A plaintext PING frame is not a record: the peer's path must end. The write itself may
	// fail when the peer closes during it, which is the outcome under test.
	raw := frame.AppendFrame(nil, frame.Frame{Type: frame.TypePing, Payload: make([]byte, 8)})
	if err := a.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _ = a.Write(raw)
	// A failed path is dropped at once; the session itself may be kept for a replacement path
	// (wire.md §8.3, the no-path grace period), so wait for the path to disappear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, _, active := srvEst.Session.Paths()
		if len(ids) == 0 && !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sealed path accepted a plaintext frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type noPasses struct{}

func (noPasses) LookupPass([]byte, time.Time) (handshake.Pass, bool) { return handshake.Pass{}, false }

// TestHiddenNodeWithoutAccessSendsNothing checks the uniform silent failure of handshake §5:
// zero bytes leave the hidden node.
func TestHiddenNodeWithoutAccessSendsNothing(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 9,
		uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	pair := memcarrier.NewQUIC()
	counted := &countingCarrier{Carrier: pair.Server}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accepted := make(chan runResult, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: counted}, session.AcceptConfig{
			Local:  srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false),
			Hidden: true,
			Passes: noPasses{},
			Rand:   oneReader{},
		})
		accepted <- runResult{est, err}
	}()

	est, err := session.Dial(ctx, remote, dialAttempt(),
		cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false),
		session.WithCarrier(pair.Client))
	if err == nil {
		_ = est.Session.Close()
		t.Fatal("hidden node without access completed the handshake")
	}
	if !errors.Is(err, session.ErrSilent) {
		t.Fatalf("Dial: %v, want ErrSilent", err)
	}
	r := <-accepted
	if r.est != nil || !errors.Is(r.err, session.ErrSilent) {
		t.Fatalf("Accept: %v %v, want ErrSilent", r.est, r.err)
	}
	if n := counted.Bytes(); n != 0 {
		t.Fatalf("hidden node sent %d bytes", n)
	}
}

// oneReader makes the uniform silent-close delay deterministic (about 0.5 s) without feeding
// all-zero ephemeral keys to the Noise handshake.
type oneReader struct{}

func (oneReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 1
	}
	return len(p), nil
}

// TestDialRejectsExpiredOrInvalidCard maps a bad peer card to ErrRejected{card-invalid}.
func TestDialRejectsExpiredOrInvalidCard(t *testing.T) {
	now := time.Now()
	node := newTestNode(t)
	// Seal the card 70 s in the past, then verify it 40 s in the past: objects allows 60 s of
	// skew, so the verification succeeds and Dial sees a card that expired by the time it runs.
	expiredEnv := node.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 2,
		uint64(now.Add(-70*time.Second).Unix()))
	expired := verifyCard(t, expiredEnv, now.Add(-40*time.Second))

	local := node.local(nil, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false)
	_, err := session.Dial(context.Background(), expired, dialAttempt(), local)
	var rejected *session.ErrRejected
	if !errors.As(err, &rejected) || rejected.Reason != handshake.ReasonCardInvalid {
		t.Fatalf("expired card: %v", err)
	}

	valid := verifyCard(t, node.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 3,
		uint64(now.Add(24*time.Hour).Unix())), now)
	valid.Env.Raw[len(valid.Env.Raw)-1] ^= 0xff
	_, err = session.Dial(context.Background(), valid, dialAttempt(), local)
	if !errors.As(err, &rejected) || rejected.Reason != handshake.ReasonCardInvalid {
		t.Fatalf("invalid card: %v", err)
	}
}

// TestDialRejectsClockSkew checks a rejection inside P2 (handshake §7): the reason is visible to
// both sides as ErrRejected.
func TestDialRejectsClockSkew(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 15,
		uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	pair := memcarrier.NewQUIC()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accepted := acceptIn(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
		Local: srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false),
	})
	_, err := session.Dial(ctx, remote, dialAttempt(),
		cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false),
		session.WithCarrier(pair.Client), session.WithOffset(20*time.Minute))
	var rejected *session.ErrRejected
	if !errors.As(err, &rejected) || rejected.Reason != handshake.ReasonClockSkew {
		t.Fatalf("Dial: %v, want clock-skew rejection", err)
	}
	r := <-accepted
	if r.est != nil || !errors.As(r.err, &rejected) || rejected.Reason != handshake.ReasonClockSkew {
		t.Fatalf("Accept: %v %v, want clock-skew rejection", r.est, r.err)
	}
}

// TestDialRejectsWrongExpectedDeviceID checks the rid binding of handshake §3.3 / §4.2: the
// prologue names the expected device, but the card in P2 is signed by another one. The initiator
// must abort without sending message 3.
func TestDialRejectsWrongExpectedDeviceID(t *testing.T) {
	now := time.Now()
	cliNode, srvNode, foreign := newTestNode(t), newTestNode(t), newTestNode(t)
	notAfter := uint64(now.Add(24 * time.Hour).Unix())
	// The hs key is the responder's, so the Noise pre-message and the card check agree; the
	// signer is a different device, so the card does not name the expected rid.
	foreignCard := foreign.cardWithHs(t, srvNode.device.Handshake().Public(), objects.ScopePublic,
		[]uint16{handshake.SuiteXK, handshake.SuiteXX}, 11, notAfter)
	own := handshake.OwnCards{Current: map[uint32]handshake.OwnCard{
		uint32(objects.ScopePublic): {Seq: 11, Envelope: foreignCard},
	}}
	throwaway := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXX}, 4, notAfter)
	pair := memcarrier.NewQUIC()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accepted := make(chan runResult, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
			Local: srvNode.local(nil, own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
		})
		accepted <- runResult{est, err}
	}()

	expected := srvNode.device.ID()
	_, err := session.Dial(ctx, nil, dialAttempt(),
		cliNode.local(throwaway, handshake.OwnCards{}, []uint16{handshake.SuiteXX}, true),
		session.WithCarrier(pair.Client), session.WithExpectedDeviceID(expected))
	var rejected *session.ErrRejected
	if !errors.As(err, &rejected) || rejected.Reason != handshake.ReasonCardInvalid {
		t.Fatalf("wrong rid: %v", err)
	}
	<-accepted // the responder sees the carrier close; its own error is not the subject here.
}

// TestResponderRejectsInvalidInitiatorCard checks REJECT{card-invalid} after message 3 (§8).
func TestResponderRejectsInvalidInitiatorCard(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	expired := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 5,
		uint64(now.Add(-2*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	pair := memcarrier.NewQUIC()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accepted := make(chan runResult, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
			Local: srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false),
		})
		accepted <- runResult{est, err}
	}()

	est, err := session.Dial(ctx, remote, dialAttempt(),
		cliNode.local(expired, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false),
		session.WithCarrier(pair.Client))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := <-accepted
	var rejected *session.ErrRejected
	if r.est != nil || !errors.As(r.err, &rejected) || rejected.Reason != handshake.ReasonCardInvalid {
		t.Fatalf("Accept: %v %v, want REJECT{card-invalid}", r.est, r.err)
	}
	select {
	case <-est.Session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("initiator session did not end after REJECT{card-invalid}")
	}
	var muxReject *mux.RejectError
	if !errors.As(est.Session.Err(), &muxReject) || muxReject.Reason != handshake.ReasonCardInvalid {
		t.Fatalf("initiator session error: %v", est.Session.Err())
	}
}

func writeFlight(t *testing.T, s mux.RawStream, hello, msg1 []byte) {
	t.Helper()
	buf := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: hello})
	buf = frame.AppendFrame(buf, frame.Frame{Type: frame.TypeHsMsg, Payload: msg1})
	if _, err := s.Write(buf); err != nil {
		t.Fatalf("write flight: %v", err)
	}
}

// TestPuzzleRetriesAtMostTwo sends three HS_HELLO flights: the third gets no answer (§6).
func TestPuzzleRetriesAtMostTwo(t *testing.T) {
	now := time.Now()
	srvNode := newTestNode(t)
	nc := srvNode.cards(t, now)
	pair := memcarrier.NewQUIC()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accepted := make(chan runResult, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
			Local: srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false),
			Puzzle: &handshake.PuzzleConfig{
				Secret:     bytes.Repeat([]byte{7}, 32),
				Difficulty: func() uint8 { return 1 },
			},
		})
		accepted <- runResult{est, err}
	}()

	raw, err := pair.Client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := (&handshake.Hello{Offered: []uint16{handshake.SuiteXK}, Chosen: handshake.SuiteXK}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	rd := frame.NewReader(raw, nil)
	for i := 0; i < 2; i++ {
		writeFlight(t, raw, hello, []byte{1, 2, 3})
		f, err := rd.ReadFrame()
		if err != nil {
			t.Fatalf("flight %d: %v", i, err)
		}
		if f.Type != frame.TypeHsRetry {
			t.Fatalf("flight %d: %s, want HS_RETRY", i, frame.TypeName(f.Type))
		}
	}
	writeFlight(t, raw, hello, []byte{1, 2, 3})
	if err := raw.SetReadDeadline(time.Now().Add(1500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if f, err := rd.ReadFrame(); err == nil {
		t.Fatalf("third flight was answered: %s", frame.TypeName(f.Type))
	}
	r := <-accepted
	if r.est != nil || !errors.Is(r.err, session.ErrSilent) {
		t.Fatalf("Accept: %v %v, want ErrSilent", r.est, r.err)
	}
}

// TestDialRetriesSuiteRejectOnce checks that exactly one retry follows REJECT{suite} (§3.1).
func TestDialRetriesSuiteRejectOnce(t *testing.T) {
	t.Run("second responder accepts", func(t *testing.T) {
		now := time.Now()
		cliNode, srvA, srvB := newTestNode(t), newTestNode(t), newTestNode(t)
		cardsA, cardsB := srvA.cards(t, now), srvB.cards(t, now)
		cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK, handshake.SuiteXX}, 6,
			uint64(now.Add(24*time.Hour).Unix()))
		remote := verifyCard(t, cardsB.peer, now)
		first, second := memcarrier.NewQUIC(), memcarrier.NewQUIC()
		var calls atomic.Int32

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		aCh := acceptIn(ctx, &oneListener{c: first.Server}, session.AcceptConfig{
			Local: srvA.local(nil, cardsA.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
		})
		bCh := acceptIn(ctx, &oneListener{c: second.Server}, session.AcceptConfig{
			Local: srvB.local(nil, cardsB.own, []uint16{handshake.SuiteXX}, false),
		})
		est, err := session.Dial(ctx, remote, dialAttempt(),
			cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXX, handshake.SuiteXK}, false),
			session.WithCarrierFunc(func(context.Context) (mux.Carrier, error) {
				switch calls.Add(1) {
				case 1:
					return first.Client, nil
				case 2:
					return second.Client, nil
				}
				return nil, errors.New("unexpected third dial")
			}))
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = est.Session.Close() })
		if est.Suite != handshake.SuiteXX {
			t.Fatalf("suite %d, want XX", est.Suite)
		}
		ra := <-aCh
		var rejected *session.ErrRejected
		if ra.est != nil || !errors.As(ra.err, &rejected) || rejected.Reason != handshake.ReasonSuite {
			t.Fatalf("first responder: %v %v", ra.est, ra.err)
		}
		rb := <-bCh
		if rb.err != nil {
			t.Fatalf("second responder: %v", rb.err)
		}
		t.Cleanup(func() { _ = rb.est.Session.Close() })
		if got := calls.Load(); got != 2 {
			t.Fatalf("dial calls %d, want 2", got)
		}
	})

	t.Run("second reject ends the dial", func(t *testing.T) {
		now := time.Now()
		cliNode, srvA, srvC := newTestNode(t), newTestNode(t), newTestNode(t)
		cardsA, cardsC := srvA.cards(t, now), srvC.cards(t, now)
		cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK, handshake.SuiteXX}, 6,
			uint64(now.Add(24*time.Hour).Unix()))
		remote := verifyCard(t, cardsA.peer, now)
		first, second := memcarrier.NewQUIC(), memcarrier.NewQUIC()
		var calls atomic.Int32

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		aCh := acceptIn(ctx, &oneListener{c: first.Server}, session.AcceptConfig{
			Local: srvA.local(nil, cardsA.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
		})
		cCh := acceptIn(ctx, &oneListener{c: second.Server}, session.AcceptConfig{
			Local: srvC.local(nil, cardsC.own, []uint16{handshake.SuiteXK, handshake.SuiteXX}, false),
		})
		_, err := session.Dial(ctx, remote, dialAttempt(),
			cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXX, handshake.SuiteXK}, false),
			session.WithCarrierFunc(func(context.Context) (mux.Carrier, error) {
				switch calls.Add(1) {
				case 1:
					return first.Client, nil
				case 2:
					return second.Client, nil
				}
				return nil, errors.New("unexpected third dial")
			}))
		var rejected *session.ErrRejected
		if !errors.As(err, &rejected) || rejected.Reason != handshake.ReasonSuite {
			t.Fatalf("Dial: %v, want REJECT{suite}", err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("dial calls %d, want exactly 2", got)
		}
		<-aCh
		<-cCh
	})
}

// TestAcceptBindsPathToBinder routes a PATH_BIND into the mux.Binder of a live session instead
// of starting a new handshake (wire.md §6.2, §8.2).
func TestAcceptBindsPathToBinder(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)
	nc := srvNode.cards(t, now)
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 12,
		uint64(now.Add(24*time.Hour).Unix()))
	remote := verifyCard(t, nc.peer, now)
	features := []uint32{uint32(frame.FeaturePathMigration)}
	cliLocal := cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false)
	cliLocal.Features = features
	srvLocal := srvNode.local(nil, nc.own, []uint16{handshake.SuiteXK}, false)
	srvLocal.Features = features

	cli, srv := establish(t, memcarrier.NewQUIC(), remote, cliLocal, srvLocal, session.AcceptConfig{})
	if !cli.Session.HasFeature(frame.FeaturePathMigration) {
		t.Skip("path-migration was not negotiated")
	}

	binder := mux.NewBinder()
	binder.Add(srv.Session)
	pair := memcarrier.NewQUIC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bound := acceptIn(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
		Local:  srvLocal,
		Binder: binder,
	})
	id, err := cli.Session.BindPath(ctx, mux.NewGuard(pair.Client), mux.PathDirect)
	if err != nil {
		t.Fatalf("BindPath: %v", err)
	}
	if id == 0 {
		t.Fatal("path ID 0")
	}
	r := <-bound
	if r.est != nil || !errors.Is(r.err, session.ErrPathBound) {
		t.Fatalf("Accept: %v %v, want ErrPathBound", r.est, r.err)
	}
}

// TestAcceptClosesOnNonHandshakeFirstFrame verifies the "anything else: silent close" rule.
func TestAcceptClosesOnNonHandshakeFirstFrame(t *testing.T) {
	srvNode := newTestNode(t)
	pair := memcarrier.NewQUIC()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accepted := make(chan runResult, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: pair.Server}, session.AcceptConfig{
			Local: srvNode.local(nil, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false),
		})
		accepted <- runResult{est, err}
	}()
	raw, err := pair.Client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypePing, Payload: make([]byte, 8)})); err != nil {
		t.Fatal(err)
	}
	r := <-accepted
	if r.est != nil || !errors.Is(r.err, session.ErrSilent) {
		t.Fatalf("Accept: %v %v, want silent close", r.est, r.err)
	}
}

// Compile-time check that the test listener satisfies carrier.Listener.
var _ carrier.Listener = (*oneListener)(nil)
