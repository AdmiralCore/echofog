package exit_gateway

import (
	"net"
	"sync"
	"time"
)

// What a public exit refuses to connect to, beyond loopback, private, link-local, multicast and
// CGNAT space (blockedIP). A client's traffic must reach the Internet, never the operator's own
// machine, its LAN or the provider's internal network.

// reservedNets are special-purpose ranges: unroutable, documentation, benchmarking (also the fake
// range TUN mode answers DNS with), NAT64 and 6to4 (both embed an IPv4 address, which could be a
// private one) and the deprecated site-local IPv6 range.
var reservedNets = mustCIDRs(
	"0.0.0.0/8", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "240.0.0.0/4",
	"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "2002::/16", "fec0::/10",
)

// deniedPorts are destination ports a public exit does not open. SMTP gets an operator's IP onto
// spam blocklists and their ISP account closed; NetBIOS/SMB/RPC and SSDP are scanning and worm
// targets that no ordinary browsing needs.
var deniedPorts = map[int]bool{25: true, 465: true, 587: true, 135: true, 137: true, 138: true, 139: true, 445: true, 1900: true}

func mustCIDRs(list ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(list))
	for _, s := range list {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// localNets caches the subnets of this machine's own interfaces. On a VPS the public address is
// on an interface, so its services bound to 0.0.0.0 are covered; at home, every device of an IPv6
// LAN has a global address in the interface's /64, reachable on-link past the router's firewall.
type localNets struct {
	mu      sync.Mutex
	nets    []*net.IPNet
	fetched time.Time
	list    func() ([]net.Addr, error)
}

const localNetsTTL = 30 * time.Second

func (l *localNets) contains(ip net.IP) bool {
	l.mu.Lock()
	if time.Since(l.fetched) > localNetsTTL {
		l.nets, l.fetched = nil, time.Now()
		list := l.list
		if list == nil {
			list = net.InterfaceAddrs
		}
		if addrs, err := list(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					l.nets = append(l.nets, n)
				}
			}
		}
	}
	nets := l.nets
	l.mu.Unlock()
	return inNets(ip, nets)
}

// SetSelfIPs tells the exit which public addresses it is reached at (what NAT discovery and the
// router mapping found). A client connecting to one of them would come back in through the
// router (hairpin NAT) and reach its admin page or forwarded services.
func (e *ExitService) SetSelfIPs(fn func() []net.IP) {
	e.policyMu.Lock()
	e.selfIPs = fn
	e.policyMu.Unlock()
}

// targetBlocked reports whether a client may not reach ip.
func (e *ExitService) targetBlocked(ip net.IP) bool {
	if e.allowPrivate.Load() {
		return false
	}
	if blockedIP(ip) || inNets(ip, reservedNets) || e.local.contains(ip) {
		return true
	}
	e.policyMu.Lock()
	self := e.selfIPs
	e.policyMu.Unlock()
	if self != nil {
		for _, s := range self() {
			if s.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// portDenied reports whether a client may not reach this destination port.
func (e *ExitService) portDenied(port int) bool {
	return !e.allowPrivate.Load() && deniedPorts[port]
}
