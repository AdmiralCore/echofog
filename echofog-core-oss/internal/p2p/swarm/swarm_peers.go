package swarm

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"

	"time"

	"github.com/echofog/echofog-core/internal/abuse"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/internal/p2p/peerstore"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

type TopologyObserver interface {
	OnPeerConnected(peerID types.PeerID, pubKey types.PeerPublicKey, addr string)
	OnPeerDisconnected(peerID types.PeerID)
}

// AddPeer registers an authenticated connection; it returns nil when the connection lost a
// simultaneous-dial tie and was closed. dialable says whether addr is where the peer
// accepts connections; an inbound TCP connection comes from an ephemeral port, which must not
// end up in the peer store or the DHT.
// keepDialedBy reports whether a connection in this direction wins a simultaneous-dial tie.
func (s *Swarm) keepDialedBy(isOut bool, remote types.PeerID) bool {
	selfLower := bytes.Compare(s.selfID[:], remote[:]) < 0
	return isOut == selfLower
}

// AddPeer records an authenticated connection as an active peer, wiring up its read loops and
// notifying observers. An existing connection to the same peer is replaced.
func (s *Swarm) AddPeer(peerPubKey types.PeerPublicKey, peerID types.PeerID, conn network.MuxConn, addr string, isOut bool, dialable bool) *Peer {
	return s.addPeer(peerPubKey, peerID, conn, addr, isOut, dialable, netip.Addr{}, nil)
}

