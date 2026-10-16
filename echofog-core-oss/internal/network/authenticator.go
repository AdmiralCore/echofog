package network

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/handshake"
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

func (a *DefaultPeerAuthenticator) negotiator(stream MuxStream) *handshake.Negotiator {
	return handshake.NewNegotiator(a.signer, &handshake.NegotiatorConfig{
		MyNetworkID: [32]byte{},
		MyVersion:   a.protocolVersion,
		MyCaps:      handshake.Capability(a.caps.Load()),
		RequirePoW:  false,
		AntiDDoS:    a.ddos,
		ListenPort:  uint16(a.listenPort.Load()),
	}, &StreamWrapper{stream: stream})
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

	peerRes, err := a.negotiator(stream).Negotiate(binding, true)
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

	peerRes, err := a.negotiator(stream).Negotiate(binding, false)
	if err != nil {
		return nil, fmt.Errorf("inbound handshake negotiation failed: %w", err)
	}
	if !AcceptReadyFrame(stream) {
		return nil, errors.New("failed to accept ready frame from remote peer")
	}
	return peerRes, nil
}
