package punch

import "net/netip"

// MaxTargets is the most targets a node accepts from its peer.
const MaxTargets = 4

// Rules decide which offered addresses may be punch targets (services.md §6.4 step 2).
type Rules struct {
	// PeerIP is the IP of the peer in PEER_ADDR (invalid when R sent none).
	PeerIP netip.Addr
	// OwnIP is this node's own IP in PEER_ADDR.
	OwnIP netip.Addr
	// CardIPs are the IPs of the peer's card addresses.
	CardIPs []netip.Addr
}

// FilterTargets returns at most MaxTargets addresses of offered that Rules allow: an address whose
// IP equals the IP of PEER_ADDR or appears in the peer's card; a private (LAN) address only when
// both PEER_ADDR IPs are equal, which means both ends sit behind one NAT. Loopback, link-local,
// multicast and unspecified addresses and port 0 are never targets (V4: a peer must not steer
// datagrams at anything it did not announce itself).
func FilterTargets(offered []netip.AddrPort, r Rules) []netip.AddrPort {
	peer, own := r.PeerIP.Unmap(), r.OwnIP.Unmap()
	sameNAT := peer.IsValid() && own.IsValid() && peer == own
	var out []netip.AddrPort
	seen := make(map[netip.AddrPort]struct{}, len(offered))
	for _, ap := range offered {
		a := ap.Addr().Unmap()
		if !a.IsValid() || ap.Port() == 0 || !routable(a) {
			continue
		}
		t := netip.AddrPortFrom(a, ap.Port())
		if _, dup := seen[t]; dup {
			continue
		}
		switch {
		case isLAN(a):
			if !sameNAT {
				continue
			}
		case a == peer:
		case inCard(a, r.CardIPs):
		default:
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) == MaxTargets {
			break
		}
	}
	return out
}

func routable(a netip.Addr) bool {
	return !(a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast())
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isLAN reports addresses that only mean something behind one NAT.
func isLAN(a netip.Addr) bool { return a.IsPrivate() || cgnat.Contains(a) }

func inCard(a netip.Addr, ips []netip.Addr) bool {
	for _, c := range ips {
		if c.Unmap() == a {
			return true
		}
	}
	return false
}
