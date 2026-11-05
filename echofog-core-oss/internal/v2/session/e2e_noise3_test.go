//go:build noise3

// This end-to-end test runs a full session over the real noise3 carrier. It is behind the
// "noise3" build tag because T2.4 (the noise3 carrier) is not merged into v2 yet; run it with
//
//	go test -tags noise3 ./internal/v2/session -run TestE2ENoise3
//
// once T2.4 is in. Without the tag the test is not compiled, so the branch stays green.
package session_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// TestE2ENoise3 establishes a session over noise3 and opens a service stream.
func TestE2ENoise3(t *testing.T) {
	now := time.Now()
	cliNode, srvNode := newTestNode(t), newTestNode(t)

	var maskSK [32]byte
	if _, err := rand.Read(maskSK[:]); err != nil {
		t.Fatal(err)
	}
	maskPriv, err := ecdh.X25519().NewPrivateKey(maskSK[:])
	if err != nil {
		t.Fatal(err)
	}
	params, err := objects.Marshal(&pb.SuiteParams{Shape: &pb.ShapeProfile{Id: shape.IDDefaultUDP}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := noise3.Listen("127.0.0.1:0", noise3.ListenerConfig{
		Keys: []noise3.MaskKey{{Private: maskSK, NotAfter: now.Add(time.Hour)}},
	})
	if err != nil {
		t.Fatalf("noise3.Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := &pb.Addr{
		Suite:  objects.SuiteNoise3,
		Host:   "127.0.0.1",
		Port:   uint32(listener.Addr().(*net.UDPAddr).Port),
		Mask:   1,
		Params: params,
	}
	notAfter := uint64(now.Add(24 * time.Hour).Unix())

	cardBody := &pb.NodeCard{
		ProtoVer: handshake.ProtoVer,
		HsKey:    srvNode.device.Handshake().Public(),
		L1Suites: []uint32{handshake.SuiteXK, handshake.SuiteXX},
		Addrs:    []*pb.Addr{addr},
		MaskKeys: []*pb.MaskKey{{
			Key:      keys.New(keys.X25519, maskPriv.PublicKey().Bytes()),
			NotAfter: uint64(now.Add(time.Hour).Unix()),
		}},
		Account: srvNode.account.Public(),
		Devices: srvNode.devices,
		Scope:   uint32(objects.ScopePeer),
	}
	var srvEnv []byte
	if err := srvNode.device.WithSigner(func(s *keys.Signer) error {
		var err error
		srvEnv, err = objects.SealCard(s, 20, notAfter, cardBody)
		return err
	}); err != nil {
		t.Fatalf("SealCard: %v", err)
	}
	remote := verifyCard(t, srvEnv, now)
	own := handshake.OwnCards{
		Current: map[uint32]handshake.OwnCard{uint32(objects.ScopePeer): {Seq: 20, Envelope: srvEnv}},
		Issued:  map[uint64]handshake.IssuedCard{20: {Scope: uint32(objects.ScopePeer), At: now}},
	}
	cliCard := cliNode.card(t, objects.ScopePeer, []uint16{handshake.SuiteXK}, 21, notAfter)

	dialer := noise3.NewDialer(noise3.DialerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialed := make(chan runResult, 1)
	go func() {
		est, err := session.Dial(ctx, remote, dialAttempt(),
			cliNode.local(cliCard, handshake.OwnCards{}, []uint16{handshake.SuiteXK}, false),
			session.WithCarrierFunc(func(ctx context.Context) (mux.Carrier, error) {
				return dialer.Dial(ctx, carrier.DialRequest{Card: remote, Addr: addr})
			}))
		dialed <- runResult{est, err}
	}()
	srvEst, err := session.Accept(ctx, listener, session.AcceptConfig{
		Local: srvNode.local(nil, own, []uint16{handshake.SuiteXK}, false),
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	r := <-dialed
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	t.Cleanup(func() { _ = r.est.Session.Close(); _ = srvEst.Session.Close() })

	if r.est.SID != srvEst.SID || r.est.Suite != handshake.SuiteXK {
		t.Fatalf("established mismatch: %+v / %+v", r.est, srvEst)
	}
	st, err := r.est.Session.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	rst, err := srvEst.Session.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if err := st.CloseWithCode(frame.CodeNoError, ""); err != nil {
		t.Fatalf("CloseWithCode: %v", err)
	}
	if _, err := rst.ReadFrame(); err == nil {
		t.Fatal("stream did not end")
	}
}
