package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// RelayLinkPrefix starts a relay link: "echofog://relay/<host:port>/<peer id>". It names a node by
// the address others reach it at, so a private relay (which does not advertise itself) or a
// friend's exit can be used without looking it up in the network.
const RelayLinkPrefix = "echofog://relay/"

const (
	maxAllowedPeers = 256
	maxRelays       = 16
)

// RelaySettingsDTO is what the user chooses about relaying. The server part is what this node does
// for others; Relays is which relays this node itself prefers.
type RelaySettingsDTO struct {
	Server        bool     `json:"server"`        // relay calls and messages for others
	Private       bool     `json:"private"`       // serve only AllowedPeers, do not advertise
	SignalingOnly bool     `json:"signalingOnly"` // only help peers connect directly; relay no traffic
	AllowedPeers  []string `json:"allowedPeers"`  // peer IDs a private relay serves
	Relays        []string `json:"relays"`        // "host:port/<peer id>" (or ws URL) to connect to and prefer
	// ListenPort fixes the port this node listens on, so its relay link stays the same after a
	// restart; 0 picks a free port at every start. Takes effect after a restart.
	ListenPort int `json:"listenPort"`
}

// RelayStatus is the relay settings together with what they produce right now.
type RelayStatus struct {
	Settings RelaySettingsDTO `json:"settings"`
	// Chosen: the user saved relay settings. False means the defaults are in effect, which make
	// every desktop a public relay.
	Chosen bool `json:"chosen"`
	// Link is the relay link to give to others, empty when they could not reach this node.
	Link   string `json:"link"`
	Addr   string `json:"addr"`
	NAT    string `json:"nat"`
	Mapped bool   `json:"mapped"`
	Reason string `json:"reason"` // why there is no link (English; the UI uses Code)
	Code   string `json:"code"`   // reason code the UI translates: connecting|nomap|cgnat|symmetric
	// RestartNeeded: the listen port was changed and is not in use yet.
	RestartNeeded bool `json:"restartNeeded"`

	ActiveCircuits int    `json:"activeCircuits"`
	BytesRouted    uint64 `json:"bytesRouted"`
	Forwarded      uint64 `json:"forwarded"`
	AccessRefused  uint64 `json:"accessRefused"`

	Using []RelayUse `json:"using"`
}

// RelayUse is one relay from Settings.Relays and whether this node is connected to it.
type RelayUse struct {
	Addr      string `json:"addr"`
	PeerID    string `json:"peerId"`
	Connected bool   `json:"connected"`
}

// ParseRelayAddrSNI reads a relay link, a "host:port/<peer id>" address or a ws:// URL ending in the
// peer ID, and returns the address, the peer ID, and the camouflage SNI (if present).
func ParseRelayAddrSNI(s string) (addr string, id types.PeerID, sni string, err error) {
	info, err := ParseRelayAddrInfo(s)
	return info.Addr, info.ID, info.SNI, err
}

