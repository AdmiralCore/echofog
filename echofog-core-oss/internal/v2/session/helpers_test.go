package session_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// testServices is the one service both sides negotiate in these tests: chat/2.
var testServices = []handshake.ServiceOffer{{ID: 3, Versions: []uint32{2}}}

// testNode is a throwaway account with one device and a signed devices/2 list. It is the raw
// material every test identity and card is built from.
type testNode struct {
	account *identity.Account
	device  *identity.Device
	devices []byte
}

func newTestNode(t *testing.T) *testNode {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatalf("identity.NewThrowaway: %v", err)
	}
	t.Cleanup(tw.Destroy)
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		t.Fatalf("DeviceList: %v", err)
	}
	return &testNode{account: tw.Account, device: tw.Device, devices: devices}
}

// card seals a card of the given scope. The caller controls seq and not_after, so expired cards
// (sealed with a nil clock check, as objects.SealCard does) are easy to make.
func (n *testNode) card(t *testing.T, scope objects.Scope, suites []uint16, seq, notAfter uint64) []byte {
	t.Helper()
	l1 := make([]uint32, 0, len(suites))
	for _, s := range suites {
		l1 = append(l1, uint32(s))
	}
	body := &pb.NodeCard{
		ProtoVer: handshake.ProtoVer,
		HsKey:    n.device.Handshake().Public(),
		L1Suites: l1,
		Account:  n.account.Public(),
		Devices:  n.devices,
		Scope:    uint32(scope),
	}
	var env []byte
	err := n.device.WithSigner(func(s *keys.Signer) error {
		var err error
		env, err = objects.SealCard(s, seq, notAfter, body)
		return err
	})
	if err != nil {
		t.Fatalf("SealCard: %v", err)
	}
	return env
}

// nodeCards is the responder's OwnCards plus the envelopes the tests hand to the initiator.
type nodeCards struct {
	peer, public []byte
	own          handshake.OwnCards
}

func (n *testNode) cards(t *testing.T, now time.Time) nodeCards {
	t.Helper()
	nc := nodeCards{own: handshake.OwnCards{
		Current: map[uint32]handshake.OwnCard{},
		Issued:  map[uint64]handshake.IssuedCard{},
	}}
	notAfter := uint64(now.Add(24 * time.Hour).Unix())
	nc.peer = n.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK, handshake.SuiteXX}, 10, notAfter)
	nc.public = n.card(t, objects.ScopePublic, []uint16{handshake.SuiteXK, handshake.SuiteXX}, 11, notAfter)
	nc.own.Current[uint32(objects.ScopePeer)] = handshake.OwnCard{Seq: 10, Envelope: nc.peer}
	nc.own.Current[uint32(objects.ScopePublic)] = handshake.OwnCard{Seq: 11, Envelope: nc.public}
	nc.own.Issued[10] = handshake.IssuedCard{Scope: uint32(objects.ScopePeer), At: now}
	nc.own.Issued[11] = handshake.IssuedCard{Scope: uint32(objects.ScopePublic), At: now}
	return nc
}

func (n *testNode) local(card []byte, own handshake.OwnCards, suites []uint16, throwaway bool) session.LocalIdentity {
	return session.LocalIdentity{
		Device:    n.device,
		Card:      card,
		OwnCards:  own,
		Suites:    suites,
		Services:  testServices,
		Throwaway: throwaway,
	}
}

func verifyCard(t *testing.T, env []byte, now time.Time) *objects.Card {
	t.Helper()
	c, err := objects.VerifyCard(env, now)
	if err != nil {
		t.Fatalf("objects.VerifyCard: %v", err)
	}
	return c
}

// oneListener hands out one carrier and then reports exhaustion.
type oneListener struct {
	c    mux.Carrier
	mu   sync.Mutex
	used bool
}

func (l *oneListener) Accept(ctx context.Context) (mux.Carrier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		return nil, errors.New("oneListener: no more carriers")
	}
	l.used = true
	return l.c, nil
}

func (l *oneListener) Addr() net.Addr { return nil }
func (l *oneListener) Close() error   { return nil }

// queueListener hands out carriers in order and blocks when empty.
type queueListener struct {
	ch chan mux.Carrier
}

func newQueueListener(cs ...mux.Carrier) *queueListener {
	l := &queueListener{ch: make(chan mux.Carrier, len(cs))}
	for _, c := range cs {
		l.ch <- c
	}
	return l
}

func (l *queueListener) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *queueListener) Addr() net.Addr { return nil }
func (l *queueListener) Close() error   { return nil }

// countingCarrier counts the bytes the local side writes to every stream of a carrier.
type countingCarrier struct {
	mux.Carrier
	n atomic.Int64
}

func (c *countingCarrier) OpenStream(ctx context.Context) (mux.RawStream, error) {
	s, err := c.Carrier.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return &countingStream{RawStream: s, n: &c.n}, nil
}

func (c *countingCarrier) AcceptStream(ctx context.Context) (mux.RawStream, error) {
	s, err := c.Carrier.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &countingStream{RawStream: s, n: &c.n}, nil
}

func (c *countingCarrier) Bytes() int64 { return c.n.Load() }

type countingStream struct {
	mux.RawStream
	n *atomic.Int64
}

func (s *countingStream) Write(p []byte) (int, error) {
	n, err := s.RawStream.Write(p)
	s.n.Add(int64(n))
	return n, err
}

type runResult struct {
	est *session.Established
	err error
}

// establish runs Dial and Accept on a fresh in-memory pair and returns both sides.
func establish(t *testing.T, pair *memcarrier.Pair, card *objects.Card, cli, srv session.LocalIdentity, cfg session.AcceptConfig) (*session.Established, *session.Established) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, card, dialAttempt(), cli, session.WithCarrier(pair.Client))
		dialed <- runResult{est, err}
	}()

	cfg.Local = srv
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

func dialAttempt() carrier.Attempt {
	return carrier.Attempt{Kind: carrier.AttemptCarrier}
}
