package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// ErrBootstrapTarget reports a bootstrap target that cannot be dialed.
var ErrBootstrapTarget = errors.New("node: bad bootstrap target")

// BootstrapTarget is one address of a bootstrap list that has no card (wire.md section 1).
type BootstrapTarget struct {
	// Addr is the address to dial: suite, host, port, mask index 1 and the suite parameters.
	// The suite must be noise3, reality2-quic or reality2-tcp.
	Addr *pb.Addr
	// Mask is the responder's masking public key from the list (the list is the only source).
	Mask keys.AKey
	// Expected is the device ID the list names for the address. Nil: bare address.
	Expected *DeviceID
}

// BootstrapResult is what an XX bootstrap session found.
type BootstrapResult struct {
	// Card is the responder's public card from message 2.
	Card *objects.Card
	// Verified is true only when the list named the device ID and the handshake checked it.
	Verified bool
}

// DialBootstrap runs the XX handshake to an address without a card (handshake.md section 4.2)
// under a fresh throwaway identity, reads the responder's card and closes the session.
//
// The session is not entered into the session table and nothing learned in it reaches the routing
// table, the card book or the peer store. The caller gets the card and decides: with Expected set
// and Verified true the handshake has checked SHA-256(card signer) against the list, and the card
// may be dialed with XK; otherwise it is only a candidate to verify through other sessions.
func (r *Runtime) DialBootstrap(ctx context.Context, t BootstrapTarget, opt DialOpt) (*BootstrapResult, error) {
	if !r.running() {
		return nil, ErrClosed
	}
	if r.Background() == BackgroundSuspended {
		return nil, ErrBackground
	}
	if ctx == nil {
		ctx = context.Background()
	}
	injected := opt.Carrier != nil || opt.CarrierFunc != nil
	if err := checkBootstrapTarget(t, injected); err != nil {
		return nil, err
	}

	tw, err := identity.NewThrowaway()
	if err != nil {
		return nil, fmt.Errorf("node: throwaway identity: %w", err)
	}
	defer tw.Destroy()
	local, err := r.throwawayLocal(ctx, tw)
	if err != nil {
		return nil, err
	}

	opt.Local = &local
	opt.Attempts = []carrier.Attempt{{Kind: carrier.AttemptCarrier, Suite: t.Addr.GetSuite(), Addr: t.Addr}}
	opt.CarrierCard = &objects.Card{Body: &pb.NodeCard{}}
	if len(t.Mask) > 0 {
		opt.CarrierCard.Body.MaskKeys = []*pb.MaskKey{{Key: append([]byte(nil), t.Mask...)}}
	}
	opt.ExpectedDeviceID = t.Expected
	opt.AccessSecret = nil

	est, err := r.dial(ctx, nil, local, opt, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = est.Session.Close() }()
	if est.Peer == nil {
		return nil, errors.New("node: bootstrap session has no peer card")
	}
	card, err := objects.VerifyCard(est.Peer.Raw, r.clock.Now())
	if err != nil {
		return nil, fmt.Errorf("node: bootstrap card: %w", err)
	}
	res := &BootstrapResult{Card: card}
	if t.Expected != nil && !est.Unverified && card.DeviceID == [32]byte(*t.Expected) {
		res.Verified = true
	}
	return res, nil
}

func checkBootstrapTarget(t BootstrapTarget, injected bool) error {
	a := t.Addr
	if a == nil {
		return fmt.Errorf("%w: no address", ErrBootstrapTarget)
	}
	switch a.GetSuite() {
	case objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP:
	default:
		return fmt.Errorf("%w: suite %d", ErrBootstrapTarget, a.GetSuite())
	}
	if a.GetHost() == "" || a.GetPort() == 0 || a.GetPort() > 65535 {
		return fmt.Errorf("%w: host or port", ErrBootstrapTarget)
	}
	if len(t.Mask) == 0 && !injected {
		return fmt.Errorf("%w: no masking key", ErrBootstrapTarget)
	}
	if len(t.Mask) > 0 {
		if _, err := keys.ParseKind(t.Mask, keys.X25519); err != nil {
			return fmt.Errorf("%w: masking key", ErrBootstrapTarget)
		}
	}
	return nil
}

// throwawayLocal builds the local identity of an XX bootstrap: a minimal peer card of the
// throwaway keys with no listeners, the XX suite only, the runtime's offers.
func (r *Runtime) throwawayLocal(ctx context.Context, tw *identity.Throwaway) (session.LocalIdentity, error) {
	devices, err := tw.DeviceList(1, r.clock.Now())
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: throwaway device list: %w", err)
	}
	b, err := cards.New(cards.Config{
		Account:    tw.Account.Public(),
		Device:     tw.Device,
		HsKey:      tw.Device.Handshake().Public(),
		L1Suites:   []uint32{uint32(handshake.SuiteXX)},
		Role:       objects.RoleUser,
		Devices:    devices,
		Visibility: cards.Hidden,
		Clock:      r.clock,
	})
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: throwaway card: %w", err)
	}
	peer, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: throwaway card: %w", err)
	}
	features := append([]uint32(nil), r.cfg.Features...)
	if r.cfg.Profile.LowPower {
		features = withoutFeature(features, idlePaddingFeature)
	}
	return session.LocalIdentity{
		Device:    tw.Device,
		Card:      peer.Envelope,
		Suites:    []uint16{handshake.SuiteXX},
		Features:  features,
		Services:  handshakeServices(r.services.Offer(false)),
		Throwaway: true,
	}, nil
}
