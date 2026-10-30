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
	"github.com/echofog/echofog-core/internal/ratelimit"
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

	ipKey, netKey string        // the initiator's address keys for the per-IP caps ("" when not limited)
	use           *peerUse      // the initiator's budgets; nil for a signaling circuit
	periodStart   time.Time     // start of the current idle period; reaper only (under circuitsMu)
	periodBytes   atomic.Uint64 // bytes carried in the current idle period
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

	lim        relayLimits // limitsOf(cfg), guarded by cfgMu
	nodeBucket *ratelimit.Bucket
	useMu      sync.Mutex
	use        map[types.PeerID]*peerUse // per initiating peer: bytes today and speed

	peersMu     sync.Mutex
	connectedAt map[types.PeerID]time.Time               // when each connected peer connected
	delivering  map[types.PeerID]bool                    // stored mail being handed to a reconnected peer
	probing     map[types.PeerID][]messenger.MailboxItem // forwarded to a silent peer, kept if it does not answer
	addrOf      func(types.PeerID) string                // a connected peer's remote address
	answers     func(types.PeerID) bool                  // whether a connected peer answers a ping

	reqLimit  *ratelimit.Keyed[types.PeerID] // relay requests per origin
	fullReply *ratelimit.Keyed[types.PeerID] // "mailbox full" answers per peer

	logMu      sync.Mutex
	logLast    map[string]time.Time
	logSkipped map[string]int

	// totals since the node started, for the operator
	circuitsOpened atomic.Uint64
	bytesRouted    atomic.Uint64
	forwarded      atomic.Uint64 // messages passed on to their recipient or the next relay
	stored         atomic.Uint64 // messages kept in the mailbox for an offline recipient
	delivered      atomic.Uint64 // mailbox messages handed over when the recipient came back
	signalRefused  atomic.Uint64 // frames a signaling circuit would not carry
	refusedMsgs    atomic.Uint64 // messages a signaling node does not relay or keep
	accessRefused  atomic.Uint64 // requests from peers not allowed on a private relay
	limitRefused   atomic.Uint64 // circuits refused by a circuit cap
	rateDropped    atomic.Uint64 // frames dropped by a speed limit
	byDatagram     atomic.Uint64 // media frames passed on as datagrams
	budgetClosed   atomic.Uint64 // circuits closed for using up a byte budget
}

// Limits of a signaling circuit: the hole punching exchange is a few small messages, so anything
// beyond them is traffic, which a signaling node does not carry.
const (
	signalCircuitBytes = 64 << 10
	signalCircuitLife  = 2 * time.Minute
)

// circuitReapEvery: how often idle circuits are looked for (the limits are in relay_limits.go).
const circuitReapEvery = 1 * time.Minute

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
	LimitRefused    uint64 `json:"limitRefused"`  // circuits refused by a per-peer, per-IP or table cap
	RateDropped     uint64 `json:"rateDropped"`   // frames dropped by a speed limit
	ByDatagram      uint64 `json:"byDatagram"`    // media frames of circuits passed on as datagrams
	BudgetClosed    uint64 `json:"budgetClosed"`  // circuits closed for using up a byte budget
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
		LimitRefused:    rs.limitRefused.Load(),
		RateDropped:     rs.rateDropped.Load(),
		ByDatagram:      rs.byDatagram.Load(),
		BudgetClosed:    rs.budgetClosed.Load(),
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
			MaxCircuits: config.DefaultRelayMaxCircuits,
		},
		use:         make(map[types.PeerID]*peerUse),
		connectedAt: make(map[types.PeerID]time.Time),
		reqLimit:    ratelimit.NewKeyed[types.PeerID](relayReqRate, relayReqBurst, limiterPeers),
		fullReply:   ratelimit.NewKeyed[types.PeerID](fullReplyRate, fullReplyBurst, limiterPeers),
		logLast:     make(map[string]time.Time),
		logSkipped:  make(map[string]int),
	}
	rs.lim = limitsOf(rs.cfg)
	rs.nodeBucket = ratelimit.NewBucket(rs.lim.nodeRate, rateBurst(rs.lim.nodeRate))
	if s != nil {
		rs.addrOf = func(id types.PeerID) string {
			if p := s.GetPeer(id); p != nil {
				return p.RemoteAddr()
			}
			return ""
		}
		rs.answers = s.Answers
		s.AddObserver(rs)
		s.AddFrameHandler(rs.HandleRawFrame)
	}
	return rs
}