// addPeer is AddPeer for a connection that came from ip and holds a place in the per-address
// limits, which release gives back once the peer is gone (both may be zero).
func (s *Swarm) addPeer(peerPubKey types.PeerPublicKey, peerID types.PeerID, conn network.MuxConn, addr string, isOut bool, dialable bool, ip netip.Addr, release func()) *Peer {

	s.mu.Lock()
	if old, exists := s.activePeers[peerID]; exists {
		if old.Alive() && old.isOut != isOut && !s.keepDialedBy(isOut, peerID) {
			// both sides dialled at once: each keeps the connection opened by the lower peer ID,
			// otherwise each could close the one the other kept and lose both
			s.mu.Unlock()
			_ = conn.CloseWithError(network.ErrCodeNormalClose, "duplicate connection")
			if release != nil {
				release()
			}
			return nil
		}
		old.Close()
	}
	s.mu.Unlock()

	advertised := addr
	if !dialable {
		advertised = "" // observers (DHT) and peer exchange only spread addresses that can be dialled back
	}
	p := NewPeer(s.ctx, peerPubKey, s.dispatcher, advertised, isOut)
	p.txBytes = &s.bytesTx
	p.ip = ip
	p.release = release
	pw := network.NewPeerWrapper(p.ctx, conn, peerID)

	if s.peerStore != nil && dialable {
		s.peerStore.SavePeerAsync(peerPubKey, p.addr, 100)
	}

	pw.OnData(func(msgType net_types.MessageType, payload []byte, peerID types.PeerID) {
		s.bytesRx.Add(uint64(len(payload)))
		// 0. a type this node runs no service for costs one map lookup, nothing more
		if s.Drops(msgType) {
			s.dropped(msgType, peerID)
			return
		}
		// 1. Fast Path: direct O(1) hashmap lookup without Protobuf overhead
		s.fastHandlersMu.RLock()
		fastHandler, hasFast := s.fastHandlers[msgType]
		s.fastHandlersMu.RUnlock()
		if hasFast && fastHandler != nil {
			if err := fastHandler(payload, peerID); err != nil {
				s.logger.Warn("Swarm FastPath: handler error", "type", msgType, "peer", peerID.String(), "err", err)
			}
			return
		}

		// 2. Generic Frame Handlers fallback (e.g. virtual circuits, relaying)
		s.frameHandlersMu.RLock()
		for _, h := range s.frameHandlers {
			if h(msgType, payload, peerID) {
				s.frameHandlersMu.RUnlock()
				return
			}
		}
		s.frameHandlersMu.RUnlock()

		// 3. Async Path: Envelope unmarshaling and pushing to dispatcher queue
		var data internal_pb.Envelope
		err := proto.Unmarshal(payload, &data)
		if err != nil {
			return
		}

		s.dispatcher.PushMessage(&data, peerID)
	})

	pw.OnNewStream(func(stream *network.Stream) {
		if !s.acquireStream(p) {
			stream.Close()
			s.ReportPeer(peerID, abuse.Flood)
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.lim.streams.Add(-1)
			pending := true
			firstFrame := func() {
				if pending {
					pending = false
					p.pendingStreams.Add(-1)
				}
			}
			defer firstFrame()
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("Swarm: panic in OnNewStream handler", "peer", peerID.String(), "panic", r)
					stream.Close()
				}
			}()

			select {
			case <-s.ctx.Done():
				stream.Close()
				return
			default:
			}

			stream.SetReadDeadline(time.Now().Add(5 * time.Second))

			msgType, payload, err := stream.ReadFrame()
			firstFrame()
			if err != nil {
				stream.Close()
				return
			}
			if s.Drops(msgType) {
				s.dropped(msgType, peerID)
				stream.Close()
				return
			}

			switch msgType {
			case net_types.TypeSessionData:
				if len(payload) > 16 {
					stream.Close()
					s.ReportPeer(peerID, abuse.BadFrame)
					return
				}
				var sessionID types.SessionID
				copy(sessionID[:], payload)

				if s.sessionManager.AddStream(sessionID, stream) {
					stream.ResetDeadline()
					stream.StartLoops()
				} else {
					stream.Close()
					s.ReportPeer(peerID, abuse.Refused)
				}

			case net_types.TypeLocalPublicKeyBundleRequest:
				// a server-role node drops these above: it holds no chats, so it hands out no
				// bundle and never mints a one-time prekey for a stranger's request
				account, err := s.identity.GetActiveAccount()
				if err != nil {
					stream.Close()
					return
				}

				bundle, err := account.CreateJITBundle()
				if err != nil {
					stream.Close()
					return
				}

				protoBundle := &internal_pb.PreKeyBundle{
					IdentityKey:   bundle.IdentityKey[:],
					SignedPreKey:  bundle.SignedPreKey[:],
					Signature:     bundle.Signature[:],
					OneTimePreKey: joinOPKs(bundle.OneTimeKeys),
					CreatedAt:     bundle.CreatedAt,
				}

				bundleBytes, err := proto.Marshal(protoBundle)
				if err != nil {
					stream.Close()
					return
				}

				stream.WriteFrame(net_types.TypeLocalPublicKeyBundleResponse, bundleBytes)
				stream.Close()
			default:
				s.streamHandlersMu.RLock()
				sh := s.streamHandlers[msgType]
				s.streamHandlersMu.RUnlock()
				if sh != nil {
					stream.ResetDeadline()
					sh(stream, payload, peerID)
					return
				}
				s.executeRPC(msgType, payload, stream, peerID)
			}
		}()
	})

	initialProto := router.ProtoDirectQUIC
	if conn.Protocol() == network.ProtocolTCP {
		initialProto = router.ProtoTCPTLS
	} else if s.cfg != nil && s.cfg.AntiDPI.ObfsPassword != "" {
		initialProto = router.ProtoObfsQUIC
	}
	p.SetTransportWithProtocol(pw, initialProto)

	s.mu.Lock()
	s.activePeers[peerID] = p
	obs := s.observers
	s.mu.Unlock()

	// forget the peer once its connection dies, so it is redialled instead of staying a zombie
	// that silently swallows every frame
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-conn.Context().Done():
		case <-p.ctx.Done():
		case <-s.ctx.Done():
			return
		}
		s.mu.Lock()
		current := s.activePeers[peerID] == p
		if current {
			delete(s.activePeers, peerID)
		}
		obs := s.observers
		s.mu.Unlock()
		if current {
			s.logLinkClosed(peerID, conn, time.Since(p.connectedAt))
		}
		if p.isOut && network.LinkKind(conn) == network.ProtocolQUIC && network.IsIdleTimeout(context.Cause(conn.Context())) {
			s.preferTCP(conn) // UDP to this address went silent: TCP first next time
		}
		p.Close()
		s.noteLongLived(p)
		if p.release != nil {
			p.release()
		}
		if current {
			s.notifyPeerDisconnected(obs, peerID)
			s.emit("network:peer_state_changed", map[string]any{"peer": peerID.String(), "state": "disconnected"})
		}
	}()

	s.notifyPeerConnected(obs, peerID, peerPubKey, advertised)
	s.emit("network:peer_state_changed", map[string]any{"peer": peerID.String(), "state": "connected", "addr": addr})

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.exchangePeers(p)
	}()

	return p
}

