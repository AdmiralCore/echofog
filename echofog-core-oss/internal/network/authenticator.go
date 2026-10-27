package network

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// PeerAuthenticator decouples L7 authentication & handshaking from L4 transport.
type PeerAuthenticator interface {
	AuthenticateInbound(ctx context.Context, conn MuxConn) (*handshake.HandshakeResult, error)
	AuthenticateOutbound(ctx context.Context, conn MuxConn) (*handshake.HandshakeResult, error)
}

// DefaultPeerAuthenticator runs the handshake negotiator on the first stream of a connection.
type DefaultPeerAuthenticator struct {
	signer          handshake.SignerGetterKey
	protocolVersion uint32
	timeout         time.Duration
	ddos            *handshake.AntiDDoS // how many peers try to connect: sets the puzzle difficulty
	caps            atomic.Uint32       // what this node offers peers, announced in the handshake
	listenPort      atomic.Uint32       // where this node accepts connections, announced when it dials
	hideListen      atomic.Bool         // a hidden node announces no port: nobody spreads its address
	gate            atomic.Pointer[access.Gate]
	keys            atomic.Pointer[access.Keyring]
	screen          atomic.Pointer[handshake.ScreenFunc]
}

// ScreenSetter is an authenticator that screens inbound peers by the key they claim (see
// handshake.NegotiatorConfig.Screen).
type ScreenSetter interface {
	SetScreen(handshake.ScreenFunc)
}

// SetScreen sets the screen of inbound handshakes; nil removes it.
func (a *DefaultPeerAuthenticator) SetScreen(f handshake.ScreenFunc) {
	if f == nil {
		a.screen.Store(nil)
		return
	}
	a.screen.Store(&f)
}

// AccessSetter is an authenticator that keeps a hidden node to the peers it invited (gate, on
// inbound connections) and proves this node's invitations to the hidden nodes it dials (keys).
type AccessSetter interface {
	SetAccess(gate *access.Gate, keys *access.Keyring)
	HideListenPort(hide bool)
}

// SetAccess sets the gate inbound peers must pass and the keyring of secrets proved when dialing;
// either may be nil.
func (a *DefaultPeerAuthenticator) SetAccess(gate *access.Gate, keys *access.Keyring) {
	a.gate.Store(gate)
	a.keys.Store(keys)
}

// HideListenPort stops announcing where this node accepts connections, so that the peers it dials
// keep its address to themselves (a hidden node).
func (a *DefaultPeerAuthenticator) HideListenPort(hide bool) { a.hideListen.Store(hide) }

type expectedPeerKey struct{}

// WithExpectedPeer tells the authenticator which peer a dial is meant to reach, so that it can
// prove the access secret this node holds for it.
func WithExpectedPeer(ctx context.Context, id types.PeerID) context.Context {
	return context.WithValue(ctx, expectedPeerKey{}, id)
}

func expectedPeer(ctx context.Context) (types.PeerID, bool) {
	id, ok := ctx.Value(expectedPeerKey{}).(types.PeerID)
	return id, ok
}

// ListenPortSetter is an authenticator that announces where the node accepts connections.
type ListenPortSetter interface {
	SetListenPort(port int)
}

// SetListenPort sets the port announced to the peers this node dials (0: none).
func (a *DefaultPeerAuthenticator) SetListenPort(port int) {
	if port < 0 || port > 0xffff {
		port = 0
	}
	a.listenPort.Store(uint32(port))
}

// CapsSetter is an authenticator that announces what the node offers.
type CapsSetter interface {
	SetCaps(handshake.Capability)
}

// SetCaps sets what the node announces to peers it connects with from now on.
func (a *DefaultPeerAuthenticator) SetCaps(c handshake.Capability) { a.caps.Store(uint32(c)) }

func NewDefaultPeerAuthenticator(signer handshake.SignerGetterKey, protocolVersion uint32, timeout time.Duration) *DefaultPeerAuthenticator {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	a := &DefaultPeerAuthenticator{
		signer:          signer,
		protocolVersion: protocolVersion,
		timeout:         timeout,
		ddos:            handshake.NewAntiDDoS(),
	}
	a.caps.Store(uint32(handshake.DefaultCaps))
	return a
}

func (a *DefaultPeerAuthenticator) negotiator(ctx context.Context, stream MuxStream, inbound bool) *handshake.Negotiator {
	cfg := &handshake.NegotiatorConfig{
		MyNetworkID: [32]byte{},
		MyVersion:   a.protocolVersion,
		MyCaps:      handshake.Capability(a.caps.Load()),
		RequirePoW:  false,
		AntiDDoS:    a.ddos,
		ListenPort:  uint16(a.listenPort.Load()),
	}
	if a.hideListen.Load() {
		cfg.ListenPort = 0
	}
	if !inbound && poolLinkRequested(ctx) {
		cfg.MyCaps |= handshake.CapPoolLink
	}
	if f := a.screen.Load(); inbound && f != nil {
		cfg.Screen = *f
	}
	if g := a.gate.Load(); inbound && g != nil && g.Enabled() {
		cfg.Admit = g.AdmitHandshake
	}
	if k := a.keys.Load(); !inbound && k != nil {
		if id, ok := expectedPeer(ctx); ok {
			if secret := k.Get(id); secret != nil {
				cfg.AccessProof = func(binding, nonce, myPub []byte) []byte {
					return access.HandshakeProof(secret, binding, nonce, myPub)
				}
			}
		}
	}
	return handshake.NewNegotiator(a.signer, cfg, &StreamWrapper{stream: stream})
}

func (a *DefaultPeerAuthenticator) AuthenticateOutbound(ctx context.Context, conn MuxConn) (*handshake.HandshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	binding, err := conn.ChannelBinding()
	if err != nil {
		return nil, fmt.Errorf("channel binding: %w", err)
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open outbound handshake stream: %w", err)
	}
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(a.timeout))

	peerRes, err := a.negotiator(ctx, stream, false).Negotiate(binding, true)
	if err != nil {
		return nil, fmt.Errorf("outbound handshake negotiation failed: %w", err)
	}
	if !SendReadyFrame(stream) {
		return nil, errors.New("failed to send ready frame to remote peer")
	}
	return peerRes, nil
}

func (a *DefaultPeerAuthenticator) AuthenticateInbound(ctx context.Context, conn MuxConn) (*handshake.HandshakeResult, error) {
	a.ddos.Note()
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	binding, err := conn.ChannelBinding()
	if err != nil {
		return nil, fmt.Errorf("channel binding: %w", err)
	}
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to accept inbound handshake stream: %w", err)
	}
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(a.timeout))

	peerRes, err := a.negotiator(ctx, stream, true).Negotiate(binding, false)
	if err != nil {
		return nil, fmt.Errorf("inbound handshake negotiation failed: %w", err)
	}
	if !AcceptReadyFrame(stream) {
		return nil, errors.New("failed to accept ready frame from remote peer")
	}
	return peerRes, nil
}
