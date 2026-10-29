package swarm

import (
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// In guard mode a direct link from across the border is refused, except from the peers the node
// keeps direct links with anyway; at home, on the local network and with guard mode off it is taken.
func TestInboundAcrossBorderInGuardMode(t *testing.T) {
	abroad := netip.MustParseAddr("198.51.100.7")
	peer := idOf(1)
	cases := []struct {
		name    string
		setup   func(s *Swarm)
		ip      netip.Addr
		caps    handshake.Capability
		refused bool
	}{
		{"a node abroad: refused", func(s *Swarm) {}, abroad, handshake.DefaultCaps, true},
		{"a node on the local network: taken", func(s *Swarm) {}, netip.MustParseAddr("192.168.1.8"), handshake.DefaultCaps, false},
		{"a node in the home region: taken", func(s *Swarm) { s.SetSameRegionFunc(func(string) bool { return true }) }, abroad, handshake.DefaultCaps, false},
		{"a node in another region: refused", func(s *Swarm) { s.SetSameRegionFunc(func(string) bool { return false }) }, abroad, handshake.DefaultCaps, true},
		{"a server by its handshake: taken", func(s *Swarm) {}, abroad, handshake.DefaultCaps | handshake.CapServer, false},
		{"a server by role: taken", func(s *Swarm) { s.SetPeerServer(peer, true) }, abroad, handshake.DefaultCaps, false},
		{"a node the user listed: taken", func(s *Swarm) { s.MarkExplicitPeer(peer) }, abroad, handshake.DefaultCaps, false},
		{"a node the owner invited: taken", func(s *Swarm) { s.SetInboundInvited(func(id types.PeerID) bool { return id == peer }) }, abroad, handshake.DefaultCaps, false},
		{"guard mode off: taken", func(s *Swarm) { withGuardMode(s, config.GuardOff) }, abroad, handshake.DefaultCaps, false},
		{"auto in a free country: taken", func(s *Swarm) { s.SetGuardsFree(true) }, abroad, handshake.DefaultCaps, false},
		{"a server of ours: taken", func(s *Swarm) { withRole(s, config.RoleServer) }, abroad, handshake.DefaultCaps, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestSwarm(t)
			c.setup(s)
			ev := network.NewConnEvent{PeerID: peer, RemoteIP: c.ip, Caps: c.caps}
			assert.Equal(t, c.refused, s.inboundAcrossBorderRefused(ev))
			if c.refused {
				_, err := s.admitInbound(ev)
				assert.ErrorIs(t, err, errInboundAcrossBorder)
			}
		})
	}
}