func (s *Swarm) notifyPeerConnected(obs []TopologyObserver, peerID types.PeerID, peerPubKey types.PeerPublicKey, addr string) {
	for _, o := range obs {
		s.wg.Add(1)
		go func(observer TopologyObserver) {
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("Swarm: panic in OnPeerConnected observer", "peer_id", peerID.String(), "panic", r)
				}
			}()
			select {
			case <-s.ctx.Done():
				return
			default:
				observer.OnPeerConnected(peerID, peerPubKey, addr)
			}
		}(o)
	}
}

func (s *Swarm) notifyPeerDisconnected(obs []TopologyObserver, peerID types.PeerID) {
	for _, o := range obs {
		s.wg.Add(1)
		go func(observer TopologyObserver) {
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("Swarm: panic in OnPeerDisconnected observer", "peer_id", peerID.String(), "panic", r)
				}
			}()
			select {
			case <-s.ctx.Done():
				return
			default:
				observer.OnPeerDisconnected(peerID)
			}
		}(o)
	}
}

// RemovePeer closes the connection to a peer and drops it from the active set.
func (s *Swarm) RemovePeer(peerID types.PeerID) {
	s.mu.Lock()
	if p, ok := s.activePeers[peerID]; ok {
		p.Close()
		delete(s.activePeers, peerID)

		obs := s.observers
		s.mu.Unlock()

		s.notifyPeerDisconnected(obs, peerID)
		s.emit("network:peer_state_changed", map[string]any{"peer": peerID.String(), "state": "disconnected"})
		return
	}
	s.mu.Unlock()
}

// logLinkClosed says which connection to a peer ended, after how long and why. People send these
// logs in: "the VPN dropped" is only clear with what link it was on and what ended it. A client
// has a handful of peers and logs it as info; a server, with thousands coming and going, as debug.
func (s *Swarm) logLinkClosed(peerID types.PeerID, conn network.MuxConn, lasted time.Duration) {
	why := "closed"
	if err := context.Cause(conn.Context()); err != nil && !errors.Is(err, context.Canceled) {
		why = err.Error()
	}
	level := slog.LevelInfo
	if s.Drops(net_types.TypeChatMessage) {
		level = slog.LevelDebug
	}
	s.log().Log(s.ctx, level, "Swarm: connection to peer ended", "peer", peerID.String()[:12],
		"link", network.DescribeLink(conn), "lasted", lasted.Round(time.Second).String(), "why", why)
}

// tcpPreferrer is the FallbackTransport: it can be told to dial an address over TCP first.
type tcpPreferrer interface{ PreferTCP(addr string) }

func (s *Swarm) preferTCP(conn network.MuxConn) {
	if t, ok := s.netTransport.(tcpPreferrer); ok && conn.RemoteAddr() != nil {
		t.PreferTCP(conn.RemoteAddr().String())
	}
}

