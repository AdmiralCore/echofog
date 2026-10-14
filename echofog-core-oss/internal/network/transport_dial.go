package network

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// pendingDial lets concurrent callers share one dial to the same address.
type pendingDial struct {
	done chan struct{}
	err  error
}

// DialEarly connects to addr. Concurrent dials to the same address are coalesced: besides
// avoiding duplicate connections, two parallel handshakes would share one cached TLS session
// ticket, which quic-go mutates without synchronisation.
func (q *QuicTransport) DialEarly(ctx context.Context, addr string) error {
	if addr == q.addrStr {
		return errSelfDial
	}

	q.dialMu.Lock()
	if q.dialing == nil {
		q.dialing = make(map[string]*pendingDial)
	}
	if p, ok := q.dialing[addr]; ok {
		q.dialMu.Unlock()
		select {
		case <-p.done:
			return p.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p := &pendingDial{done: make(chan struct{})}
	q.dialing[addr] = p
	q.dialMu.Unlock()

	p.err = q.dial(ctx, addr)
	q.dialMu.Lock()
	delete(q.dialing, addr)
	q.dialMu.Unlock()
	close(p.done)
	return p.err
}

// tlsConfigFor gives every remote address its own session cache. quic-go derives the cache key
// from the IP only, so peers behind one IP (NAT, one host) would otherwise share and race on
// one ticket, and 0-RTT would be attempted against the wrong peer.
func (q *QuicTransport) tlsConfigFor(addr string) *tls.Config {
	if q.tlsCfg == nil || q.tlsCfg.ClientSessionCache == nil {
		return q.tlsCfg
	}
	q.dialMu.Lock()
	if q.sessionCaches == nil || len(q.sessionCaches) > 4096 {
		q.sessionCaches = make(map[string]tls.ClientSessionCache)
	}
	cache, ok := q.sessionCaches[addr]
	if !ok {
		cache = tls.NewLRUClientSessionCache(1)
		q.sessionCaches[addr] = cache
	}
	q.dialMu.Unlock()
	cfg := q.tlsCfg.Clone()
	cfg.ClientSessionCache = cache
	return cfg
}

// ExpectPeer tells the transport that the node at addr has peer ID id. With obfuscation on it must
// be called before dialing addr: the packets to that node are sealed with a key derived from id.
func (q *QuicTransport) ExpectPeer(addr string, id types.PeerID) {
	q.book.set(addr, id)
	if q.obfs == nil {
		return
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return
	}
	_ = q.obfs.Expect(ua.AddrPort(), dpi.NodeKey(q.obfsSecret, id))
}

func (q *QuicTransport) dial(ctx context.Context, addr string) error {
	expected, haveExpected := q.book.get(addr)
	if q.obfs != nil && !haveExpected {
		return ErrPeerUnknown
	}

	targetAddres, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}

	conn, err := q.tr.Dial(ctx, targetAddres, q.tlsConfigFor(addr), q.quicCgf)
	if err != nil {
		return err
	}

	peerRes, err := q.authenticator.AuthenticateOutbound(ctx, WrapQUIC(conn))
	if err != nil {
		conn.CloseWithError(ErrCodeAuthFailed, "auth failed")
		return fmt.Errorf("outbound authentication failed: %w", err)
	}
	if haveExpected && peerRes.PeerID != expected {
		conn.CloseWithError(ErrCodeAuthFailed, "unexpected peer")
		return ErrPeerMismatch
	}

	q.newConn(conn, true, conn.RemoteAddr().String(), peerRes.PeerID, peerRes.PubKey, peerRes.Caps)
	q.logger.Info("Peer authenticated and TLS confirmed", "peer_id", peerRes.PeerID, "direction", "outbound", "remote_addr", conn.RemoteAddr().String())

	return nil
}
