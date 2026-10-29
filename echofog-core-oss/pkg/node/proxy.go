package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The node behind a proxy (network.proxy, a SOCKS5 proxy such as tor or Tor Browser). Its own
// connections leave through the proxy, over TCP (see internal/network/proxy.go), and nothing it
// does shows its address: no STUN, port mapping or hole punching, and a user's node is reached
// through a relay, not dialed. Calls are the exception the user can choose (network.call_route):
// Tor carries no UDP and is too slow for a video call, so a call may take a direct link anyway, at
// the price of the other side seeing this computer's address.

// applyProxy sets the node's outbound proxy from the config; it runs before the first dial.
func (n *Node) applyProxy() error {
	if err := network.SetOutboundProxy(n.Cfg.Network.Proxy); err != nil {
		return err
	}
	if p := network.OutboundProxy(); p != "" {
		n.Logger.Info("PROXY: the node's connections go through the SOCKS5 proxy, over TCP only", "proxy", p, "calls", n.CallRoute())
	}
	return nil
}

// ProxyOn reports whether the node's connections go through a proxy.
func (n *Node) ProxyOn() bool { return n.Cfg != nil && n.Cfg.Network.Proxy != "" }

// CallRoute is how calls reach the other side now: "direct", "relay", or "auto" (a direct link
// where one can be made, which guard mode turns into a relay across the border). With a proxy on,
// "auto" is "relay": the proxy is there to hide the address.
func (n *Node) CallRoute() string {
	if n.Cfg == nil {
		return config.CallAuto
	}
	switch r := n.Cfg.Network.CallRoute; r {
	case config.CallDirect, config.CallRelay:
		return r
	}
	if n.ProxyOn() {
		return config.CallRelay
	}
	return config.CallAuto
}

// natShowsAddr reports whether the NAT services may run: STUN, port mapping and hole punching all
// show this computer's address. Behind a proxy they run only for direct calls.
func (n *Node) natShowsAddr() bool {
	return !n.ProxyOn() || n.CallRoute() == config.CallDirect
}

// connectForCall gives a call its link to peer, the way CallRoute says.
func (n *Node) connectForCall(ctx context.Context, peer types.PeerID) error {
	switch n.CallRoute() {
	case config.CallRelay:
		if n.Swarm.GetPeer(peer) != nil {
			return nil // a link that is already up (to a server, or one the user dialed)
		}
		if n.RelayClient == nil {
			return errors.New("no relay to carry the call")
		}
		_, err := n.RelayClient.OpenCircuitTo(ctx, peer)
		return err
	case config.CallDirect:
		ctx = network.WithDirect(ctx)
	}
	err := n.EnsureConnected(ctx, peer)
	if err != nil && n.RelayClient != nil {
		// no direct link (a symmetric NAT): a relay circuit, open before the first frame needs it,
		// so the call's watchdogs do not take the wait for a dead peer
		_, err = n.RelayClient.OpenCircuitTo(ctx, peer)
	}
	return err
}

// admitPunchBehindProxy decides a hole punch request while the node is behind a proxy: answering
// gives out this computer's addresses, so only a contact calling while calls are set to direct
// gets one.
func (n *Node) admitPunchBehindProxy(from types.PeerID) bool {
	return n.CallRoute() == config.CallDirect && n.IsContact(from)
}
