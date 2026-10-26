package node

import (
	"net"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/config"
)

// Region-aware obfuscation. REALITY (QUIC with a camouflage SNI) mimics an allowed foreign site so
// a connection survives a censoring border. A connection that never crosses one — a peer in this
// node's own country, or on the local network — gains nothing from that mimicry, so it is dialed
// over the cheaper noise obfuscation instead. The signal reuses the GeoIP of geoip.go: this node's
// own country and the peer's country, placed from their public addresses. When either country
// cannot be placed the peer stays on REALITY, so a cross-border link is never weakened by a missing
// lookup (steps 2/3 are untouched).

// applyRegionObfs installs the region policy on the transport after the node has started. It is a
// no-op when region-aware obfuscation is off, when obfuscation itself is off (nothing distinguishes
// noise from REALITY then), or before the transport exists.
func (n *NodeApi) applyRegionObfs() {
	cfg := n.node.Cfg
	if cfg == nil || !cfg.AntiDPI.RegionAwareObfs || cfg.AntiDPI.Obfs == config.ObfsOff {
		return
	}
	if n.node.Net == nil {
		return
	}
	n.node.Net.SetRegionNoise(n.peerInHomeRegion)
}

// peerInHomeRegion reports whether the node reachable at addr ("host:port") is in this node's own
// region, so a camouflaged dial to it may use noise rather than REALITY.
func (n *NodeApi) peerInHomeRegion(addr string) bool {
	// A private, loopback, link-local or CGNAT address is on the local network: no border is
	// crossed, so noise is enough.
	if ip := hostIP(addr); ip != nil &&
		(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || isCGNATAddr(ip)) {
		return true
	}
	peerIP := geoIP(addr)
	if peerIP == "" {
		return false // not placeable (unresolved, or a reserved range): keep REALITY
	}
	home := n.homeCountry()
	if home == "" {
		return false
	}
	peerCC := n.countryOf(peerIP)
	if peerCC == "" {
		return false
	}
	return strings.EqualFold(peerCC, home)
}

// homeCountry is this node's own country: the configured override if set, else its own public
// address placed by GeoIP.
func (n *NodeApi) homeCountry() string {
	if cc := strings.ToUpper(strings.TrimSpace(n.node.Cfg.AntiDPI.HomeCountry)); cc != "" {
		return cc
	}
	if n.node.NAT == nil {
		return ""
	}
	ip := geoIP(n.node.NAT.GetPublicAddr())
	if ip == "" {
		return ""
	}
	return n.countryOf(ip)
}

// countryOf returns the ISO country code known for a public IP from the GeoIP cache, triggering a
// background lookup (for next time) when it is not cached yet.
func (n *NodeApi) countryOf(ip string) string {
	st := n.historyStorage()
	if e, _ := cachedGeo(loadGeoCache(st), ip, time.Now()); e.CountryCode != "" {
		return e.CountryCode
	}
	n.geo().refresh(st, []string{ip}, nil)
	return ""
}

// hostIP parses the IP of "host:port" (or a bare host); nil when host is a name or does not parse.
func hostIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.ParseIP(host)
}