func (rs *RelayServer) SetPolicy(cfg config.RelayServerConfig) {
	rs.cfgMu.Lock()
	defer rs.cfgMu.Unlock()
	rs.cfg = cfg
	rs.lim = limitsOf(cfg)
	rs.nodeBucket.SetRate(rs.lim.nodeRate, rateBurst(rs.lim.nodeRate))
	rs.useMu.Lock()
	for _, u := range rs.use {
		u.bucket.SetRate(rs.lim.peerRate, rateBurst(rs.lim.peerRate))
	}
	rs.useMu.Unlock()
	rs.allowed = make(map[types.PeerID]bool, len(cfg.AllowedPeers))
	for _, s := range cfg.AllowedPeers {
		if id, err := types.PeerIDFromString(s); err == nil {
			rs.allowed[id] = true
		} else {
			rs.logger.Warn("RelayServer: ignoring malformed allowed peer id", "id", s)
		}
	}
	rs.logger.Info("RelayServer: Dynamic policy updated",
		"enabled", cfg.Enabled, "private", cfg.Private, "allowed", len(rs.allowed), "max_circuits", rs.lim.maxCircuits)
}

// policy returns the configuration and the limits it sets.
func (rs *RelayServer) policy() (config.RelayServerConfig, relayLimits) {
	rs.cfgMu.RLock()
	defer rs.cfgMu.RUnlock()
	return rs.cfg, rs.lim
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

// reapLoop periodically drops idle circuits, so abandoned circuits (and ones opened only to hold
// a slot) free their place in the table, and forgets the peers the limiters no longer need.
func (rs *RelayServer) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(circuitReapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, lim := rs.policy()
			rs.reapIdleCircuits(lim.idle, lim.idleBytes)
			rs.reqLimit.Prune()
			rs.fullReply.Prune()
		}
	}
}

// reapIdleCircuits closes the idle circuits and notifies the still-connected participants. A
// circuit is idle when it carried no frame for maxIdle, or less than minBytes over the last
// maxIdle: a trickle of tiny frames does not keep a slot. A signaling circuit is closed once its
// life is over. It returns how many circuits were reaped.
func (rs *RelayServer) reapIdleCircuits(maxIdle time.Duration, minBytes uint64) int {
	now := time.Now()
	cutoff := now.Add(-maxIdle).UnixNano()

	var toClose []*Circuit
	holding := make(map[types.PeerID]bool)
	rs.circuitsMu.Lock()
	for cid, c := range rs.circuits {
		last := c.LastActive.Load()
		if last == 0 {
			last = c.CreatedAt.UnixNano()
		}
		idle := maxIdle > 0 && last < cutoff
		if c.periodStart.IsZero() {
			c.periodStart = c.CreatedAt
		}
		if maxIdle > 0 && now.Sub(c.periodStart) >= maxIdle {
			if c.periodBytes.Swap(0) < minBytes {
				idle = true
			}
			c.periodStart = now
		}
		if c.SignalOnly && now.Sub(c.CreatedAt) > signalCircuitLife {
			idle = true
		}
		if idle {
			delete(rs.circuits, cid)
			toClose = append(toClose, c)
		} else {
			holding[c.PeerA] = true
		}
	}
	rs.circuitsMu.Unlock()

	for _, c := range toClose {
		rs.notifyClosed(c.ID, c.PeerA, c.PeerB)
	}
	rs.pruneUse(holding, now)
	if len(toClose) > 0 {
		rs.logger.Debug("RelayServer: reaped idle circuits", "count", len(toClose))
	}
	return len(toClose)
}

// notifyClosed tells the given ends of a circuit that it is gone.
func (rs *RelayServer) notifyClosed(cid uint32, peers ...types.PeerID) {
	if rs.swarm == nil {
		return
	}
	closePkt := make([]byte, 4)
	binary.BigEndian.PutUint32(closePkt, cid)
	for _, id := range peers {
		if p := rs.swarm.GetPeer(id); p != nil {
			_ = p.SendRaw(net_types.TypeCircuitClose, closePkt)
		}
	}
}

