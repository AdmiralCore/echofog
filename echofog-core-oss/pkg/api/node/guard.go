package node

import (
	"errors"

	"github.com/echofog/echofog-core/internal/config"
)

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
	st := GuardStatus{Mode: mode, Guards: []GuardServer{}}
	if n.node.Swarm == nil {
		return st, nil
	}
	st.Active = n.node.Swarm.GuardModeActive()
	st.Country, st.StoodDown = n.homeCountry(), n.node.Swarm.GuardsFree() && mode == config.GuardAuto
	for _, g := range n.node.Swarm.Guards() {
		st.Guards = append(st.Guards, GuardServer{PeerID: g.ID.String(), Addr: g.Addr, Connected: g.Connected})
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
