package routing

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// Circuit is one open two-way tunnel the relay forwards between two peers.
type Circuit struct {
	ID          uint32
	SignalOnly  bool // opened for hole punching messages only
	PeerA       types.PeerID
	PeerB       types.PeerID
	CreatedAt   time.Time
	LastActive  atomic.Int64 // unix nanoseconds; frames in both directions update it concurrently
	BytesRouted uint64
}

type RelayServer struct {
	swarm   *swarm.Swarm
	mailbox *messenger.MailboxManager
	logger  *slog.Logger

	cfgMu   sync.RWMutex
	cfg     config.RelayServerConfig
	allowed map[types.PeerID]bool // when cfg.Private: the peers allowed to use this relay

	circuitsMu sync.RWMutex
	circuits   map[uint32]*Circuit

	// totals since the node started, for the operator
	circuitsOpened atomic.Uint64
	bytesRouted    atomic.Uint64
	forwarded      atomic.Uint64 // messages passed on to their recipient or the next relay
	stored         atomic.Uint64 // messages kept in the mailbox for an offline recipient
	delivered      atomic.Uint64 // mailbox messages handed over when the recipient came back
	signalRefused  atomic.Uint64 // frames a signaling circuit would not carry
	refusedMsgs    atomic.Uint64 // messages a signaling node does not relay or keep
	accessRefused  atomic.Uint64 // requests from peers not allowed on a private relay
}

// Limits of a signaling circuit: the hole punching exchange is a few small messages, so anything
// beyond them is traffic, which a signaling node does not carry.
const (
	signalCircuitBytes = 64 << 10
	signalCircuitLife  = 2 * time.Minute
)

// Circuit resource bounds. maxCircuitsPerPeer stops one peer from opening the whole circuit table
// on its own (the MaxCircuits config is a node-wide cap; this is per initiator). idleCircuitTimeout
// is how long a circuit may carry no frames before it is reaped, so abandoned circuits (and ones
// opened only to occupy a slot) do not linger.
const (
	maxCircuitsPerPeer = 16
	idleCircuitTimeout = 5 * time.Minute
	circuitReapEvery   = 1 * time.Minute
)

// signalFrameAllowed: hole punching messages only, within the circuit's byte and time budget.
func signalFrameAllowed(c *Circuit, frame []byte) bool {
	if len(frame) < 5 || net_types.MessageType(frame[4]) != net_types.TypeHolePunch {
		return false
	}
	if time.Since(c.CreatedAt) > signalCircuitLife {
		return false
	}
	return atomic.LoadUint64(&c.BytesRouted)+uint64(len(frame)) <= signalCircuitBytes
}

// RelayStats is what this node did for others since it started.
type RelayStats struct {
	ActiveCircuits  int    `json:"activeCircuits"`
	CircuitsOpened  uint64 `json:"circuitsOpened"`
	BytesRouted     uint64 `json:"bytesRouted"`    // through circuits (calls, tunnels between peers behind NATs)
	Forwarded       uint64 `json:"forwarded"`      // messages passed on
	Stored          uint64 `json:"stored"`         // messages kept for offline peers
	Delivered       uint64 `json:"delivered"`      // kept messages handed over later
	MailboxWaiting  int    `json:"mailboxWaiting"` // kept messages still waiting
	MaxCircuits     int    `json:"maxCircuits"`
	Enabled         bool   `json:"enabled"`
	SignalingOnly   bool   `json:"signalingOnly"`
	SignalRefused   uint64 `json:"signalRefused"` // frames a signaling circuit would not carry
	RefusedMessages uint64 `json:"refusedMessages"`
	Private         bool   `json:"private"`
	AllowedPeers    int    `json:"allowedPeers"`  // size of the allow list on a private relay
	AccessRefused   uint64 `json:"accessRefused"` // requests from peers not on the allow list
}

