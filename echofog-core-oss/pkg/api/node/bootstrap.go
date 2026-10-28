package node

import (
	"errors"
	"strings"

	"github.com/echofog/echofog-core/internal/network"
)

// maxBootstrapNodes caps the user's own bootstrap list.
const maxBootstrapNodes = 32

// BootstrapNode is one node the app joins the network through.
type BootstrapNode struct {
	Addr      string `json:"addr"`   // host:port as given (a name stays a name)
	PeerID    string `json:"peerId"` // 64 hex
	Link      string `json:"link"`   // echofog://relay/… form, for sharing
	Connected bool   `json:"connected"`
}

// BootstrapStatus lists the nodes built into this release and the ones the user added. When the
// built-in ones are blocked (or a release ships none), a node address from a friend or from the
// project's channel gets the app into the network.
type BootstrapStatus struct {
	BuiltIn []BootstrapNode `json:"builtIn"`
	Custom  []BootstrapNode `json:"custom"`
	Peers   int             `json:"peers"` // peers connected right now
}

// normalizeBootstrap parses each entry (a relay link or host:port/<peer id>) into its canonical
// "host:port/<peer id>" form, dropping duplicates.
func normalizeBootstrap(in []string) ([]string, error) {
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
		// keep the SNI and the server flag: they are what lets the first dial be REALITY
		e := network.JoinPeerAddrInfo(info.Addr, info.ID, info.SNI, info.Server)
		if !seen[strings.ToLower(e)] {
			seen[strings.ToLower(e)] = true
			out = append(out, e)
		}
	}
	if len(out) > maxBootstrapNodes {
		return nil, errors.New("too many bootstrap nodes")
	}
	return out, nil
}

// bootstrapList is the built-in nodes plus the user's, once each. The node's maintainer dials it,
// so a node added later is retried when the app goes offline.
func bootstrapList(builtIn, custom []string) []string {
	have := map[string]bool{}
	list := make([]string, 0, len(builtIn)+len(custom))
	for _, b := range append(append([]string(nil), builtIn...), custom...) {
		if !have[strings.ToLower(b)] {
			have[strings.ToLower(b)] = true
			list = append(list, b)
		}
	}
	return list
}

func (n *NodeApi) bootstrapNode(entry string) BootstrapNode {
	a, id, ok := network.SplitPeerAddr(entry)
	if !ok {
		return BootstrapNode{Addr: entry}
	}
	return BootstrapNode{
		Addr: a, PeerID: id.String(), Link: MakeRelayLink(entry),
		Connected: n.node.Swarm != nil && n.node.Swarm.GetPeer(id) != nil,
	}
}

// GetBootstrapNodes returns the built-in and the user's bootstrap nodes and which are connected.
func (n *NodeApi) GetBootstrapNodes() (BootstrapStatus, error) {
	if n.node == nil || n.node.Cfg == nil {
		return BootstrapStatus{}, errors.New("node not started")
	}
	settingsMu.Lock()
	custom := append([]string(nil), n.bootstrapCustom...)
	builtIn := append([]string(nil), n.bootstrapBuiltIn...)
	settingsMu.Unlock()

	st := BootstrapStatus{BuiltIn: []BootstrapNode{}, Custom: []BootstrapNode{}}
	for _, b := range custom {
		st.Custom = append(st.Custom, n.bootstrapNode(b))
	}
	for _, b := range builtIn {
		st.BuiltIn = append(st.BuiltIn, n.bootstrapNode(b))
	}
	if n.node.Swarm != nil {
		st.Peers = len(n.node.Swarm.GetAllPeers())
	}
	return st, nil
}

// AddBootstrapNode saves a node to join the network through (a relay link or host:port/<peer id>)
// and dials it at once. The peer ID is required: every node's traffic is obfuscated with a key
// derived from its ID, so an address alone cannot be dialed.
func (n *NodeApi) AddBootstrapNode(entry string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	parsed, err := normalizeBootstrap([]string{entry})
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return errors.New("enter a node address")
	}
	e := parsed[0]
	if _, id, ok := network.SplitPeerAddr(e); ok && id == n.node.ID {
		return errors.New("this is your own node")
	}

	settingsMu.Lock()
	list, err := normalizeBootstrap(append(append([]string(nil), n.bootstrapCustom...), e))
	if err == nil {
		err = saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.BootstrapNodes = list })
	}
	if err == nil {
		n.bootstrapCustom = list
		n.node.SetBootstrapNodes(bootstrapList(n.bootstrapBuiltIn, list))
	}
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	if n.node.Swarm != nil {
		n.node.Swarm.Bootstrap([]string{e})
	}
	return nil
}

// RemoveBootstrapNode forgets one of the user's bootstrap nodes (by any accepted form of it).
func (n *NodeApi) RemoveBootstrapNode(entry string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	parsed, err := normalizeBootstrap([]string{entry})
	if err != nil || len(parsed) == 0 {
		return errors.New("not a node address")
	}
	drop := strings.ToLower(parsed[0])

	settingsMu.Lock()
	defer settingsMu.Unlock()
	keep := make([]string, 0, len(n.bootstrapCustom))
	for _, b := range n.bootstrapCustom {
		if strings.ToLower(b) != drop {
			keep = append(keep, b)
		}
	}
	if err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.BootstrapNodes = keep }); err != nil {
		return err
	}
	n.bootstrapCustom = keep
	n.node.SetBootstrapNodes(bootstrapList(n.bootstrapBuiltIn, keep))
	return nil
}