// ParseRelayAddrInfo is ParseRelayAddrSNI with everything that travels with the address: the
// camouflage SNI and whether the node runs in the server role. A list that is saved and read back
// (bootstrap nodes, preferred relays) must keep both, or its entries are dialed over noise.
func ParseRelayAddrInfo(s string) (network.AddrInfo, error) {
	var zero network.AddrInfo
	s = strings.TrimSpace(s)
	if len(s) >= len(RelayLinkPrefix) && strings.EqualFold(s[:len(RelayLinkPrefix)], RelayLinkPrefix) {
		s = s[len(RelayLinkPrefix):]
	}
	s = strings.TrimRight(s, "/")
	info := network.ParseAddrInfo(s)
	a := info.Addr
	if !info.Named {
		return zero, errors.New("a relay address needs the peer ID at the end: host:port/<64 hex characters>")
	}
	if strings.HasPrefix(a, "ws://") || strings.HasPrefix(a, "wss://") {
		if len(a) <= len("wss://") {
			return zero, errors.New("the ws:// address has no host")
		}
		return info, nil
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil || host == "" {
		return zero, fmt.Errorf("%q is not host:port", a)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return zero, fmt.Errorf("%q is not a port", port)
	}
	return info, nil
}

// ParseRelayAddr reads a relay link, a "host:port/<peer id>" address or a ws:// URL ending in the
// peer ID, and returns the address and the peer.
func ParseRelayAddr(s string) (addr string, id types.PeerID, err error) {
	a, id, _, err := ParseRelayAddrSNI(s)
	return a, id, err
}

// MakeRelayLink is the link form of a "host:port/<peer id>" address.
func MakeRelayLink(addr string) string { return RelayLinkPrefix + addr }

// normalizeRelaySettings checks every entry and puts it in its canonical form (lower-case peer
// IDs, relay links turned into addresses, no duplicates).
func normalizeRelaySettings(r RelaySettingsDTO) (RelaySettingsDTO, error) {
	if r.ListenPort < 0 || r.ListenPort > 65535 {
		return r, errors.New("the listen port must be between 1 and 65535, or 0 for any free port")
	}
	peers, err := normalizePeerIDs(r.AllowedPeers)
	if err != nil {
		return r, err
	}
	seen := map[string]bool{}
	var relays []string
	for _, s := range r.Relays {
		if strings.TrimSpace(s) == "" {
			continue
		}
		info, err := ParseRelayAddrInfo(s)
		if err != nil {
			return r, err
		}
		if k := network.JoinPeerAddrInfo(info.Addr, info.ID, info.SNI, info.Server); !seen[k] {
			seen[k] = true
			relays = append(relays, k)
		}
	}
	if len(relays) > maxRelays {
		return r, fmt.Errorf("at most %d relays", maxRelays)
	}
	r.AllowedPeers, r.Relays = peers, relays
	if r.Relays == nil {
		r.Relays = []string{}
	}
	return r, nil
}

// normalizePeerIDs checks a list of peer IDs and returns it lower-case, without blanks or
// duplicates (never nil).
func normalizePeerIDs(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := types.PeerIDFromString(strings.ToLower(s))
		if err != nil {
			return nil, fmt.Errorf("%q is not a peer ID (64 hex characters)", s)
		}
		if k := id.String(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) > maxAllowedPeers {
		return nil, fmt.Errorf("at most %d peers", maxAllowedPeers)
	}
	return out, nil
}

func relaySettingsFromConfig(c *config.AppConfig) RelaySettingsDTO {
	rs := c.Relay.Server
	r := RelaySettingsDTO{
		Server:        rs.Enabled,
		Private:       rs.Private,
		SignalingOnly: rs.SignalingOnly,
		AllowedPeers:  append([]string{}, rs.AllowedPeers...),
		Relays:        append([]string{}, c.Relay.Client.Relays...),
	}
	if _, port, err := net.SplitHostPort(c.Network.ListenAddr); err == nil {
		r.ListenPort, _ = strconv.Atoi(port)
	}
	return r
}

func applyRelaySettings(c *config.AppConfig, r RelaySettingsDTO) {
	c.Relay.Server.Enabled = r.Server || r.SignalingOnly
	c.Relay.Server.SignalingOnly = r.SignalingOnly
	c.Relay.Server.Private = r.Private
	c.Relay.Server.AllowedPeers = append([]string{}, r.AllowedPeers...)
	c.Relay.Client.Relays = append([]string{}, r.Relays...)
	host, _, err := net.SplitHostPort(c.Network.ListenAddr)
	if err != nil {
		host = ""
	}
	c.Network.ListenAddr = net.JoinHostPort(host, strconv.Itoa(r.ListenPort))
}

// GetRelaySettings returns the relay settings in effect.
func (n *NodeApi) GetRelaySettings() (RelaySettingsDTO, error) {
	if n.node == nil || n.node.Cfg == nil {
		return RelaySettingsDTO{}, errors.New("node not started")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return relaySettingsFromConfig(n.node.Cfg), nil
}

// SetRelaySettings saves the relay settings and applies them at once, except the listen port,
// which is used from the next start.
func (n *NodeApi) SetRelaySettings(r RelaySettingsDTO) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	r, err := normalizeRelaySettings(r)
	if err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.Relay = &r }); err != nil {
		return err
	}
	applyRelaySettings(n.node.Cfg, r)
	n.relayChosen = true
	n.node.ApplyRelayConfig()
	return nil
}

