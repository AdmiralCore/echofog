package routing

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

var (
	ErrNoRelaysAvailable = errors.New("no active or discovered relays available in mesh")
	ErrRelayQuotaFull    = errors.New("relay rejected message: mailbox quota full on relay")
	ErrCircuitOpenFailed = errors.New("failed to open virtual circuit through relay: target offline or unreachable")
	ErrCircuitNotFound   = errors.New("virtual circuit not found or closed")
)

type ClientCircuit struct {
	ID           uint32
	SignalOnly   bool // carries hole punching messages only (the relay may be a signaling node)
	RelayPeerID  types.PeerID
	RemotePeerID types.PeerID
	CreatedAt    time.Time
}

type circuitOpenResult struct {
	circuitID uint32
	err       error
}

type CircuitDataListener func(circuitID uint32, msgType net_types.MessageType, payload []byte, remotePeer types.PeerID)

type RelayMarketInfo struct {
	PeerID        types.PeerID  `json:"peer_id"`
	Addr          string        `json:"addr"`
	Ping          time.Duration `json:"ping"`
	PricePerMB    int64         `json:"price_per_mb"`
	Score         float64       `json:"score"`
	IsBlacklisted bool          `json:"is_blacklisted"`
	IsViable      bool          `json:"is_viable"`
	Estimated     bool          `json:"estimated"`
}

type RelayClient struct {
	swarm  *swarm.Swarm
	dht    *dht.EchoFogDHT
	logger *slog.Logger

	mu            sync.RWMutex
	circuits      map[uint32]*ClientCircuit
	peerToCircuit map[types.PeerID]uint32
	signalCircuit map[types.PeerID]uint32 // circuits for hole punching messages only, never for traffic
	pendingSignal map[types.PeerID]bool   // circuits being opened: signaling ones
	pendingOpens  map[types.PeerID]chan circuitOpenResult
	blacklisted   map[types.PeerID]time.Time

	// Market-aware economics & configuration
	cfgMu       sync.RWMutex
	cfg         config.RelayClientConfig
	preferred   map[types.PeerID]bool // relays named in cfg.Relays: used even if they do not advertise
	spentToday  atomic.Int64
	relayPrices map[types.PeerID]int64
	relayPings  map[types.PeerID]time.Duration

	listenersMu sync.RWMutex
	listeners   []CircuitDataListener
}

func NewRelayClient(s *swarm.Swarm, dht *dht.EchoFogDHT, logger *slog.Logger) *RelayClient {
	if logger == nil {
		logger = slog.Default().With("component", "relay_client")
	}
	rc := &RelayClient{
		swarm:         s,
		dht:           dht,
		logger:        logger,
		circuits:      make(map[uint32]*ClientCircuit),
		peerToCircuit: make(map[types.PeerID]uint32),
		signalCircuit: make(map[types.PeerID]uint32),
		pendingSignal: make(map[types.PeerID]bool),
		pendingOpens:  make(map[types.PeerID]chan circuitOpenResult),
		blacklisted:   make(map[types.PeerID]time.Time),
		cfg: config.RelayClientConfig{
			Enabled:                 true,
			MaxAcceptablePricePerMB: 150,
			MaxAcceptablePingMs:     250,
			DailyBudgetLimit:        5_000_000,
			MarketAwareSelection:    true,
		},
		relayPrices: make(map[types.PeerID]int64),
		relayPings:  make(map[types.PeerID]time.Duration),
	}
	if s != nil {
		s.AddFrameHandler(rc.HandleRawFrame)
	}
	return rc
}

