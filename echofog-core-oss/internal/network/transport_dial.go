package network

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/echofog/echofog-core/internal/network/mimicry"
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
	var cfg *tls.Config
	if q.tlsCfg != nil {
		cfg = q.tlsCfg.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if sni, ok := q.book.getSNI(addr); ok && sni != "" {
		cfg.ServerName = sni
	} else if id, ok := q.book.get(addr); ok {
		if s, ok2 := q.book.getSNIByID(id); ok2 && s != "" {
			cfg.ServerName = s
		}
	}
	if cfg.ServerName != "" {
		cfg.Certificates = nil
		cfg.NextProtos = []string{"h3"}
		cfg.InsecureSkipVerify = true
	} else {
		cfg.NextProtos = []string{"echo-fog-protocole"}
	}
	if q.tlsCfg == nil || q.tlsCfg.ClientSessionCache == nil {
		return cfg
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
	cfg.ClientSessionCache = cache
	return cfg
}

// ExpectPeer tells the transport that the node at addr has peer ID id. With obfuscation on it must
// be called before dialing addr: the packets to that node are sealed with a key derived from id.
func (q *QuicTransport) ExpectPeer(addr string, id types.PeerID) {
	q.book.set(addr, id)
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err == nil {
		q.book.set(ua.String(), id)
	}
	if q.obfs == nil {
		return
	}
	if err != nil {
		return
	}
	_ = q.obfs.Expect(ua.AddrPort(), dpi.NodeKey(q.obfsSecret, id))
	if sni, ok := q.book.getSNI(addr); ok && sni != "" {
		q.obfs.SetRaw(ua.AddrPort(), true)
	} else if s, ok := q.book.getSNIByID(id); ok && s != "" {
		q.obfs.SetRaw(ua.AddrPort(), true)
	}
}

// ExpectPeerSNI tells the transport that the node at addr has peer ID id and camouflage SNI sni.
func (q *QuicTransport) ExpectPeerSNI(addr string, id types.PeerID, sni string) {
	q.book.setSNI(addr, id, sni)
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err == nil {
		q.book.setSNI(ua.String(), id, sni)
	}
	if q.obfs == nil {
		return
	}
	if err != nil {
		return
	}
	_ = q.obfs.Expect(ua.AddrPort(), dpi.NodeKey(q.obfsSecret, id))
	if sni != "" {
		q.obfs.SetRaw(ua.AddrPort(), true)
	}
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
	if !haveExpected {
		expected, haveExpected = q.book.get(targetAddres.String())
	}

	tlsCfg := q.tlsConfigFor(addr)
	if tlsCfg.ServerName == "" {
		if s, ok := q.book.getSNI(targetAddres.String()); ok && s != "" {
			tlsCfg.ServerName = s
		} else if s, ok := q.book.getSNI(addr); ok && s != "" {
			tlsCfg.ServerName = s
		} else if haveExpected {
			if s, ok := q.book.getSNIByID(expected); ok && s != "" {
				tlsCfg.ServerName = s
			}
		}
	}

	quicCfg := q.quicCgf
	realityCfg := quicCfg
	if tlsCfg.ServerName != "" {
		tlsCfg.Certificates = nil
		tlsCfg.NextProtos = []string{"h3"}
		tlsCfg.InsecureSkipVerify = true

		if q.obfs != nil {
			q.obfs.SetRaw(targetAddres.AddrPort(), true)
		}

		// REALITY client dial: shape the first Initial like a real Chrome's first visit to a site.
		//   - 1250-byte Initials (quic-go's default is larger), via InitialPacketSize.
		//   - The stealth token rides in the ClientHello random (tls.Config.Rand below), so the
		//     Initial's Token field is empty, as Chrome's is (a token only follows a NEW_TOKEN from
		//     an earlier visit).
		// crypto/tls already offers the X25519MLKEM768 key share and a GREASE transport parameter, so
		// those match Chrome too. We do NOT use apernet's ChromeParrot here: its uTLS handshake drops
		// the TLS exporter, which EchoFog's peer-auth channel binding needs, and its ClientHello still
		// lacks the GREASE a real Chrome sends (tracked as a quic-go dependency follow-up). Only this
		// ephemeral, one-connection-per-dial config is reshaped; the shared listener and noise dials
		// keep quic-go's own.
		if q.realityConn != nil && haveExpected {
			if r, rerr := mimicry.QUICInitialRand(mimicry.NodeStealthKey(q.obfsSecret, expected)); rerr == nil {
				tlsCfg.Rand = r
				reality := *quicCfg
				reality.InitialPacketSize = 1250
				realityCfg = &reality
			}
		}
	}

	var conn *quic.Conn
	var dialErr error
	var ephemeralTr *quic.Transport
	var uconn *net.UDPConn
	if tlsCfg.ServerName != "" {
		laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
		var lerr error
		uconn, lerr = net.ListenUDP("udp", laddr)
		if lerr == nil {
			var pconn net.PacketConn = uconn
			if q.realityConn != nil {
				pconn = NewQuicRealityConn(uconn, q)
			}
			ephemeralTr = &quic.Transport{Conn: pconn}
			conn, dialErr = ephemeralTr.Dial(ctx, targetAddres, tlsCfg, realityCfg)
			if dialErr != nil {
				_ = ephemeralTr.Close()
				_ = uconn.Close()
				conn = nil
				ephemeralTr = nil
				uconn = nil
			}
		}
	}
	if conn == nil {
		conn, dialErr = q.tr.Dial(ctx, targetAddres, tlsCfg, quicCfg)
	}
	if dialErr != nil {
		return dialErr
	}
	if ephemeralTr != nil && uconn != nil {
		go func() {
			<-conn.Context().Done()
			_ = ephemeralTr.Close()
			_ = uconn.Close()
		}()
	}
	q.applyCongestion(conn)

	actx := ctx
	if haveExpected {
		actx = WithExpectedPeer(ctx, expected)
	}
	peerRes, err := q.authenticator.AuthenticateOutbound(actx, WrapQUIC(conn))
	if err != nil {
		conn.CloseWithError(ErrCodeAuthFailed, "auth failed")
		return fmt.Errorf("outbound authentication failed: %w", err)
	}
	if haveExpected && peerRes.PeerID != expected {
		conn.CloseWithError(ErrCodeAuthFailed, "unexpected peer")
		return &PeerMismatchError{Got: peerRes.PeerID}
	}

	q.newConn(conn, true, conn.RemoteAddr().String(), true, peerRes.PeerID, peerRes.PubKey, peerRes.Caps)
	q.logger.Info("Peer authenticated and TLS confirmed", "peer_id", peerRes.PeerID, "direction", "outbound", "remote_addr", conn.RemoteAddr().String())

	return nil
}