// Stats returns the relay totals.
func (rs *RelayServer) Stats() RelayStats {
	rs.circuitsMu.RLock()
	active := len(rs.circuits)
	rs.circuitsMu.RUnlock()
	rs.cfgMu.RLock()
	cfg := rs.cfg
	allowedCount := len(rs.allowed)
	rs.cfgMu.RUnlock()
	st := RelayStats{
		ActiveCircuits:  active,
		CircuitsOpened:  rs.circuitsOpened.Load(),
		BytesRouted:     rs.bytesRouted.Load(),
		Forwarded:       rs.forwarded.Load(),
		Stored:          rs.stored.Load(),
		Delivered:       rs.delivered.Load(),
		MaxCircuits:     cfg.MaxCircuits,
		Enabled:         cfg.Enabled,
		SignalingOnly:   cfg.SignalingOnly,
		SignalRefused:   rs.signalRefused.Load(),
		RefusedMessages: rs.refusedMsgs.Load(),
		Private:         cfg.Private,
		AllowedPeers:    allowedCount,
		AccessRefused:   rs.accessRefused.Load(),
	}
	if rs.mailbox != nil {
		st.MailboxWaiting = rs.mailbox.Waiting()
	}
	return st
}

func NewRelayServer(s *swarm.Swarm, mailbox *messenger.MailboxManager, logger *slog.Logger) *RelayServer {
	if logger == nil {
		logger = slog.Default().With("component", "relay_server")
	}
	rs := &RelayServer{
		swarm:    s,
		mailbox:  mailbox,
		logger:   logger,
		circuits: make(map[uint32]*Circuit),
		cfg: config.RelayServerConfig{
			Enabled:     true,
			PricePerMB:  0,
			MaxCircuits: 50,
		},
	}
	if s != nil {
		s.AddObserver(rs)
		s.AddFrameHandler(rs.HandleRawFrame)
	}
	return rs
}

func (rs *RelayServer) SetPolicy(cfg config.RelayServerConfig) {
	rs.cfgMu.Lock()
	defer rs.cfgMu.Unlock()
	rs.cfg = cfg
	rs.allowed = make(map[types.PeerID]bool, len(cfg.AllowedPeers))
	for _, s := range cfg.AllowedPeers {
		if id, err := types.PeerIDFromString(s); err == nil {
			rs.allowed[id] = true
		} else {
			rs.logger.Warn("RelayServer: ignoring malformed allowed peer id", "id", s)
		}
	}
	rs.logger.Info("RelayServer: Dynamic policy updated",
		"enabled", cfg.Enabled, "private", cfg.Private, "allowed", len(rs.allowed), "max_circuits", cfg.MaxCircuits)
}

// servesPeer reports whether this relay will work for a given peer: always on a public relay, and
// only for the allowed peers on a private one.
func (rs *RelayServer) servesPeer(id types.PeerID) bool {
	rs.cfgMu.RLock()
	defer rs.cfgMu.RUnlock()
	if !rs.cfg.Private {
		return true
	}
	return rs.allowed[id]
}

func (rs *RelayServer) GetPolicy() config.RelayServerConfig {
	rs.cfgMu.RLock()
	defer rs.cfgMu.RUnlock()
	return rs.cfg
}

func (rs *RelayServer) Name() string {
	return "RelayServer"
}

func (rs *RelayServer) Start(ctx context.Context) error {
	go rs.reapLoop(ctx)
	return nil
}

// reapLoop periodically drops circuits that have carried no frames for idleCircuitTimeout, so
// abandoned circuits (and ones opened only to hold a slot) free their place in the table.
func (rs *RelayServer) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(circuitReapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rs.reapIdleCircuits(idleCircuitTimeout)
		}
	}
}

