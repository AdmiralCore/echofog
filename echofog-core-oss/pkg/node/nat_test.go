package node

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	"github.com/stretchr/testify/assert"
)

func nodeWithNAT(t *testing.T, listen string, obs ...string) *Node {
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = listen
	m := routing.NewNATManager(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.SetObserver(func(context.Context) []natcheck.Observation {
		var out []natcheck.Observation
		for i := 0; i+1 < len(obs); i += 2 {
			out = append(out, natcheck.Observation{From: obs[i], Addr: netip.MustParseAddrPort(obs[i+1])})
		}
		return out
	})
	_, _ = m.Discover()
	return &Node{Cfg: cfg, NAT: m}
}

// A node that others cannot reach must not be handed viewers to relay for: the links to them
// could never be built and the call would break.
func TestCallRelayCapacityFollowsReachability(t *testing.T) {
	cone := nodeWithNAT(t, "0.0.0.0:4242", "a", "203.0.113.7:5000", "b", "203.0.113.7:5000")
	assert.Equal(t, 2, cone.CallRelayCapacity(), "behind a cone NAT: only peers it can punch through to")

	cone.NAT.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:4242"))
	assert.Equal(t, 4, cone.CallRelayCapacity(), "a forwarded port makes it a normal relay")

	sym := nodeWithNAT(t, "0.0.0.0:4242", "a", "203.0.113.7:5000", "b", "203.0.113.7:5999")
	assert.Equal(t, 0, sym.CallRelayCapacity(), "behind a symmetric NAT: leaf only")

	unknown := nodeWithNAT(t, "0.0.0.0:4242")
	assert.Equal(t, 0, unknown.CallRelayCapacity(), "until the NAT is known, stay a leaf")
}

func TestLocalNodesAndUnmeasuredNodesKeepTheDefault(t *testing.T) {
	assert.Equal(t, 4, nodeWithNAT(t, "127.0.0.1:0").CallRelayCapacity(), "loopback simulations have no NAT")
	assert.Equal(t, 4, (&Node{Cfg: config.DefaultConfig("")}).CallRelayCapacity(), "no NAT manager at all")
}

func TestOnlyGloballyRoutableAddressesCountAsReachable(t *testing.T) {
	for addr, want := range map[string]bool{
		"203.0.113.7":    true,
		"2001:db8::1":    true,
		"192.168.1.1":    false, // the router itself sits behind another NAT
		"10.0.0.1":       false,
		"100.64.1.1":     false, // carrier-grade NAT
		"169.254.1.1":    false,
		"127.0.0.1":      false,
		"0.0.0.0":        false,
		"224.0.0.1":      false,
		"::ffff:8.8.8.8": true,
	} {
		assert.Equal(t, want, publicIP(netip.MustParseAddr(addr)), addr)
	}
	assert.False(t, publicIP(netip.Addr{}))
}
