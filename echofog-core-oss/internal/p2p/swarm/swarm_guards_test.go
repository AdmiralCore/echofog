package swarm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withGuardMode(s *Swarm, mode string) {
	cfg := *s.cfg
	cfg.AntiDPI.GuardMode = mode
	s.cfg = &cfg
}

// guardServer makes a known camouflaged server at 198.<net>.0.<n>:4242 and returns its candidate.
func guardServer(s *Swarm, n, net byte) guardCandidate {
	id := idOf(100 + n)
	s.SetPeerServer(id, true)
	s.SetPeerSNI(id, "cover.example")
	return guardCandidate{id: id, addr: fmt.Sprintf("198.%d.0.%d:4242", net, n)}
}

func TestGuardModeActive(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *Swarm)
		want  bool
	}{
		{"default: auto, a user's node, obfuscation on", func(s *Swarm) {}, true},
		{"on", func(s *Swarm) { withGuardMode(s, config.GuardOn) }, true},
		{"off", func(s *Swarm) { withGuardMode(s, config.GuardOff) }, false},
		{"a server never has guards", func(s *Swarm) { withRole(s, config.RoleServer) }, false},
		{"auto with obfuscation off: nothing to hide behind", func(s *Swarm) {
			cfg := *s.cfg
			cfg.AntiDPI.Obfs = config.ObfsOff
			s.cfg = &cfg
		}, false},
		{"on with obfuscation off", func(s *Swarm) {
			withGuardMode(s, config.GuardOn)
			cfg := *s.cfg
			cfg.AntiDPI.Obfs = config.ObfsOff
			s.cfg = &cfg
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestSwarm(t)
			c.setup(s)
			assert.Equal(t, c.want, s.GuardModeActive())
		})
	}
}

// Guards come from the servers that have a camouflage SNI, three of them, spread over networks,
// and a restart keeps them.
func TestGuardSelectionAndPersistence(t *testing.T) {
	s, _ := newTestSwarm(t)
	var cands []guardCandidate
	for i := 0; i < 5; i++ {
		cands = append(cands, guardServer(s, byte(i), byte(10+i)))
	}
	// two in the network of the first one, which must not be picked while others are left
	cands = append(cands, guardServer(s, 20, cands[0].netByte()), guardServer(s, 21, cands[0].netByte()))

	now := time.Now()
	s.planGuards(now, cands, nil)
	gs := s.Guards()
	require.Len(t, gs, guardsWanted)
	nets := map[string]bool{}
	for _, g := range gs {
		nets[g.Addr[:7]] = true // 198.NN.
		assert.True(t, g.Expires.After(now.Add(guardLifeMin-time.Second)), "kept for weeks")
	}
	assert.Len(t, nets, guardsWanted, "three different networks")

	// a second round with all of them present changes nothing
	before := s.Guards()
	s.planGuards(now.Add(time.Minute), cands, nil)
	assert.Equal(t, before, s.Guards())

	// a restart: the same guards come back from storage
	s.guards = guardSet{}
	assert.Equal(t, ids(before), ids(s.Guards()))
}

func (c guardCandidate) netByte() byte {
	var a, b, d, e int
	_, _ = fmt.Sscanf(c.addr, "%d.%d.%d.%d", &a, &b, &d, &e)
	return byte(b)
}

func ids(gs []GuardInfo) []types.PeerID {
	out := make([]types.PeerID, len(gs))
	for i, g := range gs {
		out[i] = g.ID
	}
	return out
}

// Not eligible: no SNI (a node that could only be reached over noise), a user's node.
func TestGuardCandidatesNeedSNIAndServerRole(t *testing.T) {
	s, _ := newTestSwarm(t)
	good := guardServer(s, 1, 10)
	noSNI := idOf(50)
	s.SetPeerServer(noSNI, true)
	user := idOf(51)
	s.SetPeerSNI(user, "cover.example")
	s.SetPeerClient(user, 1)
	banned := guardServer(s, 2, 11)
	_ = banned

	list := []string{
		network.JoinPeerAddrInfo(good.addr, good.id, "cover.example", true),
		network.JoinPeerAddrInfo("198.51.100.50:4242", noSNI, "", true),
		network.JoinPeerAddrInfo("198.51.100.51:4242", user, "cover.example", false),
	}
	s.SetBootstrapNodes(list)
	got := s.guardCandidates()
	var gotIDs []types.PeerID
	for _, c := range got {
		gotIDs = append(gotIDs, c.id)
	}
	assert.Contains(t, gotIDs, good.id)
	assert.NotContains(t, gotIDs, noSNI)
	assert.NotContains(t, gotIDs, user)
}

