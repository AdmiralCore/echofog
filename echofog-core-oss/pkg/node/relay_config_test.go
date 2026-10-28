package node

import (
	"log/slog"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// A relay link is only offered when others can actually connect: a router port mapping (or a
// public address); behind a NAT without one, the reason is given instead.
func TestRelayReachability(t *testing.T) {
	n := &Node{Cfg: config.DefaultConfig("t"), ID: types.PeerID{1}}
	r := n.RelayReachability()
	assert.Empty(t, r.Addr)
	assert.NotEmpty(t, r.Reason)

	n.NAT = routing.NewNATManager(nil, slog.Default())
	r = n.RelayReachability()
	assert.Empty(t, r.Addr, "nothing known yet")
	assert.Equal(t, "unknown", r.NAT)
	assert.NotEmpty(t, r.Reason)

	n.NAT.SetPortMapping(netip.MustParseAddrPort("203.0.113.5:40001"))
	r = n.RelayReachability()
	assert.True(t, r.Mapped)
	assert.Equal(t, "203.0.113.5:40001/"+n.ID.String(), r.Addr)
	assert.Empty(t, r.Reason)

	n.NAT.SetPortMapping(netip.AddrPort{})
	r = n.RelayReachability()
	assert.Empty(t, r.Addr)
	assert.NotEmpty(t, r.Reason, "the forward is gone: no link")
}

// The address a node offers for pasting carries its cover site and its role; announced caps say
// "server" only for the server role, and a hidden node says nothing at all.
func TestOwnAddressAndCapsCarryTheServerFacts(t *testing.T) {
	n := &Node{Cfg: config.DefaultConfig("t"), ID: types.PeerID{1}}
	n.NAT = routing.NewNATManager(nil, slog.Default())
	n.NAT.SetPortMapping(netip.MustParseAddrPort("203.0.113.5:40001"))
	assert.NotContains(t, n.RelayReachability().Addr, "srv=1")
	assert.False(t, n.announcedCaps().Has(handshake.CapServer))

	n.Cfg.Role = config.RoleServer
	n.Cfg.AntiDPI.TLSServerName = "cover.example"
	assert.Equal(t, "203.0.113.5:40001/"+n.ID.String()+"?sni=cover.example&srv=1", n.RelayReachability().Addr)
	assert.True(t, n.announcedCaps().Has(handshake.CapServer))

	n.Cfg.Access.Mode = config.AccessHidden
	assert.Equal(t, handshake.CapNone, n.announcedCaps(), "a hidden node announces nothing")
}
