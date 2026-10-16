package network

import (
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// AddrPolicy decides which peer addresses may be handed to other nodes (peer exchange, DHT
// answers) or kept for dialing later. A private, loopback or CGNAT address means something only
// on the network it came from: given to a node elsewhere it points at a host of that node's own
// LAN, or nowhere.
type AddrPolicy struct {
	// AllowLoopback accepts 127.0.0.0/8 and ::1: for test setups where every node runs on one
	// machine.
	AllowLoopback bool
	// AllowPrivate accepts private (RFC 1918, ULA), link-local and CGNAT addresses: for networks
	// built of private addresses only, such as a LAN or a Docker test cluster.
	AllowPrivate bool
}

// PolicyFor is the policy of a node listening on listenAddr: one listening on loopback only runs
// in a test with its peers on the same machine, and accepts loopback addresses.
func PolicyFor(listenAddr string, allowPrivate bool) AddrPolicy {
	p := AddrPolicy{AllowPrivate: allowPrivate}
	if host, _, err := net.SplitHostPort(listenAddr); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
			p.AllowLoopback = true
		}
	}
	return p
}

// Shareable reports whether addr ("host:port", or a ws:// or wss:// URL) is worth giving to
// another node. A host name is: it resolves wherever it is used.
func (p AddrPolicy) Shareable(addr string) bool {
	if addr == "" {
		return false
	}
	host := addr
	if isWSURL(addr) {
		u, err := url.Parse(addr)
		if err != nil || u.Hostname() == "" {
			return false
		}
		host = u.Hostname()
	} else {
		h, port, err := net.SplitHostPort(addr)
		if err != nil || port == "" || port == "0" {
			return false
		}
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return !strings.EqualFold(host, "localhost") || p.AllowLoopback
	}
	return p.allowsIP(ip.Unmap())
}

func (p AddrPolicy) allowsIP(ip netip.Addr) bool {
	switch {
	case ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast():
		return false
	case ip.IsLoopback():
		return p.AllowLoopback
	case ip.IsPrivate() || ip.IsLinkLocalUnicast() || isCGNATAddr(ip):
		return p.AllowPrivate
	}
	if ip.Is4() {
		b := ip.As4()
		if b[0] == 0 || b[0] >= 240 { // "this network", reserved and broadcast
			return false
		}
	}
	return true
}

func isCGNATAddr(ip netip.Addr) bool {
	return ip.Is4() && ip.As4()[0] == 100 && ip.As4()[1]&0xC0 == 64
}

func isWSURL(addr string) bool {
	return strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://")
}
