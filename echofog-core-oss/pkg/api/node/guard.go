package node

import (
	"errors"
	"strings"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
)

// maxPinnedGuards caps how many relays the user may pin.
const maxPinnedGuards = 8

// How this node enters the EchoFog network. In guard mode a user's node does not build a mesh of
// its own across a censoring border: it keeps a few camouflaged guard servers and goes through
// them (peer exchange, DHT lookups, relayed traffic), so its address is not spread and it opens no
// noise links a censor can flag. See docs/anti-dpi.md ("Guards") and internal/p2p/swarm.
//
// It does not change the VPN exit: which exit carries your browsing is chosen on the Proxy page,
// and that path has its own disguise (a camouflaged exit, REALITY/TLS). Guard mode is about how
// the node itself joins and talks to the network.

// GuardServer is one of the node's guards.
type GuardServer struct {
	PeerID    string `json:"peerId"`
	Addr      string `json:"addr,omitempty"`
	Connected bool   `json:"connected"`
	Pinned    bool   `json:"pinned,omitempty"` // the user pinned this relay as a way in
}

// GuardStatus is how the node enters the network and which guards it holds.
type GuardStatus struct {
	// Mode is "auto" (on for a normal app, off in the home region when allowed), "on" (always) or
	// "off" (the node dials the mesh directly, the old way).
	Mode string `json:"mode"`
	// Active: the node holds guards now (mode decided, not a server).
	Active bool `json:"active"`
	// Connected is how many guards are up; Wanted how many it aims to hold.
	Connected int           `json:"connected"`
	Wanted    int           `json:"wanted"`
	Guards    []GuardServer `json:"guards"`
	// Country is the node's own country (ISO code, "" not known yet). StoodDown: "auto" holds no
	// guards because that country does not censor (guard_auto.go).
	Country   string `json:"country,omitempty"`
	StoodDown bool   `json:"stoodDown,omitempty"`
	// Pinned are the relays the user chose to always route through (their own relay, or a trusted
	// one). Each is always held as a guard; the node leans on them first.
	Pinned []BootstrapNode `json:"pinned"`
}

// GetGuardStatus reports how the node enters the network and its guards.
func (n *NodeApi) GetGuardStatus() (GuardStatus, error) {
	if n.node == nil || n.node.Cfg == nil {
		return GuardStatus{}, errors.New("node not started")
	}
	mode := n.node.Cfg.AntiDPI.GuardMode
	if mode == "" {
		mode = config.GuardAuto
	}
	st := GuardStatus{Mode: mode, Guards: []GuardServer{}, Pinned: []BootstrapNode{}}
	pinnedIDs := map[string]bool{}
	for _, e := range n.node.Cfg.AntiDPI.PinnedGuards {
		st.Pinned = append(st.Pinned, n.bootstrapNode(e))
		if _, id, ok := network.SplitPeerAddr(e); ok {
			pinnedIDs[id.String()] = true
		}
	}
	if n.node.Swarm == nil {
		return st, nil
	}
	st.Active = n.node.Swarm.GuardModeActive()
	st.Country, st.StoodDown = n.homeCountry(), n.node.Swarm.GuardsFree() && mode == config.GuardAuto && !st.Active
	for _, g := range n.node.Swarm.Guards() {
		pid := g.ID.String()
		st.Guards = append(st.Guards, GuardServer{PeerID: pid, Addr: g.Addr, Connected: g.Connected, Pinned: pinnedIDs[pid]})
		if g.Connected {
			st.Connected++
		}
	}
	if st.Active {
		st.Wanted = n.node.Swarm.GuardsWanted()
	}
	return st, nil
}

// SetGuardMode saves how the node enters the network and applies it: "auto", "on" or "off". It
// takes effect for new dials at once and fully as the node settles on its guards.
func (n *NodeApi) SetGuardMode(mode string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node is not running")
	}
	if mode != config.GuardAuto && mode != config.GuardOn && mode != config.GuardOff {
		return errors.New("guard mode must be auto, on or off")
	}
	settingsMu.Lock()
	err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.GuardMode = mode })
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	n.node.Cfg.AntiDPI.GuardMode = mode // GuardModeActive reads this live
	if n.node.Swarm != nil {
		n.node.Swarm.RefreshGuards()
	}
	return nil
}

// normalizePinnedGuards parses the user's pinned relays (a relay link or host:port/<peer id>) into
// their canonical "host:port/<peer id>?sni=…&srv=…" form, dropping duplicates. A pinned relay must
// carry a camouflage SNI: the node dials it as REALITY and holds it as a guard, and a guard without
// a cover site is neither disguised nor eligible.
func normalizePinnedGuards(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		info, err := ParseRelayAddrInfo(s)
		if err != nil {
			return nil, err
		}
		if info.SNI == "" {
			return nil, errors.New("a pinned relay needs a cover site (sni) so the node can reach it in disguise")
		}
		e := network.JoinPeerAddrInfo(info.Addr, info.ID, info.SNI, info.Server)
		if !seen[strings.ToLower(e)] {
			seen[strings.ToLower(e)] = true
			out = append(out, e)
		}
	}
	if len(out) > maxPinnedGuards {
		return nil, errors.New("too many pinned relays")
	}
	return out, nil
}

// PinGuard saves a relay to always route through (a relay link or host:port/<peer id>?sni=…) and
// starts holding it as a guard at once. The node leans on pinned relays first, so its mesh traffic
// goes through them.
func (n *NodeApi) PinGuard(entry string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	entry, err := linkEntry(entry)
	if err != nil {
		return err
	}
	parsed, err := normalizePinnedGuards([]string{entry})
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return errors.New("enter a relay address")
	}
	e := parsed[0]
	if _, id, ok := network.SplitPeerAddr(e); ok && id == n.node.ID {
		return errors.New("this is your own node")
	}

	settingsMu.Lock()
	list, err := normalizePinnedGuards(append(append([]string(nil), n.node.Cfg.AntiDPI.PinnedGuards...), e))
	if err == nil {
		err = saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.PinnedGuards = list })
	}
	if err == nil {
		n.node.Cfg.AntiDPI.PinnedGuards = list // the swarm reads this live
	}
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	if n.node.Swarm != nil {
		n.node.Swarm.RefreshGuards()
	}
	return nil
}

// UnpinGuard forgets one of the user's pinned relays (by any accepted form of it). It keeps its
// place in the guard rotation until it expires like any other, then makes room for another.
func (n *NodeApi) UnpinGuard(entry string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	parsed, err := normalizePinnedGuards([]string{entry})
	if err != nil || len(parsed) == 0 {
		return errors.New("not a relay address")
	}
	drop := strings.ToLower(parsed[0])

	settingsMu.Lock()
	keep := make([]string, 0, len(n.node.Cfg.AntiDPI.PinnedGuards))
	for _, b := range n.node.Cfg.AntiDPI.PinnedGuards {
		if strings.ToLower(b) != drop {
			keep = append(keep, b)
		}
	}
	err = saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.PinnedGuards = keep })
	if err == nil {
		n.node.Cfg.AntiDPI.PinnedGuards = keep
	}
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	if n.node.Swarm != nil {
		n.node.Swarm.RefreshGuards()
	}
	return nil
}
