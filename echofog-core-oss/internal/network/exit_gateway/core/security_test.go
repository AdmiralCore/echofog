package core

import (
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
)

// The October SSRF fixes must survive the move: a public exit reaches neither loopback, LAN,
// link-local, CGNAT or reserved space, nor the operator's own public address, and it does not
// open SMTP, SMB/RPC or SSDP.
func TestTargetBlockingSurvivesTheMove(t *testing.T) {
	e := NewExitService()
	e.AllowPrivateTargets(false)
	e.SetSelfIPs(func() []net.IP { return []net.IP{net.ParseIP("8.8.8.8")} })

	for _, target := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.1.1",
		"100.64.0.1", "198.18.0.1", "192.0.2.1", "::1", "fe80::1", "fc00::1",
		"8.8.8.8", // the exit's own public address
	} {
		if !e.targetBlocked(net.ParseIP(target)) {
			t.Errorf("%s must be blocked", target)
		}
	}
	for _, target := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if e.targetBlocked(net.ParseIP(target)) {
			t.Errorf("%s must be allowed", target)
		}
	}
}

func TestDeniedPortsSurviveTheMove(t *testing.T) {
	e := NewExitService()
	e.AllowPrivateTargets(false)
	for _, port := range []int{25, 465, 587, 135, 137, 138, 139, 445, 1900} {
		if !e.portDenied(port) {
			t.Errorf("port %d must be denied", port)
		}
	}
	for _, port := range []int{53, 80, 443, 8080} {
		if e.portDenied(port) {
			t.Errorf("port %d must be allowed", port)
		}
	}
	e.AllowPrivateTargets(true)
	if e.portDenied(25) {
		t.Error("private-target mode (tests) lifts the port policy")
	}
}

func TestLimitsFromConfigSurvivesTheMove(t *testing.T) {
	l := LimitsFromConfig(config.ExitConfig{})
	if l.MaxStreams != 4096 || l.PerPeer != 256 || l.PerIP != 512 || l.UDPPerPeer != 32 {
		t.Fatalf("unexpected default limits: %+v", l)
	}
	if l.Idle != 5*time.Minute {
		t.Fatalf("unexpected idle limit: %v", l.Idle)
	}
}
