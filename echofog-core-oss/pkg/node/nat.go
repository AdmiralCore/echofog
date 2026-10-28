package node

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/portmap"
	"github.com/echofog/echofog-core/internal/p2p/holepunch"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	portMapRetry   = 15 * time.Minute // a router that was off, or UPnP just enabled, is found later
	portMapRefresh = 20 * time.Second
)

// relaySignaler carries hole-punch coordination messages through relays when the peers are not
// directly connected yet.
type relaySignaler struct{ rc *routing.RelayClient }

func (r relaySignaler) Send(ctx context.Context, to types.PeerID, payload []byte) error {
	// a signaling circuit: relays and signaling-only nodes both pass it on
	return r.rc.SendSignal(ctx, to, net_types.TypeHolePunch, payload)
}

// startNAT works out what our NAT does and, unless told not to, helps peers reach us: it asks the
// router to forward our port and enables direct connections between peers behind NATs.
func (n *Node) startNAT() {
	var conn net.PacketConn
	if n.Transport.STUNSharesQUICPort() {
		conn = n.Transport.GetSTUNConn()
	} // else: STUN from another socket would report another port's mapping
	n.NAT = routing.NewNATManager(conn, n.Logger)
	n.NAT.SetSTUNServers(n.Cfg.Network.STUNServers)
	n.NAT.SetObserver(func(ctx context.Context) []natcheck.Observation {
		return n.Swarm.ObserveFromPeers(ctx, 4)
	})

	if n.loopbackOnly() {
		return // simulations and tests: there is no NAT to look at
	}
	go n.NAT.StartKeepAlive(n.Ctx)
	if n.Cfg.Network.PortMapping {
		go n.runPortMapping()
	}
	if n.Cfg.Network.HolePunch && !n.isolated() {
		n.startHolePunch()
	}
}

func (n *Node) loopbackOnly() bool { return strings.HasPrefix(n.Cfg.Network.ListenAddr, "127.0.0.1") }

// runPortMapping asks the home router (UPnP, NAT-PMP) to forward our QUIC and TCP ports, keeps the
// mapping alive, and tells the NAT manager the address it produced.
func (n *Node) runPortMapping() {
	for n.Ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(n.Ctx, portmap.DiscoveryTimeout)
		pm, err := portmap.DiscoverNAT(dctx)
		cancel()
		if err != nil {
			n.Logger.Debug("no UPnP or NAT-PMP router found", "err", err)
			select {
			case <-n.Ctx.Done():
				return
			case <-time.After(portMapRetry):
				continue
			}
		}
		n.maintainMapping(pm)
		return
	}
}

func (n *Node) maintainMapping(pm *portmap.NAT) {
	defer pm.Close()
	udpPort := n.Transport.UDPPort()
	if err := pm.AddMapping(n.Ctx, "udp", udpPort); err != nil {
		n.Logger.Warn("port mapping failed", "err", err)
		return
	}
	tcpPort := 0
	if a := n.Net.TCPAddr(); a != nil {
		if ap, err := netip.ParseAddrPort(a.String()); err == nil {
			tcpPort = int(ap.Port())
			_ = pm.AddMapping(n.Ctx, "tcp", tcpPort)
		}
	}
	n.Logger.Info("router forwards our port", "udp", udpPort, "tcp", tcpPort)

	t := time.NewTicker(portMapRefresh)
	defer t.Stop()
	for {
		ext, ok := pm.GetMapping("udp", udpPort)
		// a router sitting behind another NAT (carrier-grade, double NAT) reports a private
		// address: the forward then leads nowhere from the Internet, so it must not count
		carrier := ok && !publicIP(ext.Addr())
		if carrier {
			// the router answered, but its own WAN address is private or 100.64: double/carrier NAT
			ok = false
		}
		n.NAT.SetCarrierNAT(carrier)
		if ok {
			n.NAT.SetPortMapping(ext)
		} else {
			n.NAT.SetPortMapping(netip.AddrPort{})
		}
		// peers we dial reach us back through the router's forward, which may use another port;
		// a configured advertise_port (a port map the operator set up) wins
		if n.Cfg.Network.AdvertisePort == 0 {
			if ok {
				n.Transport.SetAdvertisedPort(int(ext.Port()))
			} else {
				n.Transport.SetAdvertisedPort(0)
			}
		}
		select {
		case <-n.Ctx.Done():
			return
		case <-t.C:
		}
	}
}

func publicIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() {
		return false
	}
	if a.Is4() {
		b := a.As4()
		if b[0] == 100 && b[1]&0xC0 == 64 { // 100.64.0.0/10: carrier-grade NAT
			return false
		}
	}
	return true
}

// startHolePunch wires the direct-connection service: it listens for coordination messages (from
// relays and from direct peers) and is used by connectToPeer when a plain dial fails.
func (n *Node) startHolePunch() {
	svc := holepunch.New(holepunch.Config{
		Transport:  n.Transport,
		Signal:     relaySignaler{n.RelayClient},
		OwnAddrs:   n.NAT.Candidates,
		LocalAddrs: n.localAddrs,
		Connected:  func(p types.PeerID) bool { return n.Swarm.GetPeer(p) != nil },
		Ticket:     func(p types.PeerID) []byte { return n.Invites.Ticket(p, n.ID) },
		Admit:      n.admitPunch,
	})
	n.HolePunch = svc
	n.Swarm.RegisterFastFrameHandler(net_types.TypeHolePunch, func(payload []byte, from types.PeerID) error {
		svc.Handle(from, payload)
		return nil
	})
	n.RelayClient.AddDataListener(func(_ uint32, msgType net_types.MessageType, payload []byte, remote types.PeerID) {
		if msgType == net_types.TypeHolePunch {
			svc.Handle(remote, payload)
		}
	})
}

// connectToPeer opens a connection to a peer: a plain dial first (works when it is reachable),
// then, when both sides are behind NATs, a hole punch coordinated through a relay.
func (n *Node) connectToPeer(ctx context.Context, peer types.PeerID, addr string) error {
	if n.Swarm.GetPeer(peer) != nil {
		return nil
	}
	var dialErr error
	if addr != "" {
		dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		dialErr = n.Swarm.AddPotentialPeerWithCtx(dctx, peer, addr)
		cancel()
		if dialErr == nil {
			return nil
		}
		if errors.Is(dialErr, swarm.ErrNoiseDialRefused) {
			// held back on purpose: a hole punch is the same noise toward the same address
			return dialErr
		}
	}
	if n.HolePunch == nil || !n.NAT.CanBePunched() {
		return orDialErr(dialErr)
	}
	if err := n.HolePunch.Connect(ctx, peer); err != nil {
		if dialErr != nil {
			return dialErr
		}
		return err
	}
	return nil
}

func orDialErr(err error) error {
	if err != nil {
		return err
	}
	return context.DeadlineExceeded
}

// CallRelayCapacity is how many other participants this node may relay a call to. A node that others
// can reach (public address, or a forwarded port) relays for four; behind a cone NAT it can only
// serve peers it manages to punch through to, so two; behind a symmetric NAT, or while its NAT is
// still unknown, none: assigning it children would hand the call to links that cannot be built.
// CallRelayOnly: behind a symmetric NAT, so the other participants of a call reach this node only
// through relay circuits.
func (n *Node) CallRelayOnly() bool {
	if n.NAT == nil || n.loopbackOnly() {
		return false
	}
	_, t := n.NAT.GetNATProfile()
	return t == routing.NATSymmetric
}

func (n *Node) CallRelayCapacity() int {
	if n.NAT == nil || n.loopbackOnly() {
		return 4
	}
	_, t := n.NAT.GetNATProfile()
	switch {
	case n.NAT.CanBeRouter():
		return 4
	case t == routing.NATFullCone:
		return 2
	default:
		return 0
	}
}

// localAddrs are the addresses of this computer on private networks ("192.168.1.5:4242"): what a
// peer behind the same router can connect to when the router will not loop back its own address.
func (n *Node) localAddrs() []string {
	port := n.Transport.UDPPort()
	ifaces, err := net.InterfaceAddrs()
	if err != nil || port == 0 {
		return nil
	}
	var out []string
	for _, a := range ifaces {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok || !ip.Unmap().Is4() || !ip.Unmap().IsPrivate() {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip.Unmap(), uint16(port)).String())
	}
	return out
}
