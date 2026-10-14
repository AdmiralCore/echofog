package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"

	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/internal/p2p/peerstore"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/events"

	"google.golang.org/protobuf/proto"
)

type RPCServiceFunc func(msg *internal_pb.MessageData, peerID types.PeerID) (net_types.MessageType, *internal_pb.MessageData, error)

type TopologyObserver interface {
	OnPeerConnected(peerID types.PeerID, pubKey types.PeerPublicKey, addr string)
	OnPeerDisconnected(peerID types.PeerID)
}

type FrameHandler func(msgType net_types.MessageType, payload []byte, peerID types.PeerID) bool

// FastFrameHandler defines a zero-copy fast-path handler for a specific MessageType.
type FastFrameHandler func(payload []byte, peerID types.PeerID) error

// StreamHandler takes ownership of an incoming bidirectional stream after its first frame was read
type StreamHandler func(stream *network.Stream, payload []byte, peerID types.PeerID)

type Swarm struct {
	activePeers map[types.PeerID]*Peer
	mu          sync.RWMutex

	rpcHandlers map[net_types.MessageType]RPCServiceFunc
	rpcMu       sync.RWMutex

	fastHandlers     map[net_types.MessageType]FastFrameHandler
	fastHandlersMu   sync.RWMutex
	streamHandlers   map[net_types.MessageType]StreamHandler
	streamHandlersMu sync.RWMutex

	frameHandlers   []FrameHandler
	frameHandlersMu sync.RWMutex

	logger *slog.Logger
	wg     sync.WaitGroup

	observers   []TopologyObserver
	fetchBundle BundleFetcher
	emitter     events.Emitter

	dispatcher *dispatcher.Dispatcher
	storage    storage.Storage

	selfID     types.PeerID
	selfPubKey types.PeerPublicKey

	cardMu       sync.Mutex
	card         []byte    // our bundle record without one-time prekeys, sent with init packets
	cardAge      time.Time // when card was made
	netTransport network.Transport
	wsTransport  WSTransport // optional: dial and listen over WebSocket (behind a CDN)

	peerStore *peerstore.PeerStore

	identity *identity.ProtectedIdentity

	sessionManager *SessionManager

	cfg *config.AppConfig

	bytesRx atomic.Uint64
	bytesTx atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
}

func NewSwarm(ctx context.Context, netTransport network.Transport, selfID types.PeerID, d *dispatcher.Dispatcher, identity *identity.ProtectedIdentity, connChan <-chan network.NewConnEvent, storage storage.Storage, cfg *config.AppConfig) *Swarm {
	ctx, cancel := context.WithCancel(ctx)

	s := &Swarm{
		activePeers:    make(map[types.PeerID]*Peer),
		dispatcher:     d,
		selfID:         selfID,
		storage:        storage,
		cfg:            cfg,
		sessionManager: NewSessionManager(),
		identity:       identity,
		ctx:            ctx,
		cancel:         cancel,
		netTransport:   netTransport,
		peerStore:      peerstore.NewPeerStore(storage, nil),
		fastHandlers:   make(map[net_types.MessageType]FastFrameHandler),
		logger:         slog.Default().With("component", "swarm"),
	}

	s.sessionManager.sender = s
	s.sessionManager.ctx = s.ctx

	s.wg.Add(1)
	go s.registrationLoop(connChan)

	s.initCoreRPCs()
	s.registerObservedAddr()

	return s
}

func (s *Swarm) SetLogger(log *slog.Logger) {
	if s != nil && log != nil {
		s.logger = log
	}
}

func (s *Swarm) RegisterRPC(msgType net_types.MessageType, handler RPCServiceFunc) {
	s.rpcMu.Lock()
	defer s.rpcMu.Unlock()

	if s.rpcHandlers == nil {
		s.rpcHandlers = make(map[net_types.MessageType]RPCServiceFunc)
	}

	s.rpcHandlers[msgType] = handler
}

// RegisterFastFrameHandler registers a direct O(1) zero-copy fast path handler for a specific MessageType.
// RegisterStreamHandler routes incoming bidirectional streams whose first frame has msgType to handler.
func (s *Swarm) RegisterStreamHandler(msgType net_types.MessageType, handler StreamHandler) {
	s.streamHandlersMu.Lock()
	defer s.streamHandlersMu.Unlock()
	if s.streamHandlers == nil {
		s.streamHandlers = make(map[net_types.MessageType]StreamHandler)
	}
	s.streamHandlers[msgType] = handler
}

