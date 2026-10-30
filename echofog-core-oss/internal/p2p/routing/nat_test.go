package routing

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func seen(pairs ...string) Observer {
	return func(context.Context) []natcheck.Observation {
		var out []natcheck.Observation
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, natcheck.Observation{From: pairs[i], Addr: netip.MustParseAddrPort(pairs[i+1])})
		}
		return out
	}
}

func discover(t *testing.T, o Observer) *NATManager {
	m := NewNATManager(nil, quiet())
	m.SetObserver(o)
	_, err := m.Discover()
	require.NoError(t, err)
	return m
}

func TestConeNATCanBePunchedButNotBeARouter(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50000"))
	addr, typ := m.GetNATProfile()
	assert.Equal(t, "203.0.113.7:50000", addr)
	assert.Equal(t, NATFullCone, typ)
	assert.True(t, m.CanBePunched())
	assert.False(t, m.CanBeRouter(), "a cone NAT lets answers in, not strangers")
}

func TestSymmetricNATCannotBePunched(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50777"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
	assert.False(t, m.CanBePunched())
	assert.False(t, m.CanBeRouter())
}

// A symmetric NAT whose router forwards our port: that port is one public address for everybody,
// so peers reach us there (it was taken for unreachable, and nothing was even tried).
func TestForwardedPortBeatsASymmetricNAT(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50777"))
	m.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:49829"))
	assert.True(t, m.CanBePunched())
	assert.True(t, m.CanBeRouter())
	assert.Equal(t, "203.0.113.7:49829", m.Candidates()[0])
}

func TestOneObserverIsWorthATry(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000"))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATUnknown, typ)
	assert.True(t, m.CanBePunched(), "unknown is not a reason to give up")
	assert.False(t, m.CanBeRouter())
}

func TestAForwardedPortMakesUsReachableEvenBehindNAT(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50000"))
	require.False(t, m.CanBeRouter())

	m.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:4242"))
	assert.True(t, m.CanBeRouter())
	assert.Equal(t, []string{"203.0.113.7:4242", "203.0.113.7:50000"}, m.Candidates(), "the forwarded address first")

	m.SetPortMapping(netip.AddrPort{})
	assert.False(t, m.CanBeRouter(), "the router dropped the forward")
}

func TestCandidatesDoNotRepeatTheSameAddress(t *testing.T) {
	m := discover(t, seen("a", "203.0.113.7:4242", "b", "203.0.113.7:4242"))
	m.SetPortMapping(netip.MustParseAddrPort("203.0.113.7:4242"))
	assert.Equal(t, []string{"203.0.113.7:4242"}, m.Candidates())
}

func TestNoNATWhenTheObservedAddressIsOurOwn(t *testing.T) {
	local := natcheck.LocalAddrs()
	var v4 netip.Addr
	for _, a := range local {
		if a.Is4() {
			v4 = a
			break
		}
	}
	if !v4.IsValid() {
		t.Skip("no IPv4 interface address on this machine")
	}
	ap := netip.AddrPortFrom(v4, 4242).String()
	m := discover(t, seen("a", ap, "b", ap))
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATPublic, typ)
	assert.True(t, m.CanBeRouter())
}

func TestDiscoverFailsClearlyWhenNobodyAnswers(t *testing.T) {
	m := NewNATManager(nil, quiet())
	_, err := m.Discover()
	assert.Error(t, err)
	m.SetObserver(seen())
	_, err = m.Discover()
	assert.Error(t, err)
	_, typ := m.GetNATProfile()
	assert.Equal(t, NATUnknown, typ, "a failed check must not invent a type")
}

func TestTheTypeFollowsTheNetworkWhenItChanges(t *testing.T) {
	obs := seen("a", "203.0.113.7:50000", "b", "203.0.113.7:50000")
	m := NewNATManager(nil, quiet())
	m.SetObserver(func(ctx context.Context) []natcheck.Observation { return obs(ctx) })
	_, _ = m.Discover()
	_, typ := m.GetNATProfile()
	require.Equal(t, NATFullCone, typ)

	obs = seen("a", "203.0.113.7:50000", "b", "198.51.100.1:51000") // moved to a carrier NAT
	_, _ = m.Discover()
	_, typ = m.GetNATProfile()
	assert.Equal(t, NATSymmetric, typ)
}

func TestSTUNServersCanBeOverriddenAndEmptyKeepsDefaults(t *testing.T) {
	m := NewNATManager(nil, quiet())
	m.SetSTUNServers(nil)
	assert.Equal(t, natcheck.DefaultSTUNServers, m.servers)
	m.SetSTUNServers([]string{"stun.example.net:3478"})
	assert.Equal(t, []string{"stun.example.net:3478"}, m.servers)
}