// reapIdleCircuits removes every circuit whose last activity is older than maxIdle and notifies the
// still-connected participants. It returns how many circuits were reaped.
func (rs *RelayServer) reapIdleCircuits(maxIdle time.Duration) int {
	cutoff := time.Now().Add(-maxIdle).UnixNano()

	type closed struct {
		cid   uint32
		peerA types.PeerID
		peerB types.PeerID
	}
	var toClose []closed

	rs.circuitsMu.Lock()
	for cid, c := range rs.circuits {
		last := c.LastActive.Load()
		if last == 0 {
			last = c.CreatedAt.UnixNano()
		}
		if last < cutoff {
			delete(rs.circuits, cid)
			toClose = append(toClose, closed{cid: cid, peerA: c.PeerA, peerB: c.PeerB})
		}
	}
	rs.circuitsMu.Unlock()

	for _, c := range toClose {
		closePkt := make([]byte, 4)
		binary.BigEndian.PutUint32(closePkt, c.cid)
		if rs.swarm != nil {
			if p := rs.swarm.GetPeer(c.peerA); p != nil {
				_ = p.SendRaw(net_types.TypeCircuitClose, closePkt)
			}
			if p := rs.swarm.GetPeer(c.peerB); p != nil {
				_ = p.SendRaw(net_types.TypeCircuitClose, closePkt)
			}
		}
	}
	if len(toClose) > 0 {
		rs.logger.Info("RelayServer: reaped idle circuits", "count", len(toClose))
	}
	return len(toClose)
}

// circuitsForPeerLocked counts the circuits a given peer initiated (is PeerA of). Caller holds circuitsMu.
func (rs *RelayServer) circuitsForPeerLocked(peer types.PeerID) int {
	n := 0
	for _, c := range rs.circuits {
		if c.PeerA == peer {
			n++
		}
	}
	return n
}

func (rs *RelayServer) Stop() error {
	return nil
}

func (rs *RelayServer) GetSubscribedTypes() []interface{} {
	return []interface{}{
		&internal_pb.MessageData_RelayReq{},
	}
}

// HandleRawFrame handles the circuit frames (open, data, close) this node relays, returning
// whether the frame was one of them.
func (rs *RelayServer) HandleRawFrame(msgType net_types.MessageType, payload []byte, fromPeer types.PeerID) bool {
	switch msgType {
	case net_types.TypeCircuitOpenRequest:
		// Only handle if this is a request to act as relay: the 32-byte target peer ID, plus one
		// byte for a signaling circuit. 36 and 37 bytes are inbound circuit notifications meant
		// for RelayClient.
		if len(payload) == 32 || len(payload) == 33 {
			rs.handleCircuitOpenReq(payload, fromPeer)
			return true
		}
		return false

	case net_types.TypeCircuitData:
		if len(payload) >= 4 {
			cid := binary.BigEndian.Uint32(payload[:4])
			rs.circuitsMu.RLock()
			_, exists := rs.circuits[cid]
			rs.circuitsMu.RUnlock()
			if exists {
				rs.handleCircuitData(payload, fromPeer)
				return true
			}
		}
		return false // Let RelayClient handle it if this node is the circuit endpoint

	case net_types.TypeCircuitClose:
		if len(payload) >= 4 {
			cid := binary.BigEndian.Uint32(payload[:4])
			rs.circuitsMu.RLock()
			_, exists := rs.circuits[cid]
			rs.circuitsMu.RUnlock()
			if exists {
				rs.handleCircuitClose(payload, fromPeer)
				return true
			}
		}
		return false

	default:
		return false
	}
}

