package node

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/echofog/echofog-core/pkg/api/types"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
)

type NetworkStats struct {
	Online         bool   `json:"online"`
	ConnectedPeers int    `json:"connectedPeers"`
	BytesRx        uint64 `json:"bytesRx"`
	BytesTx        uint64 `json:"bytesTx"`
}

type RelayInfo struct {
	PeerID        string  `json:"peerId"`
	Addr          string  `json:"addr"`
	PingMs        int64   `json:"pingMs"`
	PricePerMB    int64   `json:"pricePerMb"`
	Score         float64 `json:"score"`
	Rank          int     `json:"rank"`
	IsViable      bool    `json:"isViable"`
	IsBlacklisted bool    `json:"isBlacklisted"`
	Estimated     bool    `json:"estimated"`
}

type VPNStatus struct {
	Running      bool   `json:"running"`
	ListenAddr   string `json:"listenAddr"`
	RelayPeerID  string `json:"relayPeerId"`
	ActiveConns  int64  `json:"activeConns"`
	BytesRx      uint64 `json:"bytesRx"`
	BytesTx      uint64 `json:"bytesTx"`
	TunnelReady  bool   `json:"tunnelReady"`
	TunnelNotice string `json:"tunnelNotice"`
	// ExitState is what the exit itself says about serving us: "checking", "ok", "disabled"
	// (it does not share its connection), "quota" (the free traffic is used up and nothing is
	// paid) or "unreachable". The tunnel is only ready once it is "ok".
	ExitState string `json:"exitState"`
	// Refused counts streams the exit would not carry. The latest one: why ("denied", "payment",
	// "dial failed", "timed out" or "exit unreachable"), for which host:port, and when (Unix seconds).
	Refused           uint64 `json:"refused"`
	LastRefusalReason string `json:"lastRefusalReason"`
	LastRefusalHost   string `json:"lastRefusalHost"`
	LastRefusalAt     int64  `json:"lastRefusalAt"`
	// WholeComputer: every application goes through the exit via the TUN device
	WholeComputer          bool   `json:"wholeComputer"`
	WholeComputerSupported bool   `json:"wholeComputerSupported"`
	WholeComputerError     string `json:"wholeComputerError"`
}

type vpnState struct {
	mu         sync.Mutex
	server     *exit_gateway.SOCKS5Server
	relayPeer  string
	exitState  string // see VPNStatus.ExitState
	exitNotice string
}

// Exit states reported in VPNStatus.ExitState.
const (
	ExitChecking    = "checking"
	ExitOK          = "ok"
	ExitDisabled    = "disabled"
	ExitQuota       = "quota"
	ExitUnreachable = "unreachable"
)

// classifyExit turns the exit's answer to an account query into a state and a sentence for the
// user. The peer link being up says nothing about this: a node with its exit off answers every
// stream with "denied", so without asking, the app showed "Connected" while no site opened.
func classifyExit(info *exit_gateway.AccountInfo, err error) (state, notice string) {
	switch {
	case err != nil:
		return ExitUnreachable, "Exit unreachable: the node does not answer exit requests (" + err.Error() + ")"
	case info == nil:
		return ExitUnreachable, "Exit unreachable: no answer"
	case !info.Enabled:
		return ExitDisabled, "This node does not share its connection: its exit is turned off"
	case info.PricePerMB > 0 && info.Remaining() <= 0:
		return ExitQuota, "Free traffic used up: pay for traffic or ask the owner to add you to the free list"
	}
	return ExitOK, ""
}

// noteExitState records what the exit said, and tells the UI when it changed.
func (n *NodeApi) noteExitState(relay string, info *exit_gateway.AccountInfo, err error) {
	state, notice := classifyExit(info, err)
	n.vpn.mu.Lock()
	if n.vpn.server == nil || n.vpn.relayPeer != relay {
		n.vpn.mu.Unlock()
		return // disconnected or switched exits meanwhile
	}
	changed := state != n.vpn.exitState
	n.vpn.exitState, n.vpn.exitNotice = state, notice
	n.vpn.mu.Unlock()
	if changed {
		if state == ExitOK {
			slog.Info("exit is serving us", "exit", relay)
		} else {
			slog.Warn("exit is not serving us", "exit", relay, "state", state, "why", notice)
		}
		n.emit("vpn:exit_state", map[string]any{"state": state, "notice": notice})
	}
}

func (n *NodeApi) GetNetworkStats() NetworkStats {
	if n == nil || n.node == nil || n.node.Swarm == nil {
		return NetworkStats{}
	}
	rx, tx := n.node.Swarm.TrafficStats()
	return NetworkStats{
		Online:         true,
		ConnectedPeers: len(n.node.Swarm.GetAllPeers()),
		BytesRx:        rx,
		BytesTx:        tx,
	}
}

func (n *NodeApi) GetRelayMarket() []RelayInfo {
	result := []RelayInfo{}
	if n == nil || n.node == nil || n.node.RelayClient == nil {
		return result
	}
	for i, m := range n.node.RelayClient.GetMarketStatus() {
		result = append(result, RelayInfo{
			PeerID:        hex.EncodeToString(m.PeerID[:]),
			Addr:          m.Addr,
			PingMs:        m.Ping.Milliseconds(),
			PricePerMB:    m.PricePerMB,
			Score:         m.Score,
			Rank:          i + 1,
			IsViable:      m.IsViable,
			IsBlacklisted: m.IsBlacklisted,
			Estimated:     m.Estimated,
		})
	}
	return result
}