// ResetQUICLink drops the connection to a peer when it rides QUIC, and makes the redial try TCP
// first: for a link that is up on paper but does not carry requests any more. It says whether it
// dropped one.
func (s *Swarm) ResetQUICLink(peerID types.PeerID) bool {
	p := s.GetPeer(peerID)
	if p == nil {
		return false
	}
	p.mu.RLock()
	tw := p.transport
	p.mu.RUnlock()
	if tw == nil || network.LinkKind(tw.GetConn()) != network.ProtocolQUIC {
		return false
	}
	s.preferTCP(tw.GetConn())
	s.log().Info("Swarm: dropping a QUIC link that stopped answering", "peer", peerID.String()[:12], "link", network.DescribeLink(tw.GetConn()))
	s.RemovePeer(peerID)
	return true
}

// LinkOf describes the connection to a peer ("quic [::]:60585->203.0.113.7:443"), "" when there
// is none.
func (s *Swarm) LinkOf(peerID types.PeerID) string {
	p := s.GetPeer(peerID)
	if p == nil {
		return ""
	}
	p.mu.RLock()
	tw := p.transport
	p.mu.RUnlock()
	if tw == nil {
		return ""
	}
	return network.DescribeLink(tw.GetConn())
}

// GetPeer returns the active peer with this ID, or nil if it is not connected.
func (s *Swarm) GetPeer(peerID types.PeerID) *Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activePeers[peerID]
}

// GetPeerAddr returns the remote address of a connected peer, or an empty string.
func (s *Swarm) GetPeerAddr(peerID types.PeerID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if p, ok := s.activePeers[peerID]; ok {
		return p.Addr()
	}
	return ""
}

// GetAllPeers returns a snapshot of every currently connected peer.
func (s *Swarm) GetAllPeers() []*Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]*Peer, 0, len(s.activePeers))
	for _, p := range s.activePeers {
		res = append(res, p)
	}
	return res
}

// ThisIsActivePeer reports whether a peer is currently connected.
func (s *Swarm) ThisIsActivePeer(peerID types.PeerID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.activePeers[peerID]
	return ok
}

// GetMyRandomPeers returns up to count connected peers chosen at random, e.g. for gossip.
func (s *Swarm) GetMyRandomPeers(count uint) []*Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]*Peer, 0, len(s.activePeers))

	var mapCount uint

	for _, p := range s.activePeers {
		if mapCount >= count {
			break
		}

		res = append(res, p)
		mapCount++
	}
	return res
}

// BanPeer blocks a peer: it is disconnected and refused until unbanned.
func (s *Swarm) BanPeer(peerID types.PeerID) {
	if s.peerStore != nil {
		_ = s.peerStore.BanPeer(peerID)
	}
	s.RemovePeer(peerID)
}

// UnBanPeer lifts a ban set by BanPeer.
func (s *Swarm) UnBanPeer(peerID types.PeerID) {
	if s.peerStore != nil {
		_ = s.peerStore.UnBanPeer(peerID)
	}
}

// CheckOnBan reports whether a peer is currently banned.
func (s *Swarm) CheckOnBan(peerID types.PeerID) bool {
	if s.peerStore != nil {
		return s.peerStore.IsBanned(peerID)
	}
	return false
}

// SavePeer stores a peer's address and trust score for reconnecting to it later.
func (s *Swarm) SavePeer(peerPubKey types.PeerPublicKey, addr string, trustScore uint32) {
	if s.peerStore != nil {
		_ = s.peerStore.SavePeer(peerPubKey, addr, trustScore)
	}
}

// GetPeerStore returns the store of known peers kept for reconnection.
func (s *Swarm) GetPeerStore() *peerstore.PeerStore {
	return s.peerStore
}

// AddObserver registers a listener notified when peers connect or disconnect.
func (s *Swarm) AddObserver(o TopologyObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, o)
}
