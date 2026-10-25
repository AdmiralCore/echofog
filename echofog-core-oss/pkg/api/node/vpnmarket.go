package node

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// SharingConfig is what the operator offers when sharing the connection
type SharingConfig struct {
	PricePerMB int64 `json:"pricePerMb"` // micro-USDT per MB; 49 is about 5 cents a GB. 0 = free
	FreeTestKB int   `json:"freeTestKb"`
	UploadKbps int   `json:"uploadKbps"`
	// FreePeers are peer IDs served for free: no quota and no payment, whatever the price.
	FreePeers []string `json:"freePeers"`
}

// SharingStatus describes the exit service and its DHT advert
type SharingStatus struct {
	Enabled     bool          `json:"enabled"`
	Config      SharingConfig `json:"config"`
	LastPublish int64         `json:"lastPublish"`
	ExpiresAt   int64         `json:"expiresAt"`
	LastError   string        `json:"lastError"`
	Accepted    uint64        `json:"accepted"`
	Refused     uint64        `json:"refused"`
	// RefusedBy splits Refused by reason ("exit disabled", "payment required", ...)
	RefusedBy map[string]uint64 `json:"refusedBy"`
	Bytes     uint64            `json:"bytes"`
	// PaymentsReady: clients can pay this exit (priced, and the wallet is ready to receive)
	PaymentsReady bool   `json:"paymentsReady"`
	PayAddress    string `json:"payAddress"`
	EarnedUSDT    string `json:"earnedUsdt"` // credited from clients so far
}

// ExitNode is one exit found in the network, joined with this client's own measurements
type ExitNode struct {
	PeerID          string  `json:"peerId"`
	PricePerMB      int64   `json:"pricePerMb"`
	FreeTestKB      int     `json:"freeTestKb"`
	ClaimedKbps     int     `json:"claimedKbps"`
	MeasuredKbps    int     `json:"measuredKbps"`
	LatencyMs       float64 `json:"latencyMs"`
	Score           float64 `json:"score"`
	Probes          int     `json:"probes"`
	Successes       int     `json:"successes"`
	DomainsTried    int     `json:"domainsTried"`
	DomainsOK       int     `json:"domainsOk"`
	LastProbe       int64   `json:"lastProbe"`
	PaymentRequired bool    `json:"paymentRequired"`
	Connected       bool    `json:"connected"`
	ExpiresAt       int64   `json:"expiresAt"`
}

// ProbeReport is the result of one manual check
type ProbeReport struct {
	OK              bool    `json:"ok"`
	LatencyMs       float64 `json:"latencyMs"`
	MeasuredKbps    int     `json:"measuredKbps"`
	DomainsTried    int     `json:"domainsTried"`
	DomainsOK       int     `json:"domainsOk"`
	PaymentRequired bool    `json:"paymentRequired"`
}

func (n *NodeApi) market() (*vpnmarket.Service, error) {
	if n == nil || n.node == nil || n.node.VPNMarket == nil || n.node.ExitService == nil {
		return nil, errors.New("node is not running")
	}
	return n.node.VPNMarket, nil
}

// StartSharing turns this node into an exit: it applies the offer and accepts tunnels. A public
// node also keeps an advert in the DHT; a hidden or isolated one serves only the people it invited.
func (n *NodeApi) StartSharing(cfg SharingConfig) error {
	m, err := n.market()
	if err != nil {
		return err
	}
	if cfg.UploadKbps <= 0 {
		cfg.UploadKbps = n.uploadKbps
	}
	gated := n.node.AccessMode() != config.AccessPublic
	if cfg.UploadKbps <= 0 && !gated {
		// the advert claims a speed; a hidden exit has no advert
		return errors.New("upload speed is unknown: measure it first or enter it manually")
	}
	if cfg.FreeTestKB == 0 && cfg.PricePerMB > 0 {
		cfg.FreeTestKB = 512
	}
	if cfg.FreePeers != nil {
		if err := n.SetExitFreePeers(cfg.FreePeers); err != nil {
			return err
		}
	} else {
		n.applyExitFreePeers()
	}

	if cfg.PricePerMB > 0 {
		// a priced exit must be able to receive money, or clients could never pay it
		if err := n.startEarning(); err != nil {
			return errors.New("to charge for traffic the TON wallet must be ready: " + err.Error())
		}
	}

	exit := n.node.ExitService
	exit.SetPolicy(exit_gateway.Policy{PricePerMB: cfg.PricePerMB, FreeBytes: int64(cfg.FreeTestKB) * 1024})
	exit.SetEnabled(true)

	if gated {
		return nil // a hidden exit is not advertised: the people it invited use it by its link
	}
	err = m.Publisher.Start(n.node.Ctx, vpnmarket.ShareConfig{PricePerMB: cfg.PricePerMB, UploadKbps: cfg.UploadKbps, FreeTestKB: cfg.FreeTestKB})
	if err != nil {
		exit.SetEnabled(false)
		n.stopEarning()
		return err
	}
	return nil
}