// Rotation: a guard whose time is up is replaced, the others stay.
func TestGuardRotation(t *testing.T) {
	s, _ := newTestSwarm(t)
	cands := []guardCandidate{guardServer(s, 1, 10), guardServer(s, 2, 11), guardServer(s, 3, 12), guardServer(s, 4, 13)}
	now := time.Now()
	s.planGuards(now, cands, nil)
	first := s.Guards()
	require.Len(t, first, guardsWanted)

	// the first guard's time is up
	s.guards.mu.Lock()
	old := s.guards.list[0].id
	s.guards.list[0].expires = now.Add(-time.Hour)
	s.guards.mu.Unlock()

	s.planGuards(now.Add(time.Minute), cands, nil)
	after := s.Guards()
	require.Len(t, after, guardsWanted)
	assert.False(t, s.IsGuard(old), "the expired guard is gone")
	for _, g := range first[1:] {
		assert.True(t, s.IsGuard(g.ID), "the others stay")
	}
}

// Failover: a guard that is gone gets a stand-in, and is dropped for good only when it stays gone
// while the rest of the network works. With no link at all (our own network is down) nobody is dropped.
func TestGuardFailover(t *testing.T) {
	s, _ := newTestSwarm(t)
	cands := []guardCandidate{guardServer(s, 1, 10), guardServer(s, 2, 11), guardServer(s, 3, 12), guardServer(s, 4, 13), guardServer(s, 5, 14)}
	now := time.Now()
	s.planGuards(now, cands, nil) // nobody is connected, the three are picked
	require.Len(t, s.Guards(), guardsWanted)

	// one link is up (a guard connected)
	ctx := context.Background()
	p, _ := addPrimaryPeer(ctx, s, 3)
	s.SetPeerServer(p.ID(), true)
	s.SetPeerSNI(p.ID(), "cover.example")
	s.guards.mu.Lock()
	s.guards.list[0] = &guard{id: p.ID(), addr: "203.0.113.7:443", added: now, expires: now.Add(guardLifeMin), lastOK: now}
	s.guards.mu.Unlock()
	gone := s.Guards()[1].ID

	// within the grace the list is as it was
	s.planGuards(now.Add(guardDownAfter/2), cands, nil)
	assert.Len(t, s.Guards(), guardsWanted)

	// down for longer than the grace: stand-in added next to it, it stays
	s.planGuards(now.Add(2*guardDownAfter), cands, nil)
	require.True(t, s.IsGuard(gone))
	assert.Greater(t, len(s.Guards()), guardsWanted-1)
	assert.LessOrEqual(t, len(s.Guards()), guardsMax)

	// gone for good while another link is up: dropped
	s.planGuards(now.Add(guardDropAfter+time.Minute), cands, nil)
	assert.False(t, s.IsGuard(gone), "dropped when it stays gone")
	assert.True(t, s.IsGuard(p.ID()), "the connected guard stays")
}

func TestGuardsKeptWhileOwnNetworkIsDown(t *testing.T) {
	s, _ := newTestSwarm(t)
	cands := []guardCandidate{guardServer(s, 1, 10), guardServer(s, 2, 11), guardServer(s, 3, 12)}
	now := time.Now()
	s.planGuards(now, cands, nil)
	before := ids(s.Guards())
	s.planGuards(now.Add(3*guardDropAfter), cands, nil) // no link of any kind for hours
	assert.Equal(t, before, ids(s.Guards())[:len(before)], "nobody is dropped when no link works at all")
}

// A guard that announces it is a user's node, not a server, is dropped.
func TestGuardThatIsAUserIsDropped(t *testing.T) {
	s, _ := newTestSwarm(t)
	cands := []guardCandidate{guardServer(s, 1, 10), guardServer(s, 2, 11), guardServer(s, 3, 12)}
	now := time.Now()
	s.planGuards(now, cands, nil)
	s.peerSNIMu.Lock()
	f := s.peerFacts[cands[0].id]
	f.server, f.client = false, true
	s.peerFacts[cands[0].id] = f
	s.peerSNIMu.Unlock()
	s.planGuards(now.Add(time.Second), cands[:0], nil)
	assert.False(t, s.IsGuard(cands[0].id))
}