// handleCircuitOpenReq processes a request to establish a virtual circuit tunnel to a target peer.
func (rs *RelayServer) handleCircuitOpenReq(payload []byte, fromPeer types.PeerID) {
	if len(payload) < 32 {
		rs.logger.Warn("RelayServer: malformed TypeCircuitOpenRequest (too short)")
		return
	}

	rs.cfgMu.RLock()
	cfg := rs.cfg
	rs.cfgMu.RUnlock()

	// a client asks for a signaling circuit with one more byte after the target
	signal := len(payload) == 33 && payload[32] == 1

	// 1. Check if relay server is enabled (a signaling node takes signaling circuits only)
	if !cfg.Enabled || (cfg.SignalingOnly && !signal) {
		resp := make([]byte, 5)
		resp[4] = 0 // rejected: relay server disabled
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.logger.Debug("RelayServer: CircuitOpen rejected: relay server is disabled", "from", fromPeer.String()[:8])
		return
	}

	// A private relay serves only the peers its operator allowed.
	if !rs.servesPeer(fromPeer) {
		rs.accessRefused.Add(1)
		resp := make([]byte, 5)
		resp[4] = 0 // rejected: not on the allow list
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.logger.Debug("RelayServer: CircuitOpen rejected: peer not allowed on this private relay", "from", fromPeer.String()[:8])
		return
	}

	// 2. Check active circuits quota (DoS protection): node-wide, and per initiating peer.
	rs.circuitsMu.RLock()
	currentCircuits := len(rs.circuits)
	perPeer := rs.circuitsForPeerLocked(fromPeer)
	rs.circuitsMu.RUnlock()

	if cfg.MaxCircuits > 0 && currentCircuits >= cfg.MaxCircuits {
		resp := make([]byte, 5)
		resp[4] = 0 // rejected: max circuits quota reached
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.logger.Warn("RelayServer: CircuitOpen rejected: circuit quota full",
			"active", currentCircuits, "max", cfg.MaxCircuits, "from", fromPeer.String()[:8])
		return
	}

	if perPeer >= maxCircuitsPerPeer {
		resp := make([]byte, 5)
		resp[4] = 0 // rejected: this peer already holds its share of circuits
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.logger.Warn("RelayServer: CircuitOpen rejected: per-peer circuit cap reached",
			"peer", fromPeer.String()[:8], "held", perPeer, "cap", maxCircuitsPerPeer)
		return
	}

	targetID, err := types.ToPeerID(payload[:32])
	if err != nil {
		rs.logger.Warn("RelayServer: invalid target peer ID in CircuitOpen", "err", err)
		return
	}

	// A circuit to ourselves makes no sense and would let a peer loop frames back through us.
	if targetID == rs.swarm.GetSelfID() {
		resp := make([]byte, 5)
		resp[4] = 0 // rejected: target is this relay
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.logger.Debug("RelayServer: CircuitOpen rejected: target is self", "from", fromPeer.String()[:8])
		return
	}

	targetPeer := rs.swarm.GetPeer(targetID)
	if targetPeer == nil {
		// Target is not directly connected to this relay node
		resp := make([]byte, 5) // CircuitID(4) + Status(1)
		resp[4] = 0             // 0 = Failed / Target not connected
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.logger.Debug("RelayServer: CircuitOpen rejected: target peer not connected",
			"target", targetID.String()[:8], "from", fromPeer.String()[:8])
		return
	}

	// Generate a unique non-zero CircuitID
	var cidBytes [4]byte
	_, _ = rand.Read(cidBytes[:])
	cid := binary.BigEndian.Uint32(cidBytes[:])
	if cid == 0 {
		cid = 1
	}

	circuit := &Circuit{
		ID:         cid,
		PeerA:      fromPeer,
		PeerB:      targetID,
		CreatedAt:  time.Now(),
		SignalOnly: signal || cfg.SignalingOnly,
	}

	circuit.LastActive.Store(time.Now().UnixNano())
	rs.circuitsMu.Lock()
	rs.circuits[cid] = circuit
	rs.circuitsMu.Unlock()
	rs.circuitsOpened.Add(1)

	// 1. Notify TargetPeer (PeerB): [CircuitID: 4B] [OriginID: 32B] [signal only: 1B]
	notifyTarget := make([]byte, 37)
	binary.BigEndian.PutUint32(notifyTarget[:4], cid)
	copy(notifyTarget[4:], fromPeer[:])
	if circuit.SignalOnly {
		notifyTarget[36] = 1
	}
	_ = targetPeer.SendRaw(net_types.TypeCircuitOpenRequest, notifyTarget)

	// 2. Respond to Initiator (PeerA): [CircuitID: 4B] [Status: 1] [TargetID: 32B]
	respOrigin := make([]byte, 37)
	binary.BigEndian.PutUint32(respOrigin[:4], cid)
	respOrigin[4] = 1 // 1 = Success
	copy(respOrigin[5:], targetID[:])
	_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, respOrigin)

	rs.logger.Info("RelayServer: Virtual Circuit established",
		"circuitID", cid, "peerA", fromPeer.String()[:8], "peerB", targetID.String()[:8])
}