// SetExitFreePeers saves the peers the exit serves for free and applies the list at once, also
// while sharing.
func (n *NodeApi) SetExitFreePeers(ids []string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node is not running")
	}
	norm, err := normalizePeerIDs(ids)
	if err != nil {
		return err
	}
	settingsMu.Lock()
	err = saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.ExitFreePeers = norm })
	if err == nil {
		n.exitFreePeers = norm
	}
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	n.applyExitFreePeers()
	return nil
}

// GetExitFreePeers returns the peers the exit serves for free.
func (n *NodeApi) GetExitFreePeers() []string {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return append([]string{}, n.exitFreePeers...)
}

func (n *NodeApi) applyExitFreePeers() {
	if n.node == nil || n.node.ExitService == nil {
		return
	}
	var ids []types.PeerID
	for _, s := range n.GetExitFreePeers() {
		if id, err := types.PeerIDFromString(s); err == nil {
			ids = append(ids, id)
		}
	}
	n.node.ExitService.SetFreePeers(ids)
}

// StopSharing withdraws the advert and stops serving other peers
func (n *NodeApi) StopSharing() error {
	m, err := n.market()
	if err != nil {
		return err
	}
	n.node.ExitService.SetEnabled(false)
	n.stopEarning()
	return m.Publisher.Stop()
}

func (n *NodeApi) GetSharingStatus() SharingStatus {
	m, err := n.market()
	if err != nil {
		return SharingStatus{}
	}
	ps := m.Publisher.Status()
	a, r, b := n.node.ExitService.Stats()
	return SharingStatus{
		Enabled: n.node.ExitService.Enabled(),
		Config: SharingConfig{
			PricePerMB: ps.Config.PricePerMB, FreeTestKB: ps.Config.FreeTestKB, UploadKbps: ps.Config.UploadKbps,
			FreePeers: n.GetExitFreePeers(),
		},
		LastPublish: ps.LastPublish, ExpiresAt: ps.ExpiresAt, LastError: ps.LastError,
		Accepted: a, Refused: r, Bytes: b, RefusedBy: n.node.ExitService.RefusedByReason(),
		PaymentsReady: n.node.ExitService.AccountOf(types.PeerID{}).PayAddress != "",
		PayAddress:    n.node.ExitService.AccountOf(types.PeerID{}).PayAddress,
		EarnedUSDT:    n.earnedUSDT(),
	}
}

// ListExitNodes returns the verified exits found in the DHT, best rated first
func (n *NodeApi) ListExitNodes() []ExitNode {
	m, err := n.market()
	if err != nil {
		return []ExitNode{}
	}
	out := []ExitNode{}
	for _, e := range m.Directory.List() {
		id, _ := e.Advert.ID()
		out = append(out, ExitNode{
			PeerID: e.Advert.PeerID, PricePerMB: e.Advert.PricePerMB, FreeTestKB: e.Advert.FreeTestKB,
			ClaimedKbps: e.Advert.UploadKbps, MeasuredKbps: e.Rating.MeasuredKbps, LatencyMs: e.Rating.LatencyMs,
			Score: e.Score, Probes: e.Rating.Probes, Successes: e.Rating.Successes,
			DomainsTried: e.Rating.DomainsTried, DomainsOK: e.Rating.DomainsOK, LastProbe: e.Rating.LastProbe,
			PaymentRequired: e.Rating.PaymentRequired, Connected: n.node.Swarm.GetPeer(id) != nil,
			ExpiresAt: e.Advert.ExpiresAt,
		})
	}
	return out
}

// RefreshExitNodes rescans the DHT and returns how many live exits were found
func (n *NodeApi) RefreshExitNodes() (int, error) {
	m, err := n.market()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if n.node.DhtEngine != nil {
		n.node.DhtEngine.ForgetLookups() // asked for by hand: walk the network again for every exit
	}
	return m.Directory.Refresh(ctx), nil
}

