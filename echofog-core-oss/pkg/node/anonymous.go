package node

import (
	"errors"
	"net"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
)

// Anonymous mode (network.anonymous): this computer's address is shown to nobody in the network
// ("tor": everything through the Tor proxy), or to the user's own relay alone ("relay": the node
// connects to that relay and nothing else, see network.SetDialOnly and the swarm's pinned-only
// guards). Calls and files go through relays, no STUN, port mapping or hole punch runs, and the
// app's own lookups either go through the proxy or do not happen. A mode that cannot hold (Tor
// with no proxy set, a relay mode with no relay pinned) fails closed: the node connects nowhere.

// ErrAnonymousWallet: anonymous mode keeps the TON wallet offline, since its servers would see
// this computer's address.
var ErrAnonymousWallet = errors.New("anonymous mode: the TON wallet does not connect to its servers (allow it in Settings: they will see your address)")

// AnonMode is how this computer's address is hidden: config.AnonTor, config.AnonRelay, or "".
func (n *Node) AnonMode() string {
	if n.Cfg == nil {
		return ""
	}
	switch m := n.Cfg.Network.Anonymous; m {
	case config.AnonTor, config.AnonRelay:
		return m
	}
	return ""
}

// AnonBroken reports what keeps the chosen anonymous mode from working ("" when it can): the node
// then connects nowhere.
func (n *Node) AnonBroken() string {
	switch n.AnonMode() {
	case config.AnonTor:
		if !n.ProxyOn() {
			return "no Tor proxy is set"
		}
	case config.AnonRelay:
		if len(n.Cfg.AntiDPI.PinnedGuards) == 0 {
			return "no relay is pinned to go in through"
		}
	}
	return ""
}

// applyAnonymity sets the network rules of anonymous mode; it runs before the first dial.
func (n *Node) applyAnonymity() {
	mode := n.AnonMode()
	if mode == "" {
		network.SetDialOnly(nil)
		tonwallet.SetNetworkGate(nil)
		return
	}
	if why := n.AnonBroken(); why != "" {
		network.SetDialOnly([]string{}) // nowhere, rather than in the open
		n.Logger.Error("ANONYMOUS: the mode cannot hold, the node connects nowhere", "mode", mode, "why", why)
	} else if mode == config.AnonRelay {
		var hosts []string
		for _, e := range n.Cfg.AntiDPI.PinnedGuards {
			info := network.ParseAddrInfo(e)
			if h, _, err := net.SplitHostPort(info.Addr); err == nil {
				hosts = append(hosts, h)
			}
		}
		network.SetDialOnly(hosts)
		n.Logger.Info("ANONYMOUS: the node connects only to its own relay", "relays", len(hosts))
	} else {
		network.SetDialOnly(nil)
		n.Logger.Info("ANONYMOUS: everything goes through Tor", "proxy", network.OutboundProxy())
	}
	if n.Cfg.Network.AnonWalletDirect {
		tonwallet.SetNetworkGate(nil)
	} else {
		tonwallet.SetNetworkGate(func() error { return ErrAnonymousWallet })
	}
}