// handleCircuitData achieves Zero-Copy Stream Splice: forwards raw packet directly
// to the destination socket without Protobuf unmarshaling or heap allocations.
func (rs *RelayServer) handleCircuitData(payload []byte, fromPeer types.PeerID) {
	if len(payload) < 5 { // CircuitID (4B) + InnerMsgType (1B) + InnerPayload (N B)
		return
	}

	cid := binary.BigEndian.Uint32(payload[:4])

	rs.circuitsMu.RLock()
	circuit, exists := rs.circuits[cid]
	rs.circuitsMu.RUnlock()

	if !exists {
		// Circuit does not exist or expired: send close back to sender
		closePkt := make([]byte, 4)
		binary.BigEndian.PutUint32(closePkt, cid)
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitClose, closePkt)
		return
	}

	if circuit.SignalOnly && !signalFrameAllowed(circuit, payload) {
		rs.signalRefused.Add(1)
		return
	}

	var destID types.PeerID
	if fromPeer == circuit.PeerA {
		destID = circuit.PeerB
	} else if fromPeer == circuit.PeerB {
		destID = circuit.PeerA
	} else {
		return
	}

	destPeer := rs.swarm.GetPeer(destID)
	if destPeer == nil {
		closePkt := make([]byte, 4)
		binary.BigEndian.PutUint32(closePkt, cid)
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitClose, closePkt)
		return
	}

	// Zero-copy passthrough of the circuit data frame!
	_ = destPeer.SendRaw(net_types.TypeCircuitData, payload)

	circuit.LastActive.Store(time.Now().UnixNano())
	atomic.AddUint64(&circuit.BytesRouted, uint64(len(payload)))
	rs.bytesRouted.Add(uint64(len(payload)))
}

// handleCircuitClose removes the circuit and informs the remaining peer.
func (rs *RelayServer) handleCircuitClose(payload []byte, fromPeer types.PeerID) {
	if len(payload) < 4 {
		return
	}
	cid := binary.BigEndian.Uint32(payload[:4])

	rs.circuitsMu.Lock()
	circuit, exists := rs.circuits[cid]
	// Only a participant of the circuit may close it; a close from any other peer is ignored so it
	// cannot tear down a circuit it is not part of.
	if exists && fromPeer != circuit.PeerA && fromPeer != circuit.PeerB {
		rs.circuitsMu.Unlock()
		return
	}
	if exists {
		delete(rs.circuits, cid)
	}
	rs.circuitsMu.Unlock()

	if exists {
		var otherID types.PeerID
		if fromPeer == circuit.PeerA {
			otherID = circuit.PeerB
		} else {
			otherID = circuit.PeerA
		}
		if otherPeer := rs.swarm.GetPeer(otherID); otherPeer != nil {
			_ = otherPeer.SendRaw(net_types.TypeCircuitClose, payload)
		}
		rs.logger.Info("RelayServer: Virtual Circuit closed", "circuitID", cid)
	}
}

func (rs *RelayServer) sendRawFrame(peerID types.PeerID, msgType net_types.MessageType, data []byte) error {
	if rs.swarm == nil {
		return errors.New("no swarm")
	}
	p := rs.swarm.GetPeer(peerID)
	if p == nil {
		return errors.New("peer not found")
	}
	return p.SendRaw(msgType, data)
}