// The rule itself: what a dial the node starts on its own may reach.
func TestGuardDialRefused(t *testing.T) {
	const public = "198.51.100.7:4242"
	guardID, other := idOf(1), idOf(2)
	cases := []struct {
		name   string
		setup  func(s *Swarm)
		addr   string
		refuse bool
	}{
		{"a stranger across the border: refused", func(s *Swarm) {}, public, true},
		{"a guard: dialed", func(s *Swarm) { s.planGuards(time.Now(), []guardCandidate{{guardID, public}}, nil) }, public, false},
		{"on the local network: dialed", func(s *Swarm) {}, "192.168.1.5:4242", false},
		{"in the home region: dialed", func(s *Swarm) { s.SetSameRegionFunc(func(string) bool { return true }) }, public, false},
		{"another region: refused", func(s *Swarm) { s.SetSameRegionFunc(func(string) bool { return false }) }, public, true},
		{"a name whose country is unknown: refused", func(s *Swarm) {}, "peer.example.org:4242", true},
		{"mode off: dialed", func(s *Swarm) { withGuardMode(s, config.GuardOff) }, public, false},
		{"a server: dialed", func(s *Swarm) { withRole(s, config.RoleServer) }, public, false},
		{"no guard is up and the peer is camouflaged: the way back, dialed", func(s *Swarm) { s.SetPeerSNI(other, "cover.example") }, public, false},
		{"no guard is up, the address carries the SNI: dialed", func(s *Swarm) {}, public + "?sni=cover.example", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestSwarm(t)
			s.SetBootstrapNodes(nil)
			guardID := guardID
			if c.name == "a guard: dialed" {
				s.SetPeerServer(guardID, true)
				s.SetPeerSNI(guardID, "cover.example")
			}
			c.setup(s)
			id := other
			if c.name == "a guard: dialed" {
				id = guardID
			}
			assert.Equal(t, c.refuse, s.guardDialRefused(id, c.addr))
			err := s.DialMeshPeer(context.Background(), id, c.addr)
			if c.refuse {
				assert.ErrorIs(t, err, ErrGuardDialRefused)
			} else {
				assert.NotErrorIs(t, err, ErrGuardDialRefused)
			}
		})
	}
}

// With a guard connected, a camouflaged stranger is refused too: one link across the border, the
// guard's, is all there is.
func TestGuardDialRefusedForCamouflagedWhenGuardsAreUp(t *testing.T) {
	s, _ := newTestSwarm(t)
	p, _ := addPrimaryPeer(context.Background(), s, 4)
	s.SetPeerServer(p.ID(), true)
	s.SetPeerSNI(p.ID(), "cover.example")
	s.planGuards(time.Now(), []guardCandidate{{p.ID(), "203.0.113.7:443"}}, nil)
	require.True(t, s.IsGuard(p.ID()))
	require.Equal(t, 1, s.guardsUp())

	stranger := idOf(9)
	s.SetPeerServer(stranger, true)
	s.SetPeerSNI(stranger, "cover.example")
	assert.True(t, s.guardDialRefused(stranger, "198.51.100.8:4242"))
	assert.False(t, s.guardDialRefused(p.ID(), "203.0.113.7:443"))
}

// The maintainer and peer exchange go through dialCandidates: a candidate across the border that is
// not a guard is not dialed, and not blamed.
func TestDialCandidatesSkipsNonGuards(t *testing.T) {
	s, _ := newTestSwarm(t)
	pub := randomPub()
	id := types.PeerPubKeyToID(pub)
	require.True(t, s.peerStore.SaveDiscovered(pub, "198.51.100.9:4242"))
	start := time.Now()
	s.dialCandidates([]DialCandidate{{ID: id, Addr: "198.51.100.9:4242"}})
	assert.Less(t, time.Since(start), time.Second)
	e, err := s.peerStore.GetPeer(id)
	require.NoError(t, err)
	assert.Zero(t, e.GetFailCount())
}

// A node short of peers in guard mode is short until a guard is up.
func TestNeedsPeersInGuardMode(t *testing.T) {
	s, _ := newTestSwarm(t)
	assert.True(t, s.NeedsPeers())
	p, _ := addPrimaryPeer(context.Background(), s, 6)
	p.caps.Store(uint32(handshake.CapRelay | handshake.CapServer))
	s.SetPeerServer(p.ID(), true)
	s.SetPeerSNI(p.ID(), "cover.example")
	assert.True(t, s.NeedsPeers(), "a link that is not a guard is not enough")
	s.planGuards(time.Now(), []guardCandidate{{p.ID(), "203.0.113.7:443"}}, nil)
	assert.False(t, s.NeedsPeers(), "a connected guard that relays is")
}