func (s *Swarm) RegisterFastFrameHandler(msgType net_types.MessageType, handler FastFrameHandler) {
	s.fastHandlersMu.Lock()
	defer s.fastHandlersMu.Unlock()

	if s.fastHandlers == nil {
		s.fastHandlers = make(map[net_types.MessageType]FastFrameHandler)
	}

	s.fastHandlers[msgType] = handler
}

func (s *Swarm) AddFrameHandler(handler FrameHandler) {
	s.frameHandlersMu.Lock()
	defer s.frameHandlersMu.Unlock()
	s.frameHandlers = append(s.frameHandlers, handler)
}

func (s *Swarm) initCoreRPCs() {
	s.RegisterRPC(net_types.TypeDAGSessionInitRequest, s.sessionManager.HandleDagSessionReq)
	s.initCoreFastHandlers()
}

func (s *Swarm) initCoreFastHandlers() {
	s.RegisterFastFrameHandler(net_types.TypeTransportMigrateRequest, s.handleTransportMigrateRequest)
	s.RegisterFastFrameHandler(net_types.TypeTransportMigrateAck, s.handleTransportMigrateAck)
}

func (s *Swarm) log() *slog.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func (s *Swarm) handleTransportMigrateRequest(payload []byte, peerID types.PeerID) error {
	migPayload, err := router.DecodeMigrationPayload(payload)
	if err != nil {
		s.log().Warn("Failed to decode migration payload", "peer", peerID.String(), "err", err)
		return err
	}

	s.log().Info("Received transport migration request from peer",
		"peer", peerID.String(),
		"target_protocol", migPayload.TargetProtocol.String(),
		"reason", migPayload.Reason)

	p := s.GetPeer(peerID)
	if p == nil {
		return errors.New("peer not found for migration request")
	}

	ackPayload := &router.MigrationPayload{
		TargetProtocol: migPayload.TargetProtocol,
		SessionNonce:   migPayload.SessionNonce,
		Timestamp:      time.Now().Unix(),
		Reason:         "acknowledged",
	}
	ackBytes, _ := ackPayload.Encode()
	return p.SendRaw(net_types.TypeTransportMigrateAck, ackBytes)
}

func (s *Swarm) handleTransportMigrateAck(payload []byte, peerID types.PeerID) error {
	migPayload, err := router.DecodeMigrationPayload(payload)
	if err != nil {
		return err
	}
	s.log().Info("Received transport migration ACK from peer",
		"peer", peerID.String(),
		"target_protocol", migPayload.TargetProtocol.String())
	return nil
}

func (s *Swarm) GetPeerProtocol(peerID types.PeerID) router.TransportProtocol {
	p := s.GetPeer(peerID)
	if p == nil {
		return 0
	}
	return p.ActiveProtocol()
}

func (s *Swarm) GetAllPeersProtocols() map[types.PeerID]router.TransportProtocol {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make(map[types.PeerID]router.TransportProtocol, len(s.activePeers))
	for id, p := range s.activePeers {
		res[id] = p.ActiveProtocol()
	}
	return res
}

func (s *Swarm) MigratePeerTransport(peerID types.PeerID, targetLink router.PeerTransportLink, reason string) error {
	p := s.GetPeer(peerID)
	if p == nil {
		return errors.New("peer not found")
	}
	return p.MigrateTransport(targetLink, reason)
}

func (s *Swarm) executeRPC(msgType net_types.MessageType, payload []byte, stream *network.Stream, peerID types.PeerID) {
	defer stream.Close()

	stream.SetDeadline(time.Now().Add(30 * time.Second))

	var env internal_pb.Envelope
	if err := proto.Unmarshal(payload, &env); err != nil {
		return
	}

	if !s.CheckSign(&env) {
		slog.Warn("RPC: Invalid signature")
		return
	}

	var msg internal_pb.MessageData
	if err := proto.Unmarshal(env.Data, &msg); err != nil {
		return
	}
	// the request comes over the peer's own connection: it must be signed by that peer
	if types.PeerPubKeyToID(types.PeerPublicKey(env.PubKey)) != peerID {
		slog.Warn("RPC: request not signed by the peer that sent it")
		return
	}

	s.rpcMu.RLock()
	handler, exists := s.rpcHandlers[msgType]
	s.rpcMu.RUnlock()

	if !exists {
		slog.Warn("RPC: No handler for", "type", msgType)
		return
	}

	respType, respMsg, err := handler(&msg, peerID)
	if err != nil || respMsg == nil {
		slog.Error("RPC: Handler failed", "err", err)
		return
	}

	respEnv, err := s.SignMessageData(respMsg)
	if err != nil {
		return
	}
	respBytes, _ := proto.Marshal(respEnv)

	_ = stream.WriteFrame(respType, respBytes)
}