func (rc *RelayClient) SetPolicy(cfg config.RelayClientConfig) {
	rc.cfgMu.Lock()
	defer rc.cfgMu.Unlock()
	rc.cfg = cfg
	rc.preferred = make(map[types.PeerID]bool, len(cfg.Relays))
	for _, addr := range cfg.Relays {
		if _, id, ok := network.SplitPeerAddr(addr); ok {
			rc.preferred[id] = true
		} else if id, err := types.PeerIDFromString(addr); err == nil {
			rc.preferred[id] = true
		} else {
			rc.logger.Warn("RelayClient: ignoring malformed relay address", "addr", addr)
		}
	}
	rc.logger.Info("RelayClient: Dynamic policy updated",
		"enabled", cfg.Enabled, "max_price", cfg.MaxAcceptablePricePerMB,
		"max_ping", cfg.MaxAcceptablePingMs, "market_aware", cfg.MarketAwareSelection, "preferred", len(rc.preferred))
}

// IsPreferred reports whether a relay was named in the config (used even if it does not advertise).
func (rc *RelayClient) IsPreferred(id types.PeerID) bool {
	rc.cfgMu.RLock()
	defer rc.cfgMu.RUnlock()
	return rc.preferred[id]
}

func (rc *RelayClient) GetPolicy() config.RelayClientConfig {
	rc.cfgMu.RLock()
	defer rc.cfgMu.RUnlock()
	return rc.cfg
}

func (rc *RelayClient) RecordRelayExpense(amount int64) {
	rc.spentToday.Add(amount)
}

func (rc *RelayClient) GetDailySpent() int64 {
	return rc.spentToday.Load()
}

func (rc *RelayClient) ResetDailySpent() {
	rc.spentToday.Store(0)
}

func (rc *RelayClient) SetRelayPrice(peerID types.PeerID, pricePerMB int64) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.relayPrices[peerID] = pricePerMB
}

func (rc *RelayClient) SetRelayPing(peerID types.PeerID, ping time.Duration) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.relayPings[peerID] = ping
}

func (rc *RelayClient) ScoreRelay(price int64, ping time.Duration) float64 {
	pingMs := float64(ping.Milliseconds())
	if pingMs < 1 {
		pingMs = 1
	}
	pingScore := 1000.0 / pingMs

	priceVal := float64(price)
	if priceVal < 0 {
		priceVal = 0
	}
	priceScore := 1000.0 / (1.0 + priceVal)

	return (pingScore * 0.5) + (priceScore * 0.5)
}

// GetMarketStatus inspects connected peers and computes market metrics
func (rc *RelayClient) GetMarketStatus() []RelayMarketInfo {
	var allPeers []*swarm.Peer
	if rc.swarm != nil {
		allPeers = rc.swarm.GetAllPeers()
	}

	rc.cfgMu.RLock()
	cfg := rc.cfg
	rc.cfgMu.RUnlock()

	rc.mu.RLock()
	now := time.Now()
	var market []RelayMarketInfo

	for _, p := range allPeers {
		bUntil, isBlocked := rc.blacklisted[p.ID()]
		isBlacklisted := isBlocked && now.Before(bUntil)

		price, hasPrice := rc.relayPrices[p.ID()]
		if !hasPrice {
			price = 100 // default market estimate
		}

		ping, hasPing := rc.relayPings[p.ID()]
		if !hasPing {
			ping = 50 * time.Millisecond // default healthy estimate
		}

		score := rc.ScoreRelay(price, ping)

		isViable := !isBlacklisted &&
			(cfg.MaxAcceptablePricePerMB <= 0 || price <= cfg.MaxAcceptablePricePerMB) &&
			(cfg.MaxAcceptablePingMs <= 0 || ping <= time.Duration(cfg.MaxAcceptablePingMs)*time.Millisecond)

		if cfg.DailyBudgetLimit > 0 && rc.spentToday.Load() >= cfg.DailyBudgetLimit && price > 0 {
			isViable = false
		}

		market = append(market, RelayMarketInfo{
			PeerID:        p.ID(),
			Addr:          p.Addr(),
			Ping:          ping,
			PricePerMB:    price,
			Score:         score,
			IsBlacklisted: isBlacklisted,
			IsViable:      isViable,
			Estimated:     !hasPing || !hasPrice,
		})
	}
	rc.mu.RUnlock()

	sort.Slice(market, func(i, j int) bool {
		return market[i].Score > market[j].Score
	})

	return market
}