// ProbeExitNode checks one exit now: domain availability through the tunnel and real download speed
func (n *NodeApi) ProbeExitNode(peerID string) (ProbeReport, error) {
	m, err := n.market()
	if err != nil {
		return ProbeReport{}, err
	}
	raw, err := hex.DecodeString(peerID)
	if err != nil || len(raw) != 32 {
		return ProbeReport{}, errors.New("invalid peer id")
	}
	var id types.PeerID
	copy(id[:], raw)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := m.Probe(ctx, id)
	rep := ProbeReport{
		OK: res.OK, LatencyMs: res.LatencyMs, MeasuredKbps: res.MeasuredKbps,
		DomainsTried: res.DomainsTried, DomainsOK: res.DomainsOK, PaymentRequired: res.PaymentRequired,
	}
	return rep, err
}

func (n *NodeApi) GetProbeDomains() []string {
	m, err := n.market()
	if err != nil {
		return vpnmarket.DefaultProbeDomains
	}
	return m.Domains()
}

// SiteLists is what the Check sites dialog keeps: the sites it checks and ranks exits by, the
// sites the user added, and whether the user ever chose (else the dialog offers a starter set).
type SiteLists struct {
	Selected []string `json:"selected"`
	Custom   []string `json:"custom"`
	Chosen   bool     `json:"chosen"`
}

// GetSiteLists returns the Check sites lists, kept by the node so they survive restarts.
func (n *NodeApi) GetSiteLists() (SiteLists, error) {
	m, err := n.market()
	if err != nil {
		return SiteLists{}, err
	}
	l := SiteLists{Selected: m.Domains(), Custom: m.CustomSites(), Chosen: m.DomainsSaved()}
	if l.Chosen {
		l.Selected = m.SavedDomains()
	}
	if l.Custom == nil {
		l.Custom = []string{}
	}
	return l, nil
}

// SetCustomSites saves the sites the user added to the Check sites dialog.
func (n *NodeApi) SetCustomSites(sites []string) error {
	m, err := n.market()
	if err != nil {
		return err
	}
	return m.SetCustomSites(sites)
}

func (n *NodeApi) SetProbeDomains(domains []string) error {
	m, err := n.market()
	if err != nil {
		return err
	}
	return m.SetDomains(domains)
}

// bestExit returns the highest ranked live exit that is not blocked by repeated failures
func (n *NodeApi) bestExit() (string, error) {
	m, err := n.market()
	if err != nil {
		return "", err
	}
	for _, e := range m.Directory.List() {
		if e.Rating.ConsecutiveFails < 3 {
			return e.Advert.PeerID, nil
		}
	}
	return "", errors.New("no exit nodes discovered yet")
}

// ensureConnected dials a peer found through the DHT if there is no live connection yet
func (n *NodeApi) ensureConnected(id types.PeerID) error {
	if n.node.Swarm.GetPeer(id) != nil {
		return nil
	}
	if n.node.DhtEngine == nil {
		return errors.New("exit relay is not connected")
	}
	// an exit behind NAT has no address to dial: the node falls back to a hole punch through a relay
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.node.EnsureConnected(ctx, id); err != nil {
		return errors.New("exit relay is not connected")
	}
	return nil
}

// StartVPNAt is StartVPN with an explicit loopback listen address
func (n *NodeApi) StartVPNAt(relayPeerID, listen string) error {
	return n.startVPN(relayPeerID, listen)
}

// CheckSites tells which sites open directly and which through an exit, with a real TLS
// handshake per site. exitPeerID is a peer, "auto" (the exit in use, else the best ranked one) or
// "" (only the direct check).
func (n *NodeApi) CheckSites(exitPeerID string, sites []string) ([]coreNode.SiteCheck, error) {
	if n == nil || n.node == nil || n.node.Swarm == nil {
		return nil, errors.New("node is not running")
	}
	var exit *types.PeerID
	if exitPeerID == "auto" {
		n.vpn.mu.Lock()
		exitPeerID = n.vpn.relayPeer
		n.vpn.mu.Unlock()
		if exitPeerID == "" {
			best, err := n.bestExit()
			if err != nil {
				return nil, err
			}
			exitPeerID = best
		}
	}
	if exitPeerID != "" {
		raw, err := hex.DecodeString(exitPeerID)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("invalid peer id")
		}
		var id types.PeerID
		copy(id[:], raw)
		exit = &id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return n.node.CheckSites(ctx, exit, sites)
}