// The user-to-user rule: which links between users are made through relays instead.
func TestUserLinkViaRelay(t *testing.T) {
	const public = "198.51.100.7:4242"
	user, server := idOf(1), idOf(2)
	cases := []struct {
		name  string
		setup func(s *Swarm)
		id    types.PeerID
		addr  string
		via   bool
	}{
		{"a user across the border: through a relay", func(s *Swarm) {}, user, public, true},
		{"a user, no address known: unknown is across", func(s *Swarm) {}, user, "", true},
		{"a user on the local network: direct", func(s *Swarm) {}, user, "192.168.1.8:4242", false},
		{"a user in the home region: direct", func(s *Swarm) { s.SetSameRegionFunc(func(string) bool { return true }) }, user, public, false},
		{"a user in another region: through a relay", func(s *Swarm) { s.SetSameRegionFunc(func(string) bool { return false }) }, user, public, true},
		{"a camouflaged server: direct (REALITY)", func(s *Swarm) { s.SetPeerSNI(server, "cover.example") }, server, public, false},
		{"a server by its address: direct", func(s *Swarm) {}, server, public + "?sni=cover.example&srv=1", false},
		{"a server known by role: left to the noise rule", func(s *Swarm) { s.SetPeerServer(server, true) }, server, public, false},
		{"a node the user listed (an invitation): direct", func(s *Swarm) { s.MarkExplicitPeer(user) }, user, public, false},
		{"mode off: direct", func(s *Swarm) { withGuardMode(s, config.GuardOff) }, user, public, false},
		{"a server of ours: direct", func(s *Swarm) { withRole(s, config.RoleServer) }, user, public, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestSwarm(t)
			c.setup(s)
			assert.Equal(t, c.via, s.UserLinkViaRelay(c.id, c.addr))
		})
	}
}

// A pinned relay is always held: it is not rotated out while pinned, and once unpinned it leaves
// like any other guard when its time is up.
func TestPinnedGuardAlwaysHeld(t *testing.T) {
	s, _ := newTestSwarm(t)
	withGuardMode(s, config.GuardOn)
	pin := guardServer(s, 9, 90)
	now := time.Now()

	s.planGuards(now, nil, []guardCandidate{pin})
	require.True(t, s.IsGuard(pin.id), "pinned guard is held")

	// its lifetime is up, but while pinned it is kept anyway
	s.guards.mu.Lock()
	for _, g := range s.guards.list {
		if g.id == pin.id {
			assert.True(t, g.pinned, "marked pinned")
			g.expires = now.Add(-time.Hour)
		}
	}
	s.guards.mu.Unlock()
	s.planGuards(now, nil, []guardCandidate{pin})
	assert.True(t, s.IsGuard(pin.id), "pinned guard kept past its expiry")

	// unpinned: the same expired guard is dropped on the next round
	s.planGuards(now, nil, nil)
	assert.False(t, s.IsGuard(pin.id), "unpinned, expired guard is dropped")
}

// A pinned relay is held on top of the wanted fill, so the node leans on it first.
func TestPinnedGuardHeldBesideFill(t *testing.T) {
	s, _ := newTestSwarm(t)
	withGuardMode(s, config.GuardOn)
	cands := []guardCandidate{guardServer(s, 1, 10), guardServer(s, 2, 11), guardServer(s, 3, 12)}
	pin := guardServer(s, 9, 90)

	s.planGuards(time.Now(), cands, []guardCandidate{pin})
	assert.True(t, s.IsGuard(pin.id), "pinned guard is among the guards")
	assert.GreaterOrEqual(t, len(s.Guards()), guardsWanted, "the rest still fill up to wanted")
}

// A pinned relay keeps guard mode on where "auto" would stand down (a country that does not
// censor); "off" still means direct.
func TestPinnedGuardKeepsAutoOn(t *testing.T) {
	s, _ := newTestSwarm(t)
	withGuardMode(s, config.GuardAuto)
	s.SetGuardsFree(true)
	assert.False(t, s.GuardModeActive(), "auto in a free country, nothing pinned: off")

	s.cfg.AntiDPI.PinnedGuards = []string{network.JoinPeerAddrInfo("198.51.100.9:443", idOf(9), "cover.example", true)}
	assert.True(t, s.GuardModeActive(), "a pinned relay: on")

	withGuardMode(s, config.GuardOff)
	assert.False(t, s.GuardModeActive(), "off: direct even with a pinned relay")
}

// Behind a proxy a user's node is met at a relay (a hole punch would show our address), without
// guard mode too; servers, listed peers and the local network are not.
func TestUserLinkViaRelayBehindProxy(t *testing.T) {
	s, _ := newTestSwarm(t)
	withGuardMode(s, config.GuardOff)
	user := idOf(60)
	assert.False(t, s.UserLinkViaRelay(user, "203.0.113.60:4242"), "no proxy, no guards: direct")

	require.NoError(t, network.SetOutboundProxy("127.0.0.1:9150"))
	t.Cleanup(func() { _ = network.SetOutboundProxy("") })
	assert.True(t, s.UserLinkViaRelay(user, "203.0.113.60:4242"))
	assert.True(t, s.UserLinkViaRelay(user, ""), "no address known")
	assert.False(t, s.UserLinkViaRelay(user, "192.168.1.60:4242"), "the local network")

	server := guardServer(s, 1, 10)
	assert.False(t, s.UserLinkViaRelay(server.id, server.addr), "a camouflaged server is dialed through the proxy")
}