// GetRankedViableRelays returns candidates sorted from highest score to lowest score
func (rc *RelayClient) GetRankedViableRelays(targetID types.PeerID) []*swarm.Peer {
	rc.cfgMu.RLock()
	cfg := rc.cfg
	rc.cfgMu.RUnlock()

	if !cfg.Enabled || rc.swarm == nil {
		return nil
	}

	return rc.rankedRelays(targetID, (*swarm.Peer).Relays)
}

// rankedRelays lists the relays to try, best first. Relays named in the config come first and are
// used even if they do not advertise themselves (a private relay you were given access to); then
// the viable relays discovered on the open market.
func (rc *RelayClient) rankedRelays(targetID types.PeerID, offers func(*swarm.Peer) bool) []*swarm.Peer {
	var ranked []*swarm.Peer
	seen := map[types.PeerID]bool{}
	for _, m := range rc.GetMarketStatus() {
		if m.PeerID == targetID || m.IsBlacklisted {
			continue
		}
		p := rc.swarm.GetPeer(m.PeerID)
		if p == nil {
			continue
		}
		if rc.IsPreferred(m.PeerID) {
			ranked = append(ranked, p)
			seen[m.PeerID] = true
		}
	}
	for _, m := range rc.GetMarketStatus() {
		if m.IsViable && m.PeerID != targetID && !seen[m.PeerID] {
			if p := rc.swarm.GetPeer(m.PeerID); p != nil && offers(p) {
				ranked = append(ranked, p)
			}
		}
	}
	return ranked
}

// SendSignal delivers a hole punching message to targetID: directly, over a circuit that exists,
// or over a new signaling circuit through a relay or a signaling node.
func (rc *RelayClient) SendSignal(ctx context.Context, targetID types.PeerID, msgType net_types.MessageType, payload []byte) error {
	if p := rc.swarm.GetPeer(targetID); p != nil {
		return p.SendRaw(msgType, payload)
	}
	rc.mu.RLock()
	cid, ok := rc.peerToCircuit[targetID]
	if !ok {
		cid, ok = rc.signalCircuit[targetID]
	}
	rc.mu.RUnlock()
	if ok {
		if err := rc.SendCircuitData(cid, msgType, payload); err == nil {
			return nil
		}
		rc.mu.Lock()
		delete(rc.signalCircuit, targetID)
		rc.mu.Unlock()
	}
	rc.cfgMu.RLock()
	enabled := rc.cfg.Enabled
	rc.cfgMu.RUnlock()
	if !enabled || rc.swarm == nil {
		return errors.New("no relay to pass the message on")
	}
	for _, relayPeer := range rc.rankedRelays(targetID, (*swarm.Peer).Signals) {
		openCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		// a relay gets an ordinary request, which every version understands; only a signaling
		// node is asked for a signaling circuit
		newCID, err := rc.openCircuit(openCtx, relayPeer.ID(), targetID, !relayPeer.Relays())
		cancel()
		if err == nil {
			return rc.SendCircuitData(newCID, msgType, payload)
		}
	}
	return errors.New("no relay or signaling node could reach the peer")
}

// AddDataListener registers a listener for incoming raw circuit payloads (file chunks, DHT, RPCs).
func (rc *RelayClient) AddDataListener(listener CircuitDataListener) {
	rc.listenersMu.Lock()
	defer rc.listenersMu.Unlock()
	rc.listeners = append(rc.listeners, listener)
}

// HandleRawFrame intercepts incoming circuit messages.
func (rc *RelayClient) HandleRawFrame(msgType net_types.MessageType, payload []byte, fromPeer types.PeerID) bool {
	switch msgType {
	case net_types.TypeCircuitOpenRequest:
		// Inbound circuit request from another peer forwarded by our relay
		rc.handleInboundCircuitOpen(payload, fromPeer)
		return true

	case net_types.TypeCircuitOpenResponse:
		// Response from relay to our OpenCircuit call
		rc.handleCircuitOpenResponse(payload, fromPeer)
		return true

	case net_types.TypeCircuitData:
		// a data frame arriving over a circuit this node is an endpoint of
		rc.handleCircuitData(payload, fromPeer)
		return true

	case net_types.TypeCircuitClose:
		rc.handleCircuitClose(payload, fromPeer)
		return true

	default:
		return false
	}
}

