package swarm

import (
	"errors"
	"log/slog"

	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

type RPCServiceFunc func(msg *internal_pb.MessageData, peerID types.PeerID) (net_types.MessageType, *internal_pb.MessageData, error)

type FrameHandler func(msgType net_types.MessageType, payload []byte, peerID types.PeerID) bool

// FastFrameHandler handles one MessageType on the fast path, without decoding an envelope.
type FastFrameHandler func(payload []byte, peerID types.PeerID) error

// StreamHandler takes ownership of an incoming bidirectional stream after its first frame was read
type StreamHandler func(stream *network.Stream, payload []byte, peerID types.PeerID)

// RegisterRPC sets the handler that answers request/response calls of this message type.
func (s *Swarm) RegisterRPC(msgType net_types.MessageType, handler RPCServiceFunc) {
	s.rpcMu.Lock()
	defer s.rpcMu.Unlock()

	if s.rpcHandlers == nil {
		s.rpcHandlers = make(map[net_types.MessageType]RPCServiceFunc)
	}

	s.rpcHandlers[msgType] = handler
}

// RegisterFastFrameHandler sets the fast-path handler for a MessageType (media and other
// high-rate frames that skip the normal envelope path).
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

// AddFrameHandler adds a handler offered every incoming frame; it returns true once one consumes it.
func (s *Swarm) AddFrameHandler(handler FrameHandler) {
	s.frameHandlersMu.Lock()
	defer s.frameHandlersMu.Unlock()
	s.frameHandlers = append(s.frameHandlers, handler)
}

func (s *Swarm) initCoreRPCs() {
	s.RegisterRPC(net_types.TypeDAGSessionInitRequest, s.sessionManager.HandleDagSessionReq)
	s.RegisterRPC(net_types.TypePing, s.handlePing)
	s.RegisterRPC(net_types.TypeGetPeerRequest, s.handlePeerExchange)
	s.initCoreFastHandlers()
}

// handlePing answers a liveness check (the DHT asks before it evicts an old peer).
func (s *Swarm) handlePing(msg *internal_pb.MessageData, _ types.PeerID) (net_types.MessageType, *internal_pb.MessageData, error) {
	ping := msg.GetPing()
	if ping == nil {
		return 0, nil, errors.New("ping: no payload")
	}
	return net_types.TypePong, &internal_pb.MessageData{
		MessageId: msg.MessageId,
		OriginId:  s.selfID[:],
		Payload:   &internal_pb.MessageData_Pong{Pong: &internal_pb.Pong{Nonce: ping.Nonce}},
	}, nil
}

func (s *Swarm) initCoreFastHandlers() {
	s.RegisterFastFrameHandler(net_types.TypeTransportMigrateRequest, s.handleTransportMigrateRequest)
	s.RegisterFastFrameHandler(net_types.TypeTransportMigrateAck, s.handleTransportMigrateAck)
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

// GetPeerProtocol reports which transport (QUIC or TCP) a peer is currently connected over.
func (s *Swarm) GetPeerProtocol(peerID types.PeerID) router.TransportProtocol {
	p := s.GetPeer(peerID)
	if p == nil {
		return 0
	}
	return p.ActiveProtocol()
}

// GetAllPeersProtocols returns the current transport of every connected peer.
func (s *Swarm) GetAllPeersProtocols() map[types.PeerID]router.TransportProtocol {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make(map[types.PeerID]router.TransportProtocol, len(s.activePeers))
	for id, p := range s.activePeers {
		res[id] = p.ActiveProtocol()
	}
	return res
}

// MigratePeerTransport asks a peer to move its connection to another transport link (e.g. QUIC to TCP).
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
