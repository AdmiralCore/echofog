package node

import (
	"net"
	"strconv"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/p2p/routing"
)

// ApplyRelayConfig puts a changed n.Cfg.Relay into effect on a running node: the relay server's
// mode and allow list, the client's preferred relays (newly named ones are dialed), and what the
// node advertises to peers it connects to from now on. Peers already connected keep the advert
// they were given until they reconnect.
func (n *Node) ApplyRelayConfig() {
	if n.RelayServer != nil {
		n.RelayServer.SetPolicy(n.Cfg.Relay.Server)
	}
	if n.RelayClient != nil {
		n.RelayClient.SetPolicy(n.Cfg.Relay.Client)
		if n.Swarm != nil {
			n.dialConfiguredRelays()
		}
	}
	caps := n.announcedCaps()
	if n.Net != nil {
		n.Net.SetCaps(caps)
	}
	if n.WS != nil {
		n.WS.SetCaps(caps)
	}
	if n.Swarm != nil {
		n.Swarm.SetPadsTunnels(caps.Has(handshake.CapTunnelPadding))
	}
}

// ResolveRelayHost resolves the host of a relay address so a domain name works; a ws:// URL or an
// unresolvable value is returned unchanged.
func ResolveRelayHost(addr string) string { return resolveRelayHost(addr) }

// RelayReachability says whether other people can connect to this node by an address, which is
// what a relay link needs. addr is "host:port/<peer id>" when they can; otherwise reason tells why
// not (behind a NAT without a port forward, still checking, ...).
type RelayReachability struct {
	Addr   string `json:"addr"`
	NAT    string `json:"nat"`    // public, cone, symmetric, unknown
	Mapped bool   `json:"mapped"` // the router forwards a port to us (UPnP / NAT-PMP)
	Reason string `json:"reason"`
	// Code is why there is no link, as a stable value the UI translates and maps to advice:
	// "" (reachable), "connecting", "nomap" (router does not forward a port), "cgnat" (behind a
	// carrier-grade or double NAT), "symmetric" (NAT changes address per peer).
	Code string `json:"code"`
}

// RelayReachability works out the address others can reach this node at, from what NAT discovery
// and the router port mapping found.
func (n *Node) RelayReachability() RelayReachability {
	if n.NAT == nil {
		return RelayReachability{NAT: "unknown", Code: "connecting", Reason: "The node is not connected to the network yet."}
	}
	pub, t := n.NAT.GetNATProfile()
	r := RelayReachability{NAT: t.String()}
	ext := pub
	if m := n.NAT.PortMapping(); m.IsValid() {
		r.Mapped, ext = true, m.String()
	}
	if (t == routing.NATPublic || r.Mapped) && ext != "" {
		if host, port, err := net.SplitHostPort(ext); err == nil {
			if !r.Mapped && n.Cfg.Network.AdvertisePort > 0 {
				port = strconv.Itoa(n.Cfg.Network.AdvertisePort)
			}
			// the link people paste into --bootstrap or a relay list: with the cover site's name and
			// the server role, so the first dial of whoever uses it is not noise
			r.Addr = network.JoinPeerAddrInfo(net.JoinHostPort(host, port), n.ID, n.Cfg.AntiDPI.TLSServerName, n.Cfg.IsServer())
			return r
		}
	}
	switch {
	case t == routing.NATUnknown:
		r.Code = "connecting"
		r.Reason = "Still finding out how this computer is connected to the Internet. Try again in a minute."
	case n.NAT.CarrierNAT():
		r.Code = "cgnat"
		r.Reason = "This computer is behind a carrier-grade or double NAT: the router itself has no public address, so forwarding a port cannot help. Ask the Internet provider for a public (\"white\") IP, or run the relay on a computer with a public IP (a VPS)."
	case t == routing.NATSymmetric:
		r.Code = "symmetric"
		r.Reason = "This computer is behind a NAT that changes its address for every peer, and the router does not forward a port to it, so nobody can connect to it directly. Run the relay on a computer with a public IP (a VPS), or forward a port on the router."
	default:
		r.Code = "nomap"
		r.Reason = "This computer is behind a router that does not forward a port to it, so others cannot connect to it. Turn on UPnP or NAT-PMP on the router, or forward the port by hand, or run the relay on a computer with a public IP."
	}
	return r
}

// publicIPs are the addresses the Internet reaches this node at: what NAT discovery observed and
// what the router's port mapping reports. The exit refuses them as destinations.
func (n *Node) publicIPs() []net.IP {
	if n.NAT == nil {
		return nil
	}
	var out []net.IP
	add := func(hostport string) {
		host, _, err := net.SplitHostPort(hostport)
		if err != nil {
			host = hostport
		}
		if ip := net.ParseIP(host); ip != nil {
			out = append(out, ip)
		}
	}
	if pub, _ := n.NAT.GetNATProfile(); pub != "" {
		add(pub)
	}
	if m := n.NAT.PortMapping(); m.IsValid() {
		add(m.String())
	}
	return out
}