// closeCircuit removes a circuit and tells both ends.
func (rs *RelayServer) closeCircuit(c *Circuit) {
	rs.circuitsMu.Lock()
	_, ok := rs.circuits[c.ID]
	delete(rs.circuits, c.ID)
	rs.circuitsMu.Unlock()
	if ok {
		rs.notifyClosed(c.ID, c.PeerA, c.PeerB)
	}
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

	cfg, lim := rs.policy()

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

	// 2. Circuit caps (DoS protection): the table, the share fresh peers may take, and what one
	// peer, one IP address and one subnet may hold. Checked here cheaply and again when the
	// circuit goes in, under the same lock, so parallel opens cannot overshoot.
	ipKey, netKey := rs.addrKeys(fromPeer)
	now := time.Now()
	established := rs.established(fromPeer, now)
	var use *peerUse
	if !signal && !cfg.SignalingOnly {
		use = rs.useOf(fromPeer, lim)
	}
	refuse := func(reason string) {
		rs.limitRefused.Add(1)
		resp := make([]byte, 5) // status 0: rejected
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, resp)
		rs.warnLimited(reason, "from", fromPeer.String()[:8], "max_circuits", lim.maxCircuits)
	}
	if use != nil && use.exhausted(lim, now) {
		refuse("daily relay budget used up")
		return
	}
	rs.circuitsMu.RLock()
	reason := rs.admitLocked(fromPeer, ipKey, netKey, established, lim)
	rs.circuitsMu.RUnlock()
	if reason != "" {
		refuse(reason)
		return
	}

	targetID, err := types.ToPeerID(payload[:32])
	if err != nil {
		rs.logger.Debug("RelayServer: invalid target peer ID in CircuitOpen", "err", err)
		return
	}

	// A circuit to ourselves makes no sense and would let a peer loop frames back through us.
	if rs.swarm == nil || targetID == rs.swarm.GetSelfID() {
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
		ID:          cid,
		PeerA:       fromPeer,
		PeerB:       targetID,
		CreatedAt:   now,
		SignalOnly:  signal || cfg.SignalingOnly,
		ipKey:       ipKey,
		netKey:      netKey,
		use:         use,
		periodStart: now,
	}

	circuit.LastActive.Store(now.UnixNano())
	rs.circuitsMu.Lock()
	if reason := rs.admitLocked(fromPeer, ipKey, netKey, established, lim); reason != "" {
		rs.circuitsMu.Unlock()
		refuse(reason)
		return
	}
	if _, taken := rs.circuits[cid]; taken {
		rs.circuitsMu.Unlock()
		_ = rs.sendRawFrame(fromPeer, net_types.TypeCircuitOpenResponse, make([]byte, 5))
		return
	}
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

	rs.logger.Debug("RelayServer: Virtual Circuit established",
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

	n := uint64(len(payload))
	now := time.Now()
	if !circuit.SignalOnly {
		_, lim := rs.policy()
		switch rs.chargeFrame(circuit, n, lim, now) {
		case frameDrop:
			rs.rateDropped.Add(1)
			return
		case frameOverBudget:
			rs.budgetClosed.Add(1)
			rs.closeCircuit(circuit)
			rs.warnLimited("relay byte budget used up, circuit closed", "peer", circuit.PeerA.String()[:8])
			return
		}
	}

	// Zero-copy passthrough of the circuit data frame! The media of a call goes on as a datagram
	// to a peer that takes it that way (see circuit_lane.go), the rest as a stream.
	sent := false
	if inner, ok := innerType(payload); ok && mediaByDatagram(inner, destPeer.Caps()) {
		if destPeer.SendRawUnreliable(net_types.TypeCircuitData, payload) == nil {
			rs.byDatagram.Add(1)
			sent = true
		}
	}
	if !sent {
		_ = destPeer.SendRaw(net_types.TypeCircuitData, payload)
	}

	circuit.LastActive.Store(now.UnixNano())
	circuit.periodBytes.Add(n)
	atomic.AddUint64(&circuit.BytesRouted, n)
	rs.bytesRouted.Add(n)
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
		rs.logger.Debug("RelayServer: Virtual Circuit closed", "circuitID", cid)
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
		rs.logger.Debug("RelayServer: received empty RelayReq")
		return
	}

	targetID, err := types.ToPeerID(req.TargetPeerId)
	if err != nil {
		rs.logger.Debug("RelayServer: invalid target peer ID", "err", err)
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
		rs.warnLimited("relay request with no authenticated origin", "from", fromPeer.String()[:8])
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
	rs.logger.Debug("RelayServer: received relay message",
		"from", fromPeer.String()[:8],
		"target", targetID.String()[:8],
		"self", selfID.String()[:8])

	// 1. If target is self: unwrap envelope and pass to dispatcher, unless this node runs no
	// messenger (server role) and has nobody to give it to
	if targetID == selfID {
		if rs.swarm.Drops(net_types.TypeChatMessage) {
			return
		}
		var innerEnv internal_pb.Envelope
		if err = proto.Unmarshal(req.ForwardedEnvelope, &innerEnv); err == nil {
			rs.swarm.GetDispatcher().PushMessage(&innerEnv, fromPeer)
			rs.logger.Debug("RelayServer: Delivered relay message to local dispatcher", "from", fromPeer.String()[:8])
		} else {
			rs.logger.Debug("RelayServer: failed to unmarshal forwarded envelope", "err", err)
		}
		return
	}

	// Work for others is limited per origin: every message may cost a signature, a send to the
	// next hop and a mailbox write, so one identity cannot keep the node busy with them.
	if !rs.reqLimit.Allow(originID, 1) {
		rs.refusedMsgs.Add(1)
		rs.warnLimited("relay requests from one origin too fast", "origin", originID.String()[:8])
		return
	}

	// 2. If target is directly connected to this node in Swarm: deliver immediately!
	direct := rs.swarm.ThisIsActivePeer(targetID)
	if direct {
		var innerEnv internal_pb.Envelope
		if err = proto.Unmarshal(req.ForwardedEnvelope, &innerEnv); err == nil {
			peer := rs.swarm.GetPeer(targetID)
			if peer != nil {
				_ = peer.Send(net_types.TypeChatMessage, &innerEnv)
				rs.forwarded.Add(1)
				rs.logger.Debug("RelayServer: forwarded relay message directly to target peer", "target", targetID.String()[:8])
				if !peer.HeardWithin(relayHeardFresh) && rs.mailbox != nil {
					rs.keepUnlessAnswered(mailboxItemOf(msg, targetID, originID, req.ForwardedEnvelope))
				}
				return
			}
		}
	}

	// A message that has travelled far enough can only go to the mailbox: when the mailbox would
	// refuse it anyway, say so now, before any work is done for it.
	if !direct && msg.HopLimit <= 1 {
		if reason := rs.mailboxRefuses(targetID, len(req.ForwardedEnvelope)); reason != nil {
			rs.refusedMsgs.Add(1)
			rs.replyMailboxFull(fromPeer, selfID)
			rs.logger.Debug("RelayServer: mailbox would refuse, dropped early", "err", reason, "target", targetID.String()[:8])
			return
		}
	}

	// 3. Multi-hop: Forward to next peer in the chain (not the one who just sent it to us).
	// The request is ours now, signed by us (once, whatever the number of candidates tried); the
	// envelope inside stays the sender's.
	forwarded := false
	if msg.HopLimit > 1 {
		hop := &internal_pb.MessageData{
			MessageId: msg.MessageId,
			OriginId:  selfID.ToBytes(),
			TargetId:  msg.TargetId,
			Timestamp: msg.Timestamp,
			HopLimit:  msg.HopLimit - 1,
			Payload:   msg.Payload,
		}
		var relayEnv *internal_pb.Envelope
		for _, nextPeer := range rs.swarm.GetAllPeers() {
			nextPeerID := nextPeer.ID()
			if nextPeerID == fromPeer || !nextPeer.Relays() {
				continue
			}
			if relayEnv == nil {
				if relayEnv, err = rs.swarm.SignMessageData(hop); err != nil {
					break
				}
			}
			if err := nextPeer.Send(net_types.TypeRelayMessageRequest, relayEnv); err == nil {
				rs.logger.Debug("RelayServer: forwarded relay message to next hop",
					"next", nextPeerID.String()[:8], "target", targetID.String()[:8])
				forwarded = true
				rs.forwarded.Add(1)
				break
			}
		}
	}

	// 4. If target is offline (or unreachable): store in friendly Mailbox!
	if !forwarded && rs.mailbox != nil {
		item := mailboxItemOf(msg, targetID, originID, req.ForwardedEnvelope)
		if err = rs.mailbox.Store(item); err == nil {
			rs.stored.Add(1)
			rs.logger.Debug("RelayServer: saved message to offline Mailbox",
				"target", targetID.String()[:8], "msgID", item.ID)
		} else {
			rs.logger.Debug("RelayServer: failed to store in mailbox", "err", err, "target", targetID.String()[:8])
			// Send network error back to sender peer so it searches for another relay via DHT!
			rs.replyMailboxFull(fromPeer, selfID)
		}
	}
}