func (n *NodeApi) relayPeerID(hexID string) (types.PeerID, error) {
	var id types.PeerID
	raw, err := hex.DecodeString(hexID)
	if err != nil || len(raw) != len(id) {
		return id, errors.New("choose an exit relay from the list")
	}
	copy(id[:], raw)
	return id, nil
}

func (n *NodeApi) relayConnected(hexID string) bool {
	id, err := n.relayPeerID(hexID)
	if err != nil || n == nil || n.node == nil || n.node.Swarm == nil {
		return false
	}
	return n.node.Swarm.GetPeer(id) != nil
}

func (n *NodeApi) GetVPNStatus() VPNStatus {
	whole, wholeErr := n.wholeComputerStatus()
	n.vpn.mu.Lock()
	defer n.vpn.mu.Unlock()
	st := VPNStatus{TunnelNotice: "Not connected", WholeComputer: whole, WholeComputerError: wholeErr, WholeComputerSupported: n.WholeComputerSupported()}
	if n.vpn.server == nil {
		return st
	}
	st.Running = true
	st.RelayPeerID = n.vpn.relayPeer
	if a := n.vpn.server.Addr(); a != nil {
		st.ListenAddr = a.String()
	}
	st.ActiveConns, st.BytesRx, st.BytesTx = n.vpn.server.Stats()
	var last *exit_gateway.Refusal
	st.Refused, last = n.vpn.server.Refusals()
	if last != nil {
		st.LastRefusalReason, st.LastRefusalHost = last.Reason, last.Host
		st.LastRefusalAt = last.At.Unix()
	}
	st.ExitState = n.vpn.exitState
	switch {
	case !n.relayConnected(n.vpn.relayPeer):
		st.TunnelNotice = "Exit relay is not connected"
	case st.ExitState == ExitOK:
		st.TunnelReady = true
		st.TunnelNotice = ""
	case st.ExitState == ExitChecking || st.ExitState == "":
		st.TunnelNotice = "Asking the exit whether it serves us..."
	default:
		st.TunnelNotice = n.vpn.exitNotice
	}
	return st
}

func (n *NodeApi) StartVPN(relayPeerID string) error {
	return n.startVPN(relayPeerID, "127.0.0.1:1080")
}

func (n *NodeApi) startVPN(relayPeerID, listen string) error {
	if relayPeerID == "auto" {
		best, err := n.bestExit()
		if err != nil {
			return err
		}
		relayPeerID = best
	}
	if host, _, err := net.SplitHostPort(listen); err != nil || host != "127.0.0.1" {
		return errors.New("vpn proxy must listen on 127.0.0.1")
	}
	if n.node == nil || n.node.Swarm == nil {
		return errors.New("node is not running")
	}
	exit, err := n.relayPeerID(relayPeerID)
	if err != nil {
		return err
	}
	if err := n.ensureConnected(exit); err != nil {
		return err
	}

	n.vpn.mu.Lock()
	defer n.vpn.mu.Unlock()
	if n.vpn.server != nil {
		return errors.New("vpn already running")
	}
	srv := exit_gateway.NewSOCKS5Server(listen, exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(n.node.Swarm, exit)))
	if n.node.EventBus != nil {
		srv.SetEmitter(n.node.EventBus)
	}
	if err := srv.Start(); err != nil {
		return err
	}
	n.vpn.server = srv
	n.vpn.relayPeer = relayPeerID
	n.vpn.exitState, n.vpn.exitNotice = ExitChecking, ""
	go n.startPayGuard() // outside the vpn lock: it reads the same state
	if n.node.EventBus != nil {
		n.node.EventBus.Emit("vpn:started", map[string]any{"relay": relayPeerID})
	}
	return nil
}

// ExitGatewayStatus describes whether this node lets other peers reach the Internet through it
type ExitGatewayStatus struct {
	Enabled  bool   `json:"enabled"`
	Accepted uint64 `json:"accepted"`
	Refused  uint64 `json:"refused"`
	Bytes    uint64 `json:"bytes"`
}

// SetExitGatewayEnabled opts this node in or out of serving other peers' Internet traffic; it is off after every restart
func (n *NodeApi) SetExitGatewayEnabled(on bool) error {
	if n == nil || n.node == nil || n.node.ExitService == nil {
		return errors.New("node is not running")
	}
	n.node.ExitService.SetEnabled(on)
	return nil
}

func (n *NodeApi) GetExitGatewayStatus() ExitGatewayStatus {
	if n == nil || n.node == nil || n.node.ExitService == nil {
		return ExitGatewayStatus{}
	}
	a, r, b := n.node.ExitService.Stats()
	return ExitGatewayStatus{Enabled: n.node.ExitService.Enabled(), Accepted: a, Refused: r, Bytes: b}
}

func (n *NodeApi) StopVPN() error {
	n.stopPayGuard()
	// the TUN must go first: with the proxy gone it would cut every application off
	if n.node != nil && n.node.Transport != nil {
		_ = n.SetWholeComputer(false)
	}
	n.vpn.mu.Lock()
	defer n.vpn.mu.Unlock()
	if n.vpn.server == nil {
		return nil
	}
	err := n.vpn.server.Stop()
	n.vpn.server = nil
	n.vpn.relayPeer = ""
	n.vpn.exitState, n.vpn.exitNotice = "", ""
	if n.node != nil && n.node.EventBus != nil {
		n.node.EventBus.Emit("vpn:stopped", map[string]any{})
	}
	return err
}
