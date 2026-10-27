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

// ephemeralSocketKey marks a dial that must open its own fresh UDP socket (a new 5-tuple) instead
// of sharing the node's main listener.
type ephemeralSocketKey struct{}

// WithEphemeralSocket marks ctx so that even a noise dial opens its own ephemeral UDP socket,
// giving the connection a distinct 5-tuple. Flow-shaping rotation sets it on exit-tunnel re-dials
// so each rotated flow looks separate to a censor; the REALITY path already uses a fresh socket
// per dial regardless. Ordinary node<->node noise dials leave it unset and keep the main socket.
func WithEphemeralSocket(ctx context.Context) context.Context {
	return context.WithValue(ctx, ephemeralSocketKey{}, true)
}

func ephemeralSocketRequested(ctx context.Context) bool {
	v, _ := ctx.Value(ephemeralSocketKey{}).(bool)
	return v
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

// SetRegionNoise installs the region policy: a predicate that reports whether the node at addr is
// in this node's own region (its own country, or the local network). When it is, a camouflage SNI
// is ignored and the peer is dialed over the cheaper noise obfuscation instead of QUIC-REALITY:
// REALITY mimics an allowed foreign site so a connection survives a censoring border, which a
// connection that never crosses one does not need. nil (the default) keeps REALITY for every
// camouflaged peer, the behaviour before the region signal.
func (q *QuicTransport) SetRegionNoise(fn func(addr string) bool) {
	q.regionMu.Lock()
	q.regionFn = fn
	q.regionMu.Unlock()
}

// sameRegion reports whether the region policy places addr in this node's own region.
func (q *QuicTransport) sameRegion(addr string) bool {
	q.regionMu.RLock()
	fn := q.regionFn
	q.regionMu.RUnlock()
	return fn != nil && fn(addr)
}

// ExpectPeerSNI tells the transport that the node at addr has peer ID id and camouflage SNI sni.
func (q *QuicTransport) ExpectPeerSNI(addr string, id types.PeerID, sni string) {
	if sni != "" && q.sameRegion(addr) {
		// Same region (same country, or the local network): REALITY's cover-site mimicry buys
		// nothing for a connection that never crosses a censoring border, so drop the SNI and use
		// the cheaper noise obfuscation instead.
		q.ExpectPeer(addr, id)
		return
	}
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
		//   - ChromeParrot makes quic-go send Chrome's ClientHello (via uTLS) and Chrome's transport
		//     parameters: real GREASE (a GREASE cipher, GREASE-typed extensions, a GREASE key share
		//     and version), the X25519MLKEM768 key share, and a GREASE transport parameter. Without
		//     it the wire carried Go's crypto/tls fingerprint, which a JA3/JA4 censor blocks.
		//   - 1250-byte Initials (ChromeParrot also pins this), via InitialPacketSize.
		//   - The stealth token rides in the ClientHello random (tls.Config.Rand below): uTLS reads
		//     the 32-byte random as its first rand draw, so the token lands there and the Initial's
		//     Token field stays empty, as Chrome's is (a token only follows a NEW_TOKEN from an
		//     earlier visit). GREASE values are drawn from a later, fixed-size seed, so they do not
		//     disturb the token.
		// This needs the patched apernet/quic-go fork (see go.mod replace): stock ChromeParrot adds
		// the GREASE but its uTLS handshake cannot carry the TLS exporter EchoFog's peer-auth channel
		// binding needs; the fork exposes it (Conn.ExportKeyingMaterial, used by quicMuxConn). Only
		// this ephemeral, one-connection-per-dial config is reshaped; the shared listener and noise
		// dials keep quic-go's own.
		if q.realityConn != nil && haveExpected {
			if r, rerr := mimicry.QUICInitialRand(mimicry.NodeStealthKey(q.obfsSecret, expected), tlsCfg.NextProtos); rerr == nil {
				tlsCfg.Rand = r
				reality := *quicCfg
				reality.InitialPacketSize = 1250
				reality.ChromeParrot = true
				realityCfg = &reality
			}
		}
	}

	var conn *quic.Conn
	var dialErr error
	var ephemeralTr *quic.Transport
	var uconn *net.UDPConn
	reality := tlsCfg.ServerName != ""
	// A fresh per-dial socket (its own 5-tuple) is always used for a REALITY dial, and for a noise
	// dial that asked for one (flow-shaping rotation). Without it a rotated noise exit tunnel would
	// go out from the shared main socket and reuse its 5-tuple, so a censor would still see one
	// continuous flow instead of several short, rotated ones.
	if reality || ephemeralSocketRequested(ctx) {
		laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
		var lerr error
		uconn, lerr = net.ListenUDP("udp", laddr)
		if lerr == nil {
			var pconn net.PacketConn = uconn
			if reality {
				if q.realityConn != nil {
					pconn = NewQuicRealityConn(uconn, q)
				}
			} else if q.obfs != nil {
				// Mask the ephemeral socket with the node's shared obfuscator so the rotated noise
				// flow keys exactly as the main socket would: the peer was already Expect()ed, and
				// the obfuscator tracks keys per remote address, which does not change on rotation.
				pconn = dpi.WrapPacketConn(uconn, q.obfs)
			}
			ephemeralTr = &quic.Transport{Conn: pconn}
			if realityCfg.ChromeParrot {
				// Chrome's QUIC client uses a zero-length source connection ID; match it so the
				// parrot is not given away by an 8-byte SCID quic-go would otherwise generate. Safe
				// here only because this transport carries exactly one connection per dial, which is
				// what ZeroLengthConnectionIDGenerator requires.
				ephemeralTr.ConnectionIDGenerator = quic.ZeroLengthConnectionIDGenerator{}
			}
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

	q.newConn(conn, true, conn.RemoteAddr().String(), true, peerRes.PeerID, peerRes.PubKey, peerRes.Caps, reality)
	q.logger.Info("Peer authenticated and TLS confirmed", "peer_id", peerRes.PeerID, "direction", "outbound", "remote_addr", conn.RemoteAddr().String())

	return nil
}