// mailboxItemOf is a relayed message as the mailbox keeps it for recipient.
func mailboxItemOf(msg *internal_pb.MessageData, recipient, origin types.PeerID, envelope []byte) messenger.MailboxItem {
	msgID := hex.EncodeToString(msg.MessageId)
	if msgID == "" {
		msgID = fmt.Sprintf("relay-%d", time.Now().UnixNano())
	}
	return messenger.MailboxItem{
		ID:          msgID,
		RecipientID: recipient,
		SenderID:    origin, // the authenticated signer, used for the per-sender quota
		Payload:     envelope,
		CreatedAt:   time.Now().Unix(),
		ExpiresAt:   time.Now().Add(7 * 24 * time.Hour).Unix(),
	}
}

// A recipient can vanish (killed, unplugged, its network gone) without this node noticing: its
// connection stays open here until the transport's idle timeout, about a minute, and everything
// forwarded to it meanwhile is lost — and with it the message, if its sender goes away too. So a
// recipient not heard from for relayHeardFresh is pinged after the forward, and one that does not
// answer gets the message in its mailbox as well; it is handed over when the recipient connects
// again, and dropped by id if the forward did arrive after all. One ping covers every message
// forwarded while it is under way.
const relayHeardFresh = 15 * time.Second

func (rs *RelayServer) keepUnlessAnswered(item messenger.MailboxItem) {
	id := item.RecipientID
	rs.peersMu.Lock()
	if rs.probing == nil {
		rs.probing = map[types.PeerID][]messenger.MailboxItem{}
	}
	waiting, busy := rs.probing[id]
	rs.probing[id] = append(waiting, item)
	rs.peersMu.Unlock()
	if busy {
		return
	}
	go func() {
		answered := rs.answers != nil && rs.answers(id)
		rs.peersMu.Lock()
		items := rs.probing[id]
		delete(rs.probing, id)
		rs.peersMu.Unlock()
		if answered {
			return
		}
		for _, it := range items {
			if err := rs.mailbox.Store(it); err != nil {
				rs.logger.Debug("RelayServer: a silent recipient's mailbox refused a copy", "err", err, "target", id.String()[:8])
				continue
			}
			rs.stored.Add(1)
		}
		rs.logger.Info("RelayServer: recipient did not answer, kept its messages", "peer", id.String()[:8], "count", len(items))
	}()
}

