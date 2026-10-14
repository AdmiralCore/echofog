package network

import (
	"context"
	"fmt"

	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/quic-go/quic-go"
)

// ListenEarly starts accepting inbound QUIC connections (0-RTT capable).
func (q *QuicTransport) ListenEarly(ctx context.Context) error {
	ln, err := q.tr.ListenEarly(q.tlsCfg, q.quicCgf)
	if err != nil {
		return fmt.Errorf("QuicTransport Listen error: %v", err)
	}

	go q.ConnAcceptor(ctx, ln, q.protocolVersion)

	return nil
}

// ConnAcceptor accepts connections from the listener, authenticates each peer, and emits it on ConnChan.
func (q *QuicTransport) ConnAcceptor(ctx context.Context, ln *quic.EarlyListener, protocolVersion uint32) {
	defer ln.Close()
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err.Error() == "quic: transport closed" {
				return
			}

			q.logger.Error("QuicTransport Accept error", "err", err)
			continue
		}

		ip := addrOfNet(conn.RemoteAddr())
		releaseIP, allowed := q.guard.Load().Allow(ip)
		if !allowed {
			q.logger.Debug("QuicTransport: address is limited, dropping inbound connection", "remote", conn.RemoteAddr())
			conn.CloseWithError(ErrCodeSpamDetected, "too many attempts")
			continue
		}

		select {
		case q.handshakeSem <- struct{}{}:
		default:
			releaseIP()
			q.logger.Warn("QuicTransport: concurrent handshakes limit reached, dropping inbound connection", "remote", conn.RemoteAddr())
			conn.CloseWithError(ErrCodeSpamDetected, "handshake overloaded")
			continue
		}

		go func(c *quic.Conn) {
			defer func() { <-q.handshakeSem }()
			defer releaseIP()

			peerRes, err := q.authenticator.AuthenticateInbound(ctx, WrapQUIC(c))
			if err != nil {
				q.guard.Load().Fail(ip)
				c.CloseWithError(ErrCodeAuthFailed, "auth failed")
				q.logger.Debug("QuicTransport inbound authentication error", "err", err, "remote", c.RemoteAddr())
				return
			}
			q.guard.Load().Success(ip)

			q.newConn(c, false, c.RemoteAddr().String(), peerRes.PeerID, peerRes.PubKey, peerRes.Caps)
			q.logger.Info("Peer authenticated and TLS confirmed", "peer_id", peerRes.PeerID, "direction", "inbound", "remote_addr", c.RemoteAddr().String())
		}(conn)
	}
}

func (q *QuicTransport) newConn(conn *quic.Conn, isOut bool, addr string, PeerID types.PeerID, PeerPubKey types.PeerPublicKey, caps handshake.Capability) {
	ev := NewConnEvent{
		Caps:       caps,
		Conn:       WrapQUIC(conn),
		Dialable:   true, // a QUIC peer's source address is its listening socket
		IsOut:      isOut,
		PeerID:     PeerID,
		Addr:       addr,
		PeerPubKey: PeerPubKey,
	}
	select {
	case q.connChan <- ev:
		q.arrivalNotify(PeerID)
	case <-q.closed:
		_ = conn.CloseWithError(ErrCodeNormalClose, "shutting down")
	}
}