func (s *Swarm) registrationLoop(ch <-chan network.NewConnEvent) {
	defer s.wg.Done()
	s.findAndConnectToPeers()

	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			if s.CheckOnBan(event.PeerID) {
				_ = event.Conn.CloseWithError(0x01, "banned")
				continue
			}

			p := s.AddPeer(event.PeerPubKey, event.PeerID, event.Conn, event.Addr, event.IsOut, event.Dialable)
			if p == nil {
				continue // duplicate connection, already closed
			}
			p.SetCaps(event.Caps)
			go p.transport.StartLoops()
		}
	}
}

func (s *Swarm) findAndConnectToPeers() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		history := s.GetHistoryConnected(0)
		for _, peer := range history {
			select {
			case <-s.ctx.Done():
				return
			default:
			}

			pubKey, _ := types.ToPeerPubKey(peer.PubKey)
			peerID := types.PeerPubKeyToID(pubKey)

			s.mu.RLock()
			if len(s.activePeers) > s.cfg.Network.MaxConnections {
				s.mu.RUnlock()
				break
			}
			_, ok := s.activePeers[peerID]
			s.mu.RUnlock()
			if ok {
				continue
			}

			s.wg.Add(1)
			go func(addr string) {
				defer s.wg.Done()
				select {
				case <-s.ctx.Done():
					return
				default:
					s.expectPeer(addr, peerID)
					s.connect(addr)
				}
			}(peer.GetLastKnownAddr())
		}
	}()
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

