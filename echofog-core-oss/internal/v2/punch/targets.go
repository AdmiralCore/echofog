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

// FilterTargets returns at most MaxTargets addresses of offered that Rules allow.
func FilterTargets(offered []netip.AddrPort, r Rules) []netip.AddrPort { return nil }