// AddRelay adds a relay link or address to the preferred relays and connects to it.
func (n *NodeApi) AddRelay(link string) error {
	cur, err := n.GetRelaySettings()
	if err != nil {
		return err
	}
	if link, err = linkEntry(link); err != nil {
		return err
	}
	info, err := ParseRelayAddrInfo(link)
	if err != nil {
		return err
	}
	if n.node.ID == info.ID {
		return errors.New("this is your own relay")
	}
	cur.Relays = append(cur.Relays, network.JoinPeerAddrInfo(info.Addr, info.ID, info.SNI, info.Server))
	return n.SetRelaySettings(cur)
}

// GetRelayStatus returns the relay settings, the link to share and what the relay is doing.
func (n *NodeApi) GetRelayStatus() (RelayStatus, error) {
	cur, err := n.GetRelaySettings()
	if err != nil {
		return RelayStatus{}, err
	}
	settingsMu.Lock()
	st := RelayStatus{Settings: cur, Chosen: n.relayChosen}
	st.RestartNeeded = n.startPort != cur.ListenPort
	settingsMu.Unlock()

	reach := n.node.RelayReachability()
	st.Addr, st.NAT, st.Mapped, st.Reason, st.Code = reach.Addr, reach.NAT, reach.Mapped, reach.Reason, reach.Code
	if st.Addr != "" {
		st.Link = MakeRelayLink(st.Addr)
	}
	if rs := n.node.RelayServer; rs != nil {
		s := rs.Stats()
		st.ActiveCircuits, st.BytesRouted, st.Forwarded, st.AccessRefused = s.ActiveCircuits, s.BytesRouted, s.Forwarded, s.AccessRefused
	}
	st.Using = []RelayUse{}
	for _, r := range cur.Relays {
		a, id, ok := network.SplitPeerAddr(r)
		if !ok {
			continue
		}
		st.Using = append(st.Using, RelayUse{Addr: a, PeerID: id.String(), Connected: n.node.Swarm != nil && n.node.Swarm.GetPeer(id) != nil})
	}
	return st, nil
}

// StartVPNVia connects the proxy through one particular node: an invitation link, a relay link,
// "host:port/<peer id>" or a bare peer ID (then looked up in the network). Use it for a friend's
// exit that lets you in for free, a hidden exit, or an exit that is not in the market list.
func (n *NodeApi) StartVPNVia(target string) error {
	if n.node == nil || n.node.Swarm == nil {
		return errors.New("node is not running")
	}
	target = strings.TrimSpace(target)
	if access.IsLink(target) {
		id, err := n.JoinInvite(target)
		if err != nil {
			return err
		}
		return n.StartVPN(id)
	}
	if id, err := types.PeerIDFromString(strings.ToLower(target)); err == nil {
		return n.StartVPN(id.String())
	}
	a, id, sni, err := ParseRelayAddrSNI(target)
	if err != nil {
		return err
	}
	if id == n.node.ID {
		return errors.New("this is your own node")
	}
	resolved := coreNode.ResolveRelayHost(a)
	if sni != "" {
		if n.node.Net != nil {
			n.node.Net.ExpectPeerSNI(resolved, id, sni)
		}
		if n.node.Swarm != nil {
			n.node.Swarm.SetPeerSNI(id, sni)
		}
	}
	if n.node.Swarm.GetPeer(id) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := n.node.Swarm.AddPotentialPeerWithCtx(ctx, id, resolved); err != nil {
			return fmt.Errorf("could not reach %s: %w", a, err)
		}
	}
	return n.StartVPN(id.String())
}
