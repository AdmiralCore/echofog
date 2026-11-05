package session_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestReservedReportedAddressesAreIgnored: only a routable public address can be the node's
// public address. A node behind CGNAT must not publish 100.64.x.x, a documentation range or a
// translation prefix in its card.
func TestReservedReportedAddressesAreIgnored(t *testing.T) {
	reserved := []string{
		"0.1.2.3:1", "10.1.1.1:1", "100.64.0.1:1", "100.127.255.254:1", "127.0.0.1:1",
		"169.254.1.1:1", "172.16.0.1:1", "172.31.255.1:1", "192.0.0.9:1", "192.0.2.1:1",
		"192.88.99.1:1", "192.168.1.1:1", "198.18.0.1:1", "198.19.255.1:1", "198.51.100.1:1",
		"203.0.113.1:1", "224.0.0.1:1", "240.0.0.1:1", "255.255.255.255:1",
		"[::1]:1", "[::]:1", "[fe80::1]:1", "[fc00::1]:1", "[fec0::1]:1", "[ff02::1]:1",
		"[2001:db8::1]:1", "[2001::1]:1", "[2002::1]:1", "[64:ff9b::808:808]:1", "[100::1]:1",
		"[::ffff:10.0.0.1]:1", "[3fff::1]:1",
	}
	for _, rep := range reserved {
		r := newObsRig(t)
		if v := r.o.Observe(obs(1, "8.8.4.4", rep, "s")); v != session.ObsIgnored {
			t.Errorf("reported %s: verdict %v, want ignored", rep, v)
		}
		if r.o.Len() != 0 {
			t.Errorf("reported %s was stored", rep)
		}
	}
}

func TestRoutableReportedAddressesAreAccepted(t *testing.T) {
	for _, rep := range []string{"8.8.8.8:1", "93.184.216.34:1", "[2606:4700::1111]:1", "[2a00:1450::1]:1"} {
		r := newObsRig(t)
		if v := r.o.Observe(obs(1, "9.9.9.9", rep, "s")); v != session.ObsAccepted {
			t.Errorf("reported %s: verdict %v, want accepted", rep, v)
		}
	}
}
