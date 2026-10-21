package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
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

		q.applyCongestion(conn)
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
				if errors.Is(err, handshake.ErrPeerRefused) {
					c.CloseWithError(ErrCodeSpamDetected, "full") // the node is full, the peer did nothing wrong
					return
				}
				q.guard.Load().Fail(ip)
				c.CloseWithError(ErrCodeAuthFailed, "auth failed")
				q.logger.Debug("QuicTransport inbound authentication error", "err", err, "remote", c.RemoteAddr())
				return
			}
			q.guard.Load().Success(ip)

			addr, dialable := inboundListenAddr(c.RemoteAddr().String(), peerRes.ListenPort)
			q.newConn(c, false, addr, dialable, peerRes.PeerID, peerRes.PubKey, peerRes.Caps)
			q.logger.Debug("Peer authenticated and TLS confirmed", "peer_id", peerRes.PeerID, "direction", "inbound", "remote_addr", c.RemoteAddr().String())
		}(conn)
	}
}

// inboundListenAddr is where an inbound peer accepts connections: its source IP with the port it
// announced. Its source port is not that: behind a port map (Docker publishing 443 to the
// container's 4242) it is the inner port, which nobody outside can reach. A peer that announced
// no port is not dialable, so its address stays out of the peer store and the DHT.
func inboundListenAddr(remote string, announced uint16) (string, bool) {
	if announced == 0 {
		return remote, false
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote, false
	}
	return net.JoinHostPort(host, strconv.Itoa(int(announced))), true
}

func (q *QuicTransport) newConn(conn *quic.Conn, isOut bool, addr string, dialable bool, PeerID types.PeerID, PeerPubKey types.PeerPublicKey, caps handshake.Capability) {
	ev := NewConnEvent{
		Caps:       caps,
		Conn:       WrapQUIC(conn),
		Dialable:   dialable,
		IsOut:      isOut,
		PeerID:     PeerID,
		Addr:       addr,
		PeerPubKey: PeerPubKey,
		RemoteIP:   addrOfNet(conn.RemoteAddr()),
	}
	select {
	case q.connChan <- ev:
		q.arrivalNotify(PeerID)
	case <-q.closed:
		_ = conn.CloseWithError(ErrCodeNormalClose, "shutting down")
	}
}