// Handle processes multi-hop and offline store-and-forward envelopes.
func (rs *RelayServer) Handle(msg *internal_pb.MessageData, fromPeer types.PeerID) {
	req := msg.GetRelayReq()
	if req == nil {
		rs.logger.Warn("RelayServer: received empty RelayReq")
		return
	}

	targetID, err := types.ToPeerID(req.TargetPeerId)
	if err != nil {
		rs.logger.Warn("RelayServer: invalid target peer ID", "err", err)
		return
	}

	// The peer we work for is the signed origin of this request, not the transport sender: the
	// dispatcher has already verified the envelope signature and that OriginId is the signer, so
	// OriginId is an authenticated identity, whereas fromPeer can be a circuit's spoofed claim.
	// Reject an empty origin (the dispatcher lets it through) so authorisation always has a subject.
	originID, oErr := types.ToPeerID(msg.OriginId)
	selfID := rs.swarm.GetSelfID()
	if targetID != selfID && (oErr != nil || originID == (types.PeerID{})) {
		rs.refusedMsgs.Add(1)
		rs.logger.Warn("RelayServer: refused relay request with no authenticated origin")
		return
	}

	rs.cfgMu.RLock()
	signalingOnly, enabled := rs.cfg.SignalingOnly, rs.cfg.Enabled
	rs.cfgMu.RUnlock()
	if targetID != selfID && (signalingOnly || !enabled) {
		// not a relay: nothing is passed on or kept for others. Clients that know pick relays
		// by what peers announce; this is for older ones.
		rs.refusedMsgs.Add(1)
		return
	}
	if targetID != selfID && !rs.servesPeer(originID) {
		// a private relay does not pass on or keep messages for peers it was not told to serve
		rs.accessRefused.Add(1)
		return
	}
	rs.logger.Info("RelayServer: received relay message",
		"from", fromPeer.String()[:8],
		"target", targetID.String()[:8],
		"self", selfID.String()[:8])

	// 1. If target is self: unwrap envelope and pass to dispatcher
	if targetID == selfID {
		var innerEnv internal_pb.Envelope
		if err = proto.Unmarshal(req.ForwardedEnvelope, &innerEnv); err == nil {
			rs.swarm.GetDispatcher().PushMessage(&innerEnv, fromPeer)
			rs.logger.Info("RelayServer: Delivered relay message to local dispatcher", "from", fromPeer.String()[:8])
		} else {
			rs.logger.Error("RelayServer: failed to unmarshal forwarded envelope", "err", err)
		}
		return
	}

	// 2. If target is directly connected to this node in Swarm: deliver immediately!
	if rs.swarm.ThisIsActivePeer(targetID) {
		var innerEnv internal_pb.Envelope
		if err = proto.Unmarshal(req.ForwardedEnvelope, &innerEnv); err == nil {
			peer := rs.swarm.GetPeer(targetID)
			if peer != nil {
				_ = peer.Send(net_types.TypeChatMessage, &innerEnv)
				rs.forwarded.Add(1)
				rs.logger.Info("RelayServer: forwarded relay message directly to target peer", "target", targetID.String()[:8])
				return
			}
		}
	}

	// 3. Multi-hop: Forward to next peer in the chain (not the one who just sent it to us).
	// The request is ours now, signed by us; the envelope inside stays the sender's.
	forwarded := false
	hop := &internal_pb.MessageData{
		MessageId: msg.MessageId,
		OriginId:  selfID.ToBytes(),
		TargetId:  msg.TargetId,
		Timestamp: msg.Timestamp,
		HopLimit:  msg.HopLimit - 1,
		Payload:   msg.Payload,
	}
	if msg.HopLimit <= 1 {
		hop = nil // travelled far enough: keep it in the mailbox below
	}
	allPeers := rs.swarm.GetAllPeers()
	for _, nextPeer := range allPeers {
		nextPeerID := nextPeer.ID()
		if hop != nil && nextPeerID != fromPeer && nextPeer.Relays() {
			relayEnv, err := rs.swarm.SignMessageData(hop)
			if err == nil {
				if err := nextPeer.Send(net_types.TypeRelayMessageRequest, relayEnv); err == nil {
					rs.logger.Info("RelayServer: forwarded relay message to next hop",
						"next", nextPeerID.String()[:8], "target", targetID.String()[:8])
					forwarded = true
					rs.forwarded.Add(1)
					break
				}
			}
		}
	}

	// 4. If target is offline (or unreachable): store in friendly Mailbox!
	if !forwarded && rs.mailbox != nil {
		msgID := hex.EncodeToString(msg.MessageId)
		if msgID == "" {
			msgID = fmt.Sprintf("relay-%d", time.Now().UnixNano())
		}

		item := messenger.MailboxItem{
			ID:          msgID,
			RecipientID: targetID,
			SenderID:    originID, // the authenticated signer, used for the per-sender quota
			Payload:     req.ForwardedEnvelope,
			CreatedAt:   time.Now().Unix(),
			ExpiresAt:   time.Now().Add(7 * 24 * time.Hour).Unix(),
		}

		if err = rs.mailbox.Store(item); err == nil {
			rs.stored.Add(1)
			rs.logger.Info("RelayServer: saved message to offline Mailbox",
				"target", targetID.String()[:8], "msgID", msgID[:8])
		} else {
			rs.logger.Warn("RelayServer: failed to store in mailbox", "err", err, "target", targetID.String()[:8])
			// Send network error back to sender peer so it searches for another relay via DHT!
			if senderPeer := rs.swarm.GetPeer(fromPeer); senderPeer != nil {
				rejMsg := &internal_pb.MessageData{
					MessageId: types.NewMessageID(),
					OriginId:  selfID.ToBytes(),
					TargetId:  fromPeer[:],
					Timestamp: uint64(time.Now().UnixNano()),
				}
				if rejEnv, rErr := rs.swarm.SignMessageData(rejMsg); rErr == nil {
					_ = senderPeer.Send(net_types.TypeMailboxFull, rejEnv)
					rs.logger.Info("RelayServer: Sent TypeMailboxFull error to sender", "sender", fromPeer.String()[:8])
				}
			}
		}
	}
}