func (rc *RelayClient) handleInboundCircuitOpen(payload []byte, relayPeer types.PeerID) {
	if len(payload) < 36 {
		return
	}
	cid := binary.BigEndian.Uint32(payload[:4])
	originID, err := types.ToPeerID(payload[4:36])
	if err != nil {
		return
	}

	cc := &ClientCircuit{
		ID:           cid,
		RelayPeerID:  relayPeer,
		RemotePeerID: originID,
		CreatedAt:    time.Now(),
	}
	cc.SignalOnly = len(payload) >= 37 && payload[36] == 1
	rc.mu.Lock()
	rc.circuits[cid] = cc
	if cc.SignalOnly {
		rc.signalCircuit[originID] = cid // not for traffic
	} else {
		rc.peerToCircuit[originID] = cid
	}
	rc.mu.Unlock()

	rc.logger.Info("RelayClient: Inbound virtual circuit established",
		"circuitID", cid, "origin", originID.String()[:8], "relay", relayPeer.String()[:8])
}

func (rc *RelayClient) handleCircuitOpenResponse(payload []byte, fromPeer types.PeerID) {
	if len(payload) < 5 {
		return
	}
	cid := binary.BigEndian.Uint32(payload[:4])
	status := payload[4]

	var targetID types.PeerID
	if len(payload) >= 37 {
		copy(targetID[:], payload[5:37])
	}

	rc.mu.Lock()
	var ch chan circuitOpenResult
	for pID, pendingCh := range rc.pendingOpens {
		if targetID == (types.PeerID{}) || pID == targetID {
			ch = pendingCh
			targetID = pID
			break
		}
	}

	if status == 1 && targetID != (types.PeerID{}) {
		cc := &ClientCircuit{
			ID:           cid,
			RelayPeerID:  fromPeer,
			RemotePeerID: targetID,
			CreatedAt:    time.Now(),
			SignalOnly:   rc.pendingSignal[targetID],
		}
		rc.circuits[cid] = cc
		if cc.SignalOnly {
			rc.signalCircuit[targetID] = cid
		} else {
			rc.peerToCircuit[targetID] = cid
		}
	}
	rc.mu.Unlock()

	if ch != nil {
		var res circuitOpenResult
		if status == 1 {
			res = circuitOpenResult{circuitID: cid, err: nil}
		} else {
			res = circuitOpenResult{circuitID: 0, err: ErrCircuitOpenFailed}
		}
		select {
		case ch <- res:
		default:
		}
	}
}

func (rc *RelayClient) handleCircuitData(payload []byte, fromPeer types.PeerID) {
	if len(payload) < 5 {
		return
	}
	cid := binary.BigEndian.Uint32(payload[:4])
	innerMsgType := net_types.MessageType(payload[4])
	innerData := payload[5:]

	rc.mu.RLock()
	circuit, exists := rc.circuits[cid]
	rc.mu.RUnlock()

	if !exists {
		return
	}

	// 1. Notify listeners (for file transfers, DHT, RPCs)
	rc.listenersMu.RLock()
	for _, l := range rc.listeners {
		l(cid, innerMsgType, innerData, circuit.RemotePeerID)
	}
	rc.listenersMu.RUnlock()

	// 2. If it's a chat message or E2EE envelope, unwrap and deliver to Dispatcher
	if innerMsgType == net_types.TypeChatMessage {
		var innerEnv internal_pb.Envelope
		if err := proto.Unmarshal(innerData, &innerEnv); err == nil {
			rc.swarm.GetDispatcher().PushMessage(&innerEnv, circuit.RemotePeerID)
		}
	}
}