// mailboxRefuses says why the mailbox would not take a message of size bytes for recipient, from
// its limits and running counts alone (nil when it might take it). It is the cheap part of
// MailboxManager.Store; Store still decides.
func (rs *RelayServer) mailboxRefuses(recipient types.PeerID, size int) error {
	if rs.mailbox == nil {
		return messenger.ErrMailboxFull
	}
	l := rs.mailbox.GetLimits()
	switch {
	case l.MaxItemSizeBytes > 0 && int64(size) > l.MaxItemSizeBytes:
		return messenger.ErrItemTooLarge
	case l.MaxTotalMessages > 0 && rs.mailbox.Waiting() >= l.MaxTotalMessages:
		return messenger.ErrMailboxFull
	case l.MaxMessagesPerPeer > 0 && rs.mailbox.Count(recipient) >= l.MaxMessagesPerPeer:
		return messenger.ErrMailboxFull
	}
	return nil
}

// replyMailboxFull tells the sending peer that this node keeps no more messages, so it looks for
// another relay. The answer is signed, so a peer gets one only every few seconds.
func (rs *RelayServer) replyMailboxFull(to, selfID types.PeerID) {
	if !rs.fullReply.Allow(to, 1) {
		return
	}
	senderPeer := rs.swarm.GetPeer(to)
	if senderPeer == nil {
		return
	}
	rejMsg := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  selfID.ToBytes(),
		TargetId:  to[:],
		Timestamp: uint64(time.Now().UnixNano()),
	}
	if rejEnv, rErr := rs.swarm.SignMessageData(rejMsg); rErr == nil {
		_ = senderPeer.Send(net_types.TypeMailboxFull, rejEnv)
		rs.logger.Debug("RelayServer: Sent TypeMailboxFull error to sender", "sender", to.String()[:8])
	}
}