func (s *Swarm) AddPeer(peerPubKey types.PeerPublicKey, peerID types.PeerID, conn network.MuxConn, addr string, isOut bool, dialable bool) *Peer {

	s.mu.Lock()
	if old, exists := s.activePeers[peerID]; exists {
		if old.Alive() && old.isOut != isOut && !s.keepDialedBy(isOut, peerID) {
			// both sides dialled at once: each keeps the connection opened by the lower peer ID,
			// otherwise each could close the one the other kept and lose both
			s.mu.Unlock()
			_ = conn.CloseWithError(network.ErrCodeNormalClose, "duplicate connection")
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
	pw := network.NewPeerWrapper(p.ctx, conn, peerID)

	if s.peerStore != nil && dialable {
		s.peerStore.SavePeerAsync(peerPubKey, p.addr, 100)
	}

	pw.OnData(func(msgType net_types.MessageType, payload []byte, peerID types.PeerID) {
		s.bytesRx.Add(uint64(len(payload)))
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
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
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
			if err != nil {
				stream.Close()
				return
			}

			switch msgType {
			case net_types.TypeSessionData:
				if len(payload) > 16 {
					stream.Close()
					return
				}
				var sessionID types.SessionID
				copy(sessionID[:], payload)

				if s.sessionManager.AddStream(sessionID, stream) {
					stream.ResetDeadline()
					stream.StartLoops()
				} else {
					stream.Close()
				}

			case net_types.TypeLocalPublicKeyBundleRequest:
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
		p.Close()
		if current {
			s.notifyPeerDisconnected(obs, peerID)
			s.emit("network:peer_state_changed", map[string]any{"peer": peerID.String(), "state": "disconnected"})
		}
	}()

	s.notifyPeerConnected(obs, peerID, peerPubKey, advertised)
	s.emit("network:peer_state_changed", map[string]any{"peer": peerID.String(), "state": "connected", "addr": addr})

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

func (s *Swarm) Close() {
	s.cancel()
	if s.peerStore != nil {
		_ = s.peerStore.Close()
	}
	s.mu.Lock()
	for _, p := range s.activePeers {
		p.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Swarm) GetPeer(peerID types.PeerID) *Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activePeers[peerID]
}

func (s *Swarm) GetStorage() storage.Storage {
	return s.storage
}

func (s *Swarm) GetPeerAddr(peerID types.PeerID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if p, ok := s.activePeers[peerID]; ok {
		return p.Addr()
	}
	return ""
}

func (s *Swarm) GetAllPeers() []*Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]*Peer, 0, len(s.activePeers))
	for _, p := range s.activePeers {
		res = append(res, p)
	}
	return res
}

func (s *Swarm) ThisIsActivePeer(peerID types.PeerID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.activePeers[peerID]
	return ok
}

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

func (s *Swarm) BanPeer(peerID types.PeerID) {
	if s.peerStore != nil {
		_ = s.peerStore.BanPeer(peerID)
	}
	s.RemovePeer(peerID)
}

func (s *Swarm) UnBanPeer(peerID types.PeerID) {
	if s.peerStore != nil {
		_ = s.peerStore.UnBanPeer(peerID)
	}
}

func (s *Swarm) CheckOnBan(peerID types.PeerID) bool {
	if s.peerStore != nil {
		return s.peerStore.IsBanned(peerID)
	}
	return false
}

func (s *Swarm) SavePeer(peerPubKey types.PeerPublicKey, addr string, trustScore uint32) {
	if s.peerStore != nil {
		_ = s.peerStore.SavePeer(peerPubKey, addr, trustScore)
	}
}

func (s *Swarm) GetPeerStore() *peerstore.PeerStore {
	return s.peerStore
}

// expectPeer tells the transport whose address addr is: with obfuscation on, the packets to a node
// are keyed by its peer ID, so every dial has to name it.
// WSTransport is the optional WebSocket transport, used to reach peers at ws:// or wss://
// addresses (for going behind a CDN).
type WSTransport interface {
	Dial(ctx context.Context, url string) error
	ExpectPeer(addr string, id types.PeerID)
	ConnChan() <-chan network.NewConnEvent
}

// SetWSTransport attaches a WebSocket transport: its inbound peers register with the swarm, and
// addresses starting with ws:// or wss:// are dialed through it.
func (s *Swarm) SetWSTransport(w WSTransport) {
	s.wsTransport = w
	s.wg.Add(1)
	go s.registrationLoop(w.ConnChan())
}

func isWSAddr(addr string) bool {
	return strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://")
}

func (s *Swarm) expectPeer(addr string, id types.PeerID) {
	if isWSAddr(addr) {
		if s.wsTransport != nil {
			s.wsTransport.ExpectPeer(addr, id)
		}
		return
	}
	if e, ok := s.netTransport.(network.PeerExpecter); ok {
		e.ExpectPeer(addr, id)
	}
}

// dialEarly dials addr on the transport its scheme selects: WebSocket for ws:// and wss://,
// otherwise the QUIC/TCP transport.
func (s *Swarm) dialEarly(ctx context.Context, addr string) error {
	if isWSAddr(addr) {
		if s.wsTransport == nil {
			return errors.New("no WebSocket transport to dial " + addr)
		}
		return s.wsTransport.Dial(ctx, addr)
	}
	return s.netTransport.DialEarly(ctx, addr)
}

func (s *Swarm) connect(addr string) {
	ctx, cancel := context.WithTimeout(s.ctx, time.Second*3)
	defer cancel()
	err := s.dialEarly(ctx, addr)
	if err != nil {
		s.logger.Error("Failed to connect to swarm", "err: ", err)
		return
	}
}

func (s *Swarm) SendDataForPeer(peerID types.PeerID, msgType net_types.MessageType, data *internal_pb.MessageData) error {
	peer := s.GetPeer(peerID)
	if peer == nil {
		return errors.New("this peer is not connected")
	}

	env, err := s.SignMessageData(data)
	if err != nil {
		return err
	}

	return peer.Send(msgType, env)
}

// SendRawFrame sends raw binary bytes directly over the peer transport without Protobuf Envelope overhead (Fast Path).
func (s *Swarm) SendRawFrame(peerID types.PeerID, msgType net_types.MessageType, data []byte) error {
	peer := s.GetPeer(peerID)
	if peer == nil {
		return errors.New("this peer is not connected")
	}
	return peer.SendRaw(msgType, data)
}

// SendUnreliableFrame sends a loss-tolerant frame (real-time media) without retransmission.
func (s *Swarm) SendUnreliableFrame(peerID types.PeerID, msgType net_types.MessageType, data []byte) error {
	peer := s.GetPeer(peerID)
	if peer == nil {
		return errors.New("this peer is not connected")
	}
	return peer.SendRawUnreliable(msgType, data)
}

// GetHistoryConnected Set to 0 to get all peers
func (s *Swarm) GetHistoryConnected(count uint) []*storage.PeerStoreEntry {
	if s.peerStore != nil {
		return s.peerStore.GetHistoryConnected(count)
	}
	return nil
}

func (s *Swarm) OpenNewStreams(ctx context.Context, peerID types.PeerID, count uint32) ([]*network.Stream, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()

	if !ok {
		return nil, errors.New("this peer is inactive")
	}
	var (
		mu                = sync.Mutex{}
		wg                = sync.WaitGroup{}
		streams           = make([]*network.Stream, 0, count)
		openedCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	)
	defer cancel()

	wg.Add(int(count))
	for range count {
		go func() {
			defer wg.Done()
			stream, err := peer.transport.OpenBidirectionalStream(openedCtx)
			if err != nil {
				return
			}
			mu.Lock()
			streams = append(streams, stream)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(streams) == 0 {
		return nil, errors.New("failed to open any streams")
	}

	return streams, nil
}

func (s *Swarm) GetSessionManager() *SessionManager {
	return s.sessionManager
}

func (s *Swarm) SignMessageData(mesData *internal_pb.MessageData) (*internal_pb.Envelope, error) {
	if len(mesData.MessageId) == 0 {
		mesData.MessageId = types.NewMessageID()[:]
	}

	if len(mesData.OriginId) == 0 {
		mesData.OriginId = s.selfID[:]
	}

	if mesData.Timestamp == 0 {
		mesData.Timestamp = uint64(time.Now().Unix())
	}

	if mesData.HopLimit == 0 {
		mesData.HopLimit = types.HOPLIMIT
	}

	Data, err := proto.Marshal(mesData)
	if err != nil {
		return nil, err
	}

	var pubKey []byte
	pubKey, err = s.identity.GetPublicKey()
	if err != nil {
		return nil, err
	}

	var signature []byte
	signature, err = s.identity.SignData(Data)
	if err != nil {
		return nil, err
	}

	return &internal_pb.Envelope{
		Data:      Data,
		Signature: signature,
		PubKey:    pubKey[:],
	}, nil
}

func (s *Swarm) CheckSign(envelope *internal_pb.Envelope) bool {
	if len(envelope.PubKey) != 32 {
		return false
	}
	pubKey := types.PeerPublicKey(envelope.PubKey)
	return crypto.VerifySignature(pubKey, envelope.Data, envelope.Signature)
}

func (s *Swarm) AddPotentialPeer(id types.PeerID, addr string) {
	s.mu.Lock()
	_, connected := s.activePeers[id]
	s.mu.Unlock()

	if connected {
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Info("Swarm: Attempting automatic local connection", "addr", addr)
			ctx, cancel := context.WithTimeout(s.ctx, time.Second*5)
			defer cancel()
			s.expectPeer(addr, id)
			err := s.dialEarly(ctx, addr)
			if err != nil {
				s.logger.Debug("Swarm: Failed to dial discovered peer", "addr", addr, "err", err)
			}
		}
	}()
}

func (s *Swarm) AddPotentialPeerWithCtx(ctx context.Context, id types.PeerID, addr string) error {
	s.mu.Lock()
	_, connected := s.activePeers[id]
	s.mu.Unlock()

	if connected {
		return nil
	}

	slog.Info("Swarm: Dialing peer...", "id", id, "addr", addr)
	s.expectPeer(addr, id)
	err := s.dialEarly(ctx, addr)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.New("timeout waiting for peer registration in swarm")
		case <-ticker.C:
			s.mu.RLock()
			_, active := s.activePeers[id]
			s.mu.RUnlock()
			if active {
				return nil
			}
		}
	}
}

func (s *Swarm) SendDataWithRequest(peerID types.PeerID, msgType net_types.MessageType, data []byte) (net_types.MessageType, []byte, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()
	if ok {
		return peer.transport.SendDataWithRequest(msgType, data)
	}
	return 0, nil, errors.New("peer not connected")
}

func (s *Swarm) SendMessageWithRequest(peerID types.PeerID, msgType net_types.MessageType, msg *internal_pb.MessageData) (net_types.MessageType, *internal_pb.MessageData, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()

	if ok {
		env, err := s.SignMessageData(msg)
		if err != nil {
			return 0, nil, err
		}

		var envBytes []byte
		envBytes, err = proto.Marshal(env)
		if err != nil {
			return 0, nil, err
		}

		var respType net_types.MessageType
		var respMsg []byte
		respType, respMsg, err = peer.transport.SendDataWithRequest(msgType, envBytes)
		if err != nil {
			return 0, nil, err
		}

		var respEnvelope internal_pb.Envelope
		err = proto.Unmarshal(respMsg, &respEnvelope)
		if err != nil {
			return 0, nil, err
		}

		if !s.CheckSign(&respEnvelope) {
			return 0, nil, errors.New("invalid signature")
		}

		var msgResp internal_pb.MessageData
		err = proto.Unmarshal(respEnvelope.Data, &msgResp)
		if err != nil {
			return 0, nil, err
		}

		return respType, &msgResp, nil
	}
	return 0, nil, errors.New("peer not connected")
}

type StoredBundleRecord struct {
	PeerID    types.PeerID        `json:"peer_id"`
	PubKey    types.PeerPublicKey `json:"pub_key"`
	BundleRaw []byte              `json:"bundle_raw"`
}

// BundleFetcher looks a value up in the network (the DHT): the key bundles of peers that are not
// connected, which is the case for the first message to somebody behind another NAT.
type BundleFetcher func(key []byte) ([]byte, error)

func (s *Swarm) SetBundleFetcher(f BundleFetcher) {
	s.mu.Lock()
	s.fetchBundle = f
	s.mu.Unlock()
}

// The proto bundle has one field for one-time prekeys; several are stored back to back in it.
func joinOPKs(keys [][32]byte) []byte {
	out := make([]byte, 0, 32*len(keys))
	for _, k := range keys {
		out = append(out, k[:]...)
	}
	return out
}

func splitOPKs(raw []byte) ([][32]byte, error) {
	if len(raw)%32 != 0 || len(raw)/32 > keymanager.MaxBundleOPKs {
		return nil, errors.New("bad one-time prekey list")
	}
	out := make([][32]byte, len(raw)/32)
	for i := range out {
		copy(out[i][:], raw[i*32:])
	}
	return out, nil
}

// ForgetBundle drops the cached bundle of a peer, so the next session starts from a fresh one.
func (s *Swarm) ForgetBundle(id types.PeerID) {
	_ = s.storage.Delete(s.bundleCacheKey(id))
}

// bundleCacheKey is where we keep the bundle of a peer we wrote to; the peer is blinded in it.
func (s *Swarm) bundleCacheKey(id types.PeerID) []byte {
	return []byte("bcache:" + storage.BlindID(s.storage, id.String()))
}

// CacheBundle keeps a bundle record (as CreateMyPreKeyBundle makes it) of a peer, for sessions
// with it while it cannot be reached.
func (s *Swarm) CacheBundle(id types.PeerID, record []byte) error {
	if _, err := s.bundleFromRecord(id, record); err != nil {
		return err
	}
	return s.storage.Set(s.bundleCacheKey(id), record)
}

// BundleKey is where a peer's key bundle lives in the DHT.
func BundleKey(id types.PeerID) []byte { return []byte("bundle:" + id.String()) }

// bundleFromRecord checks a stored bundle record: the key must belong to the peer and the bundle
// must be signed by it.
func (s *Swarm) bundleFromRecord(peerID types.PeerID, data []byte) (*keymanager.PublicBundle, error) {
	var record StoredBundleRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if types.PeerPubKeyToID(record.PubKey) != peerID {
		return nil, errors.New("the bundle does not belong to this peer")
	}
	var protoBundle internal_pb.PreKeyBundle
	if err := proto.Unmarshal(record.BundleRaw, &protoBundle); err != nil {
		return nil, err
	}
	return s.parseAndVerifyProtoBundle(record.PubKey, &protoBundle)
}

func (s *Swarm) GetBundle(peerID types.PeerID) (*keymanager.PublicBundle, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()

	if !ok {
		// Fallback: check local storage cache if peer is not currently connected
		if data, err := s.storage.Get(s.bundleCacheKey(peerID)); err == nil && len(data) > 0 {
			if bundle, err := s.bundleFromRecord(peerID, data); err == nil {
				return bundle, nil
			}
			// expired or damaged: ask the network for a fresh one
		}
		// not connected and not cached: ask the network, the peer published its bundle there
		s.mu.RLock()
		fetch := s.fetchBundle
		s.mu.RUnlock()
		if fetch != nil {
			if data, err := fetch(BundleKey(peerID)); err == nil && len(data) > 0 {
				if bundle, err := s.bundleFromRecord(peerID, data); err == nil {
					_ = s.storage.Set(s.bundleCacheKey(peerID), data)
					return bundle, nil
				}
			}
		}
		return nil, errors.New("peer not connected and bundle not found in storage or in the network")
	}

	resType, resData, err := peer.transport.SendDataWithRequest(
		net_types.TypeLocalPublicKeyBundleRequest,
		nil,
	)
	if err != nil {
		return nil, err
	}

	if resType != net_types.TypeLocalPublicKeyBundleResponse {
		return nil, errors.New("unexpected response type for bundle request")
	}
	var protoBundle internal_pb.PreKeyBundle
	if err = proto.Unmarshal(resData, &protoBundle); err != nil {
		return nil, err
	}

	// Cache bundle in storage for offline/relay use
	record := StoredBundleRecord{
		PeerID:    peerID,
		PubKey:    peer.PubKey(),
		BundleRaw: resData,
	}
	if recBytes, err := json.Marshal(record); err == nil {
		_ = s.storage.Set(s.bundleCacheKey(peerID), recBytes)
	}

	return s.parseAndVerifyProtoBundle(peer.PubKey(), &protoBundle)
}

func (s *Swarm) parseAndVerifyProtoBundle(pubKey types.PeerPublicKey, protoBundle *internal_pb.PreKeyBundle) (*keymanager.PublicBundle, error) {
	var idKey, spkKey [32]byte
	var sig [64]byte

	copy(idKey[:], protoBundle.IdentityKey)
	copy(spkKey[:], protoBundle.SignedPreKey)
	copy(sig[:], protoBundle.Signature)
	opks, err := splitOPKs(protoBundle.OneTimePreKey)
	if err != nil {
		return nil, err
	}

	var peerKey [32]byte
	copy(peerKey[:], pubKey[:])

	bundle := &keymanager.PublicBundle{
		PeerID:       peerKey,
		IdentityKey:  idKey,
		SignedPreKey: spkKey,
		Signature:    sig,
		OneTimeKeys:  opks,
		CreatedAt:    protoBundle.CreatedAt,
	}

	if err := keymanager.VerifyBundle(bundle); err != nil {
		return nil, errors.New("received invalid or forged bundle: " + err.Error())
	}

	return bundle, nil
}

// IdentityCard is our signed bundle record without one-time prekeys. An init packet carries it, so
// the receiver can check our identity key without looking us up. It is made again once a day.
func (s *Swarm) IdentityCard() ([]byte, error) {
	s.cardMu.Lock()
	defer s.cardMu.Unlock()
	if s.card != nil && time.Since(s.cardAge) < 24*time.Hour {
		return s.card, nil
	}
	account, err := s.identity.GetActiveAccount()
	if err != nil {
		return nil, err
	}
	bundle, err := account.CreateMyBundle(nil)
	if err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(&internal_pb.PreKeyBundle{
		IdentityKey:  bundle.IdentityKey[:],
		SignedPreKey: bundle.SignedPreKey[:],
		Signature:    bundle.Signature[:],
		CreatedAt:    bundle.CreatedAt,
	})
	if err != nil {
		return nil, err
	}
	card, err := json.Marshal(StoredBundleRecord{PeerID: s.selfID, PubKey: s.GetMyPubKey(), BundleRaw: raw})
	if err != nil {
		return nil, err
	}
	s.card, s.cardAge = card, time.Now()
	return card, nil
}

// BundleFromRecord checks a bundle record that claims to be peerID's: the key must hash to peerID,
// the bundle must be signed by it and not expired.
func (s *Swarm) BundleFromRecord(peerID types.PeerID, record []byte) (*keymanager.PublicBundle, error) {
	return s.bundleFromRecord(peerID, record)
}

func (s *Swarm) CreateMyPreKeyBundle() ([]byte, error) {
	account, err := s.identity.GetActiveAccount()
	if err != nil {
		return nil, err
	}

	// published for everyone, so without one-time prekeys: peers starting sessions at the same
	// time would pick the same one, and only the first would get through. A one-time prekey is
	// handed out only on a direct request, to that peer alone.
	bundle, err := account.CreateMyBundle(nil)
	if err != nil {
		return nil, err
	}

	protoBundle := &internal_pb.PreKeyBundle{
		IdentityKey:   bundle.IdentityKey[:],
		SignedPreKey:  bundle.SignedPreKey[:],
		Signature:     bundle.Signature[:],
		OneTimePreKey: joinOPKs(bundle.OneTimeKeys),
		CreatedAt:     bundle.CreatedAt,
	}

	rawProto, err := proto.Marshal(protoBundle)
	if err != nil {
		return nil, err
	}

	record := StoredBundleRecord{
		PeerID:    s.selfID,
		PubKey:    s.selfPubKey,
		BundleRaw: rawProto,
	}

	return json.Marshal(record)
}

func (s *Swarm) GetDispatcher() *dispatcher.Dispatcher {
	return s.dispatcher
}

func (s *Swarm) GetContext() context.Context {
	return s.ctx
}

func (s *Swarm) GetSelfID() types.PeerID {
	return s.selfID
}

func (s *Swarm) GetMyPubKey() types.PeerPublicKey {
	if s.selfPubKey != (types.PeerPublicKey{}) {
		return s.selfPubKey
	}
	if s.identity != nil {
		key, _ := s.identity.GetPublicKey()
		copy(s.selfPubKey[:], key)
	}
	return s.selfPubKey
}

func (s *Swarm) PingPeer(id types.PeerID, lastAddr string) (time.Duration, error) {
	s.mu.RLock()
	_, connected := s.activePeers[id]
	s.mu.RUnlock()

	start := time.Now()

	if connected {
		pingData := &internal_pb.MessageData{
			MessageId: types.NewMessageID(),
			OriginId:  s.selfID[:],
			Payload: &internal_pb.MessageData_Ping{
				Ping: &internal_pb.Ping{Nonce: start.UnixNano()},
			},
		}

		env, err := s.SignMessageData(pingData)
		if err != nil {
			return 0, err
		}

		envBytes, err := proto.Marshal(env)
		if err != nil {
			return 0, err
		}

		_, _, err = s.SendDataWithRequest(id, net_types.TypePing, envBytes)
		if err != nil {
			return 0, err
		}
		return time.Since(start), nil
	}

	if lastAddr == "" {
		return 0, errors.New("peer not connected and no address provided")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()

	s.expectPeer(lastAddr, id)
	err := s.netTransport.DialEarly(ctx, lastAddr)
	if err != nil {
		return 0, fmt.Errorf("peer unreachable at %s: %w", lastAddr, err)
	}

	return time.Since(start), nil
}

func (s *Swarm) AddObserver(o TopologyObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, o)
}

// Bootstrap dials the given nodes. With obfuscation on each entry must name the node:
// "host:port/<peer id>" (network.JoinPeerAddr).
// Bootstrap dials the given nodes ("host:port/peerid"; the host may be a name, so a seed node can
// move to another IP without a new release).
func (s *Swarm) Bootstrap(addrs []string) {
	for _, entry := range addrs {
		if entry == "" {
			continue
		}
		addr, id, named := network.SplitPeerAddr(entry)

		s.wg.Add(1)
		go func(a string) {
			defer s.wg.Done()
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			// obfuscation keys are bound to the address packets come from: an IP, not a name
			a = resolveHostPort(s.ctx, a)
			if named {
				s.expectPeer(a, id)
			}
			s.connect(a)
		}(addr)
	}
}

// resolveHostPort turns "name:port" into "ip:port" (IPv4 first); an IP or an unresolvable name is
// returned as it is.
func resolveHostPort(ctx context.Context, addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return addr
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return addr
	}
	pick := ips[0].IP
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			pick = ip.IP
			break
		}
	}
	return net.JoinHostPort(pick.String(), port)
}

