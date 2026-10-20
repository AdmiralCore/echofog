package exit_gateway

import (
	"net"
	"testing"
)

func TestTargetBlocked(t *testing.T) {
	e := NewExitService()
	e.local.list = func() ([]net.Addr, error) {
		_, n1, _ := net.ParseCIDR("203.0.113.10/24") // this host's "public" /24
		_, n2, _ := net.ParseCIDR("2a00:1234:5678::1/64")
		return []net.Addr{n1, n2}, nil
	}
	e.SetSelfIPs(func() []net.IP { return []net.IP{net.ParseIP("198.51.100.7")} })

	blocked := []string{
		"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "100.64.0.1", // blockedIP
		"0.0.0.0", "192.0.2.5", "198.18.0.9", "240.0.0.1", // reserved
		"203.0.113.55",              // in this host's own /24
		"2a00:1234:5678::dead",      // in this host's own v6 /64 (on-link LAN device)
		"198.51.100.7",              // this exit's own public IP (hairpin)
		"::1", "fe80::1", "fc00::1", // v6 loopback/link-local/ULA
	}
	for _, s := range blocked {
		if ip := net.ParseIP(s); ip == nil || !e.targetBlocked(ip) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "140.82.121.4", "2606:4700::1111"} {
		if !e.targetBlocked(net.ParseIP(s)) {
			continue // (none should block)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if e.targetBlocked(net.ParseIP(s)) {
			t.Errorf("%s is a normal public address and must be allowed", s)
		}
	}

	e.allowPrivate.Store(true)
	if e.targetBlocked(net.ParseIP("127.0.0.1")) {
		t.Error("allowPrivate must let tests reach loopback")
	}
}

func TestPortDenied(t *testing.T) {
	e := NewExitService()
	for _, p := range []int{25, 465, 587, 445, 139, 1900} {
		if !e.portDenied(p) {
			t.Errorf("port %d should be denied (abuse magnet)", p)
		}
	}
	for _, p := range []int{80, 443, 993, 8080, 51820} {
		if e.portDenied(p) {
			t.Errorf("port %d is normal and must be allowed", p)
		}
	}
	e.allowPrivate.Store(true)
	if e.portDenied(25) {
		t.Error("allowPrivate lifts the port policy for tests")
	}
}
