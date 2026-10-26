package network

import (
	"context"
	"errors"
	"net"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/handshake"
)

// Streams one peer may keep open at once on a connection (QUIC and smux alike).
const (
	MaxBidiStreamsPerPeer = 1024
	MaxUniStreamsPerPeer  = 256
)

// retryRate is the rate of new-connection Initials per second above which every new address must
// prove it is real with a QUIC Retry before the node does any TLS work for it.
const retryRate = 25.0

var errAddrRefused = errors.New("address refused")

// verifySourceAddress tells quic-go whether to answer an Initial with a Retry: always on a server
// (SetRetryAlways), otherwise once Initials arrive faster than retryRate. A Retry costs a real
// client one round trip; a spoofed source never sees it, so a flood of forged Initials costs the
// node no TLS work and no state. The packet goes out through the obfuscation layer like every
// reply, sealed with this node's key, which is what the dialer expects from it.
func (q *QuicTransport) verifySourceAddress(net.Addr) bool {
	if q.realityConn != nil && q.realityConn.serverMode {
		return false
	}
	q.initials.Note()
	return q.retryAlways.Load() || q.initials.Rate() >= retryRate
}

// connContext refuses a new connection before its TLS handshake when its address is banned or
// already holds all the peers it may: the refusal is one small packet.
func (q *QuicTransport) connContext(ctx context.Context, info *quic.ClientInfo) (context.Context, error) {
	if info != nil && q.guard.Load().Refuses(addrOfNet(info.RemoteAddr)) {
		return ctx, errAddrRefused
	}
	return ctx, nil
}

// SetRetryAlways makes every new address prove itself with a QUIC Retry (one extra round trip),
// not only under load. For servers: spoofed Initials then never cost a TLS handshake.
func (q *QuicTransport) SetRetryAlways(on bool) { q.retryAlways.Store(on) }

// Guard is the per-address limiter of inbound connections.
func (q *QuicTransport) Guard() *handshake.Guard { return q.guard.Load() }

// SetScreen sets the screen of inbound handshakes (see handshake.NegotiatorConfig.Screen).
func (q *QuicTransport) SetScreen(f handshake.ScreenFunc) {
	if s, ok := q.authenticator.(ScreenSetter); ok {
		s.SetScreen(f)
	}
}

// Guard is the per-address limiter of inbound connections.
func (t *TCPTransport) Guard() *handshake.Guard { return t.guard.Load() }

// SetScreen sets the screen of inbound handshakes.
func (t *TCPTransport) SetScreen(f handshake.ScreenFunc) {
	if s, ok := t.auth.(ScreenSetter); ok {
		s.SetScreen(f)
	}
}

// Guard is the per-address limiter shared by both transports.
func (f *FallbackTransport) Guard() *handshake.Guard { return f.QUIC.Guard() }

// SetGuard replaces the per-address limiter of both transports.
func (f *FallbackTransport) SetGuard(g *handshake.Guard) {
	f.QUIC.SetGuard(g)
	if f.TCP != nil {
		f.TCP.SetGuard(g)
	}
}

// SetScreen sets the screen of inbound handshakes on both transports.
func (f *FallbackTransport) SetScreen(s handshake.ScreenFunc) {
	f.QUIC.SetScreen(s)
	if f.TCP != nil {
		f.TCP.SetScreen(s)
	}
}

// SetRetryAlways: see QuicTransport.SetRetryAlways.
func (f *FallbackTransport) SetRetryAlways(on bool) { f.QUIC.SetRetryAlways(on) }