// Rejoin is for a node that has lost every peer (it started while offline, or its network was
// cut): it dials the bootstrap nodes and the peers it was connected to before. It does nothing
// while any peer is connected. Returns whether there was anybody to dial.
func (s *Swarm) Rejoin(bootstrap []string) bool {
	if len(s.GetAllPeers()) > 0 {
		return false
	}
	s.Bootstrap(bootstrap)
	tried := len(bootstrap) > 0
	if s.peerStore != nil {
		for _, e := range s.peerStore.GetHistoryConnected(16) {
			pub, err := types.ToPeerPubKey(e.GetPubKey())
			if err != nil || e.GetLastKnownAddr() == "" {
				continue
			}
			s.AddPotentialPeer(types.PeerPubKeyToID(pub), e.GetLastKnownAddr())
			tried = true
		}
	}
	return tried
}

func (s *Swarm) CreateSignature(dataForSign []byte) ([]byte, error) {
	return s.identity.SignData(dataForSign)
}

func (s *Swarm) PruneInactiveSessions(maxIdle time.Duration) int {
	if s.sessionManager != nil {
		return s.sessionManager.PruneInactive(maxIdle)
	}
	return 0
}

// SetEmitter attaches the node event bus for network telemetry
func (s *Swarm) SetEmitter(e events.Emitter) {
	s.mu.Lock()
	s.emitter = e
	s.mu.Unlock()
}

func (s *Swarm) emit(topic string, payload any) {
	s.mu.RLock()
	e := s.emitter
	s.mu.RUnlock()
	if e != nil {
		e.Emit(topic, payload)
	}
}

func (s *Swarm) TrafficStats() (rx uint64, tx uint64) {
	return s.bytesRx.Load(), s.bytesTx.Load()
}