// OnPeerConnected reacts when any peer connects to this node.
// RefreshSeen marks every connected peer as a recipient of this relay again, so that one that
// stays connected for days keeps its mailbox.
func (rs *RelayServer) RefreshSeen() {
	if rs.mailbox == nil {
		return
	}
	for _, p := range rs.swarm.GetAllPeers() {
		rs.mailbox.NoteRecipientSeen(p.ID())
	}
}

func (rs *RelayServer) OnPeerConnected(peerID types.PeerID, pubKey types.PeerPublicKey, addr string) {
	rs.peersMu.Lock()
	if _, ok := rs.connectedAt[peerID]; !ok {
		rs.connectedAt[peerID] = time.Now()
	}
	rs.peersMu.Unlock()

	if rs.mailbox == nil {
		return
	}

	// Remember that this peer uses us, so we will accept stored messages addressed to it while it
	// is offline. We never keep messages for a recipient that has never been here (that is how a
	// stranger would otherwise fill our disk by naming random recipients).
	rs.mailbox.NoteRecipientSeen(peerID)

	rs.peersMu.Lock()
	if rs.delivering == nil {
		rs.delivering = map[types.PeerID]bool{}
	}
	busy := rs.delivering[peerID]
	rs.delivering[peerID] = true
	rs.peersMu.Unlock()
	if busy {
		return // a run for this peer is under way and will deliver everything
	}
	sent := rs.sendStored(peerID)
	if len(sent) == 0 {
		rs.doneDelivering(peerID)
		return
	}
	rs.delivered.Add(uint64(len(sent))) // handed over; dropped after the second pass
	go rs.finishStored(peerID, sent)
}

// A reconnected recipient gets its stored mail twice — when it connects and once more a little
// later — and only then is the mail dropped, if the recipient is still connected. A write on a
// fresh connection that never reaches the recipient's handlers no longer loses the mail; the
// recipient drops the repeat (messages are deduplicated by id). Tests shorten the pause.
var storedSecondPass = 10 * time.Second

func (rs *RelayServer) doneDelivering(peerID types.PeerID) {
	rs.peersMu.Lock()
	delete(rs.delivering, peerID)
	rs.peersMu.Unlock()
}

func (rs *RelayServer) finishStored(peerID types.PeerID, sent map[string]bool) {
	defer rs.doneDelivering(peerID)
	time.Sleep(storedSecondPass)
	if rs.swarm.GetPeer(peerID) == nil {
		return // gone again: the mail stays for its next visit
	}
	for id := range rs.sendStored(peerID) {
		sent[id] = true
	}
	ids := make([]string, 0, len(sent))
	for id := range sent {
		ids = append(ids, id)
	}
	_ = rs.mailbox.Acknowledge(peerID, ids)
	rs.logger.Info("RelayServer: Delivered stored mailbox items to reconnected peer",
		"peer", peerID.String()[:8], "count", len(ids))
}

// sendStored hands the peer everything stored for it and returns the ids that went out.
func (rs *RelayServer) sendStored(peerID types.PeerID) map[string]bool {
	out := map[string]bool{}
	peer := rs.swarm.GetPeer(peerID)
	if peer == nil {
		return out
	}
	pending, err := rs.mailbox.GetPending(peerID)
	if err != nil {
		return out
	}
	for _, item := range pending {
		var innerEnv internal_pb.Envelope
		if err := proto.Unmarshal(item.Payload, &innerEnv); err == nil {
			if err := peer.Send(net_types.TypeChatMessage, &innerEnv); err == nil {
				out[item.ID] = true
			}
		}
	}
	return out
}

func (rs *RelayServer) OnPeerDisconnected(peerID types.PeerID) {
	rs.peersMu.Lock()
	delete(rs.connectedAt, peerID)
	rs.peersMu.Unlock()

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