func (rc *RelayClient) handleCircuitClose(payload []byte, fromPeer types.PeerID) {
	if len(payload) < 4 {
		return
	}
	cid := binary.BigEndian.Uint32(payload[:4])

	rc.mu.Lock()
	if c, ok := rc.circuits[cid]; ok {
		delete(rc.circuits, cid)
		if rc.peerToCircuit[c.RemotePeerID] == cid {
			delete(rc.peerToCircuit, c.RemotePeerID)
		}
		if rc.signalCircuit[c.RemotePeerID] == cid {
			delete(rc.signalCircuit, c.RemotePeerID)
		}
	}
	rc.mu.Unlock()
}

// OpenCircuit opens a virtual tunnel to targetID through relayID.
func (rc *RelayClient) OpenCircuit(ctx context.Context, relayID types.PeerID, targetID types.PeerID) (uint32, error) {
	return rc.openCircuit(ctx, relayID, targetID, false)
}

// openCircuit asks relayID for a circuit to targetID; signal asks for one that carries hole
// punching messages only, which signaling nodes grant too.
func (rc *RelayClient) openCircuit(ctx context.Context, relayID types.PeerID, targetID types.PeerID, signal bool) (uint32, error) {
	rc.mu.RLock()
	if cid, ok := rc.peerToCircuit[targetID]; ok {
		rc.mu.RUnlock()
		return cid, nil
	}
	if cid, ok := rc.signalCircuit[targetID]; ok && signal {
		rc.mu.RUnlock()
		return cid, nil
	}
	rc.mu.RUnlock()

	relayPeer := rc.swarm.GetPeer(relayID)
	if relayPeer == nil {
		return 0, fmt.Errorf("relay %s not connected", relayID.String()[:8])
	}

	ch := make(chan circuitOpenResult, 1)
	rc.mu.Lock()
	rc.pendingOpens[targetID] = ch
	rc.pendingSignal[targetID] = signal
	rc.mu.Unlock()

	defer func() {
		rc.mu.Lock()
		delete(rc.pendingOpens, targetID)
		delete(rc.pendingSignal, targetID)
		rc.mu.Unlock()
	}()

	req := targetID[:]
	if signal {
		req = append(append([]byte(nil), targetID[:]...), 1)
	}
	if err := relayPeer.SendRaw(net_types.TypeCircuitOpenRequest, req); err != nil {
		return 0, err
	}

	select {
	case res := <-ch:
		return res.circuitID, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// SendCircuitData dispatches arbitrary msgType and payload across an active circuit tunnel.
func (rc *RelayClient) SendCircuitData(circuitID uint32, msgType net_types.MessageType, payload []byte) error {
	rc.mu.RLock()
	circuit, ok := rc.circuits[circuitID]
	rc.mu.RUnlock()
	if !ok {
		return ErrCircuitNotFound
	}

	relayPeer := rc.swarm.GetPeer(circuit.RelayPeerID)
	if relayPeer == nil {
		return fmt.Errorf("relay %s disconnected", circuit.RelayPeerID.String()[:8])
	}

	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[:4], circuitID)
	frame[4] = byte(msgType)
	copy(frame[5:], payload)

	return relayPeer.SendRaw(net_types.TypeCircuitData, frame)
}

// SendUniversal automatically delivers ANY payload (files, DHT, messages) to targetID:
// 1. Direct P2P if connected.
// 2. Over existing Virtual Circuit if established.
// 3. Spawns a new Virtual Circuit via available connected relays.
func (rc *RelayClient) SendUniversal(ctx context.Context, targetID types.PeerID, msgType net_types.MessageType, payload []byte) error {
	// 1. Direct peer?
	if p := rc.swarm.GetPeer(targetID); p != nil {
		return p.SendRaw(msgType, payload)
	}

	// 2. Existing Circuit?
	rc.mu.RLock()
	cid, hasCircuit := rc.peerToCircuit[targetID]
	rc.mu.RUnlock()
	if hasCircuit {
		if err := rc.SendCircuitData(cid, msgType, payload); err == nil {
			return nil
		}
	}

	// 3. Connect via market-ranked viable relay
	viableRelays := rc.GetRankedViableRelays(targetID)

	for _, relayPeer := range viableRelays {
		openCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		newCID, err := rc.OpenCircuit(openCtx, relayPeer.ID(), targetID)
		cancel()
		if err == nil {
			rc.logger.Info("RelayClient: Established new circuit and routed data",
				"target", targetID.String()[:8], "circuitID", newCID, "relay", relayPeer.ID().String()[:8])

			// Record expense based on relay's price
			rc.mu.RLock()
			price, hasPrice := rc.relayPrices[relayPeer.ID()]
			if !hasPrice {
				price = 100
			}
			rc.mu.RUnlock()
			rc.RecordRelayExpense(price)

			return rc.SendCircuitData(newCID, msgType, payload)
		}
	}

	// 4. Query DHT
	if rc.dht != nil {
		addr, dErr := rc.dht.FindPeer(ctx, targetID)
		if dErr == nil && addr != "" {
			rc.swarm.Bootstrap([]string{network.JoinPeerAddr(addr, targetID)})
		}
	}

	return ErrNoRelaysAvailable
}

// SendRoutedEnvelope routes an encrypted envelope to targetID through available relays.
// If a relay fails or responds full, it automatically discovers alternative relays via DHT.
func (rc *RelayClient) SendRoutedEnvelope(ctx context.Context, targetID types.PeerID, env *internal_pb.Envelope) error {
	envBytes, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal inner envelope: %w", err)
	}

	// Fast Path: If a circuit is open, send with Zero-Copy directly!
	rc.mu.RLock()
	cid, hasCircuit := rc.peerToCircuit[targetID]
	rc.mu.RUnlock()
	if hasCircuit {
		if err := rc.SendCircuitData(cid, net_types.TypeChatMessage, envBytes); err == nil {
			return nil
		}
	}

	// Multi-hop / Store-and-forward fallback
	relayMsg := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  rc.swarm.GetSelfID().ToBytes(),
		TargetId:  targetID[:],
		Timestamp: uint64(time.Now().UnixNano()),
		Payload: &internal_pb.MessageData_RelayReq{
			RelayReq: &internal_pb.RelayRequest{
				TargetPeerId:      targetID[:],
				ForwardedEnvelope: envBytes,
			},
		},
	}

	relayEnv, err := rc.swarm.SignMessageData(relayMsg)
	if err != nil {
		return fmt.Errorf("failed to sign relay envelope: %w", err)
	}

	allPeers := rc.swarm.GetAllPeers()
	rc.mu.RLock()
	now := time.Now()
	var viablePeers []*swarm.Peer
	for _, p := range allPeers {
		bUntil, isBlocked := rc.blacklisted[p.ID()]
		if !isBlocked || now.After(bUntil) {
			viablePeers = append(viablePeers, p)
		}
	}
	rc.mu.RUnlock()

	for _, p := range viablePeers {
		if err := p.Send(net_types.TypeRelayMessageRequest, relayEnv); err == nil {
			rc.logger.Info("RelayClient: successfully dispatched relay request",
				"relay", p.ID().String()[:8], "target", targetID.String()[:8])
			return nil
		}
	}

	if rc.dht != nil {
		addr, dErr := rc.dht.FindPeer(ctx, targetID)
		if dErr == nil && addr != "" {
			rc.swarm.Bootstrap([]string{network.JoinPeerAddr(addr, targetID)})
		}
	}

	return ErrNoRelaysAvailable
}

// MarkRelayFull temporarily blacklists a relay node when it sends TypeMailboxFull error.
func (rc *RelayClient) MarkRelayFull(relayID types.PeerID) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.blacklisted[relayID] = time.Now().Add(10 * time.Minute)
	rc.logger.Warn("RelayClient: Relay node marked full, searching for alternative relays",
		"relay", relayID.String()[:8])
}