// OnPeerConnected reacts when any peer connects to this node.
func (rs *RelayServer) OnPeerConnected(peerID types.PeerID, pubKey types.PeerPublicKey, addr string) {
	if rs.mailbox == nil {
		return
	}

	// Remember that this peer uses us, so we will accept stored messages addressed to it while it
	// is offline. We never keep messages for a recipient that has never been here (that is how a
	// stranger would otherwise fill our disk by naming random recipients).
	rs.mailbox.NoteRecipientSeen(peerID)

	pending, err := rs.mailbox.GetPending(peerID)
	if err != nil || len(pending) == 0 {
		return
	}

	peer := rs.swarm.GetPeer(peerID)
	if peer == nil {
		return
	}

	var deliveredIDs []string
	for _, item := range pending {
		var innerEnv internal_pb.Envelope
		if err := proto.Unmarshal(item.Payload, &innerEnv); err == nil {
			if err := peer.Send(net_types.TypeChatMessage, &innerEnv); err == nil {
				deliveredIDs = append(deliveredIDs, item.ID)
			}
		}
	}

	if len(deliveredIDs) > 0 {
		rs.delivered.Add(uint64(len(deliveredIDs)))
		_ = rs.mailbox.Acknowledge(peerID, deliveredIDs)
		rs.logger.Info("RelayServer: Delivered stored mailbox items to reconnected peer",
			"peer", peerID.String()[:8], "count", len(deliveredIDs))
	}
}

func (rs *RelayServer) OnPeerDisconnected(peerID types.PeerID) {
	// Clean up any circuits where this disconnected peer was a participant
	rs.circuitsMu.Lock()
	defer rs.circuitsMu.Unlock()

	for cid, c := range rs.circuits {
		if c.PeerA == peerID || c.PeerB == peerID {
			delete(rs.circuits, cid)
			otherID := c.PeerB
			if otherID == peerID {
				otherID = c.PeerA
			}
			if otherPeer := rs.swarm.GetPeer(otherID); otherPeer != nil {
				closePkt := make([]byte, 4)
				binary.BigEndian.PutUint32(closePkt, cid)
				_ = otherPeer.SendRaw(net_types.TypeCircuitClose, closePkt)
			}
		}
	}
}
