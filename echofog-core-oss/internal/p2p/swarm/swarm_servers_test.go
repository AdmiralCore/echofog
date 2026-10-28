package swarm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func idOf(b byte) types.PeerID {
	var id types.PeerID
	id[0] = b
	return id
}

func withRole(s *Swarm, role string) {
	cfg := *s.cfg
	cfg.Role = role
	s.cfg = &cfg
}

// The noise guard: a client never dials a server across the border over noise. Every case in the
// table is one way the rule is met or not.
func TestNoiseDialRefused(t *testing.T) {
	const public = "198.51.100.7:4242"
	server, other := idOf(1), idOf(2)

	cases := []struct {
		name   string
		setup  func(s *Swarm)
		id     types.PeerID
		addr   string
		refuse bool
	}{
		{"server, no SNI, public address: refused", func(s *Swarm) { s.SetPeerServer(server, true) }, server, public, true},
		{"unknown peer is not a server: dialed", func(s *Swarm) {}, other, public, false},
		{"server with a known SNI: REALITY, dialed", func(s *Swarm) { s.SetPeerServer(server, true); s.SetPeerSNI(server, "cover.example") }, server, public, false},
		{"server whose address carries the SNI: dialed", func(s *Swarm) { s.SetPeerServer(server, true) }, server, public + "?sni=cover.example", false},
		{"server on the local network: no border", func(s *Swarm) { s.SetPeerServer(server, true) }, server, "192.168.1.5:4242", false},
		{"server on loopback: no border", func(s *Swarm) { s.SetPeerServer(server, true) }, server, "127.0.0.1:4242", false},
		{"server behind carrier NAT address: no border", func(s *Swarm) { s.SetPeerServer(server, true) }, server, "100.64.1.2:4242", false},
		{"server of the home region: dialed over noise", func(s *Swarm) {
			s.SetPeerServer(server, true)
			s.SetSameRegionFunc(func(string) bool { return true })
		}, server, public, false},
		{"server in another region: refused", func(s *Swarm) {
			s.SetPeerServer(server, true)
			s.SetSameRegionFunc(func(string) bool { return false })
		}, server, public, true},
		{"server by name (country unknown): refused", func(s *Swarm) { s.SetPeerServer(server, true) }, server, "vpn.example.org:4242", true},
		{"server over WebSocket: refused", func(s *Swarm) { s.SetPeerServer(server, true) }, server, "wss://vpn.example.org/ws", true},
		{"operator lifted the rule", func(s *Swarm) {
			s.SetPeerServer(server, true)
			cfg := *s.cfg
			cfg.AntiDPI.AllowNoiseToServers = true
			s.cfg = &cfg
		}, server, public, false},
		{"this node is a server, the peer a server: dialed", func(s *Swarm) { s.SetPeerServer(server, true); withRole(s, config.RoleServer) }, server, public, false},
		{"this node is a server, the peer a user's node: refused", func(s *Swarm) {
			s.SetPeerClient(other, handshake.CapRelay|handshake.CapDHTServer)
			withRole(s, config.RoleServer)
		}, other, public, true},
		{"a server dialing an unknown peer: dialed", func(s *Swarm) { withRole(s, config.RoleServer) }, other, public, false},
		{"a server dialing a hidden node (announces nothing): dialed", func(s *Swarm) {
			s.SetPeerClient(other, handshake.CapNone)
			withRole(s, config.RoleServer)
		}, other, public, false},
		{"a server dialing a user's node on the local network: dialed", func(s *Swarm) {
			s.SetPeerClient(other, handshake.CapRelay)
			withRole(s, config.RoleServer)
		}, other, "10.0.0.5:4242", false},
		{"a server dialing a user's node that has an SNI: dialed", func(s *Swarm) {
			s.SetPeerClient(other, handshake.CapRelay)
			s.SetPeerSNI(other, "cover.example")
			withRole(s, config.RoleServer)
		}, other, public, false},
		{"a client dialing a user's node: dialed (user to user is not this rule)", func(s *Swarm) { s.SetPeerClient(other, handshake.CapRelay) }, other, public, false},
		{"a server's own flag beats a client guess", func(s *Swarm) {
			s.SetPeerServer(server, true)
			s.SetPeerClient(server, handshake.CapRelay)
			withRole(s, config.RoleServer)
		}, server, public, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestSwarm(t)
			c.setup(s)
			assert.Equal(t, c.refuse, s.noiseDialRefused(c.id, c.addr))
			err := s.expectPeer(c.addr, c.id)
			if c.refuse {
				assert.ErrorIs(t, err, ErrNoiseDialRefused)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// A connected peer that announces CapServer in its handshake is a server, with no address saying so.
func TestServerKnownFromHandshakeCaps(t *testing.T) {
	s, _ := newTestSwarm(t)
	p, _ := addPrimaryPeer(context.Background(), s, 5)
	require.False(t, s.IsServerPeer(p.ID()))
	p.caps.Store(uint32(handshake.CapRelay | handshake.CapServer))
	assert.True(t, s.IsServerPeer(p.ID()))
	assert.True(t, s.noiseDialRefused(p.ID(), "198.51.100.7:4242"))
}

// A candidate the guard holds back is not dialed, and is not counted as a peer that failed: it is
// waiting for its SNI, and forgetting it after a few rounds would lose it for good.
func TestDialCandidatesSkipsRefusedWithoutBlame(t *testing.T) {
	s, _ := newTestSwarm(t)
	server := idOf(7)
	s.SetPeerServer(server, true)
	pub := randomPub()
	id := types.PeerPubKeyToID(pub)
	s.SetPeerServer(id, true)
	require.True(t, s.peerStore.SaveDiscovered(pub, "198.51.100.9:4242"))

	start := time.Now()
	s.dialCandidates([]DialCandidate{{ID: id, Addr: "198.51.100.9:4242"}})
	assert.Less(t, time.Since(start), time.Second, "no dial was made")
	e, err := s.peerStore.GetPeer(id)
	require.NoError(t, err)
	assert.Zero(t, e.GetFailCount(), "a held-back peer is not a failed one")

	// once its SNI arrives the peer is dialed
	s.SetPeerSNI(id, "cover.example")
	assert.NoError(t, s.expectPeer("198.51.100.9:4242", id))
}

// What the swarm learns survives a restart: the SNI and the role, kept under a blinded key.
func TestPeerFactsPersist(t *testing.T) {
	s, _ := newTestSwarm(t)
	id := idOf(3)
	user := idOf(6)
	s.SetPeerSNI(id, "cover.example")
	s.SetPeerServer(id, true)
	s.SetPeerClient(user, handshake.CapRelay)

	s.peerSNIMu.Lock()
	s.peerFacts = nil // as after a restart
	s.peerSNIMu.Unlock()
	require.Empty(t, s.GetPeerSNI(id))
	s.loadPeerFacts()
	assert.Equal(t, "cover.example", s.GetPeerSNI(id))
	assert.True(t, s.IsServerPeer(id))
	assert.True(t, s.isClientPeer(user))
	assert.False(t, s.isClientPeer(id))
}

// The facts a peer tells about itself are bounded, so peer exchange cannot fill memory with them.
func TestPeerFactsAreBounded(t *testing.T) {
	s, _ := newTestSwarm(t)
	for i := 0; i < maxPeerFacts+50; i++ {
		var id types.PeerID
		id[0], id[1], id[2] = byte(i), byte(i>>8), byte(i>>16)
		id[31] = 1
		s.peerSNIMu.Lock()
		if s.peerFacts == nil {
			s.peerFacts = map[types.PeerID]peerFact{}
		}
		if len(s.peerFacts) < maxPeerFacts {
			s.peerFacts[id] = peerFact{sni: "x"}
		}
		s.peerSNIMu.Unlock()
	}
	over := idOf(250)
	over[31] = 9
	s.SetPeerSNI(over, "cover.example")
	assert.Empty(t, s.GetPeerSNI(over), "a new peer past the bound is not remembered")
}

// An address passed on by peer exchange or the DHT carries the SNI and the role, and what comes out
// at the other end is the plain host:port to dial, not the string with its query.
func TestAnnotatedAddrRoundTrip(t *testing.T) {
	a, _ := newTestSwarm(t)
	b, _ := newTestSwarm(t)
	server := idOf(4)
	a.SetPeerSNI(server, "cover.example")
	a.SetPeerServer(server, true)

	sent := a.AnnotatedAddr(server, "198.51.100.7:4242")
	assert.Equal(t, "198.51.100.7:4242?sni=cover.example&srv=1", sent)
	assert.Equal(t, sent, a.AnnotatedAddr(server, sent), "annotated once")

	got := b.LearnPeerAddr(server, sent)
	assert.Equal(t, "198.51.100.7:4242", got)
	assert.Equal(t, "cover.example", b.GetPeerSNI(server))
	assert.True(t, b.IsServerPeer(server))

	// a peer without either fact is passed on as it is
	plain := idOf(5)
	assert.Equal(t, "198.51.100.8:4242", a.AnnotatedAddr(plain, "198.51.100.8:4242"))
	assert.Equal(t, "198.51.100.8:4242", b.LearnPeerAddr(plain, "198.51.100.8:4242"))
}

// A node tells the peer it exchanges peers with how it is to be dialed: its own entry in the list
// has no host, only the SNI and the server role.
func TestPeerExchangeCarriesOwnCamouflage(t *testing.T) {
	s, _ := newTestSwarm(t)
	withRole(s, config.RoleServer)
	s.SetAnnouncedSNI("cover.example")

	list := s.shareablePeers(idOf(9), 8)
	require.NotEmpty(t, list.Peers)
	self := list.Peers[0]
	assert.Equal(t, s.selfID[:], self.Id)
	assert.Equal(t, "?sni=cover.example&srv=1", self.LastKnownAddr)
	assert.True(t, handshake.Capability(self.Caps).Has(handshake.CapServer))

	// a client without a cover site says nothing about itself
	c, _ := newTestSwarm(t)
	assert.Empty(t, c.shareablePeers(idOf(9), 8).Peers)
}

// The peer on the other end describing itself is how a node learns the SNI of a camouflage node it
// reached through a third party: taken in, though the entry has no address.
func TestLearnPeersTakesTheSendersOwnCamouflage(t *testing.T) {
	s, _ := newTestSwarm(t)
	pub := randomPub()
	from := types.PeerPubKeyToID(pub)
	s.learnPeers(from, &internal_pb.PeerList{Peers: []*internal_pb.PeerList_Peer{
		{Id: from[:], PubKey: pub[:], LastKnownAddr: "?sni=cover.example&srv=1", Caps: uint32(handshake.CapServer)},
	}})
	assert.Equal(t, "cover.example", s.GetPeerSNI(from))
	assert.True(t, s.IsServerPeer(from))
}

// Two real nodes: the client learns the server's SNI and role from the exchange that follows the
// connection, whichever way it came to know the address.
func TestSNIArrivesWithTheConnection(t *testing.T) {
	srv, srvTr := newTestSwarm(t)
	withRole(srv, config.RoleServer)
	srv.SetAnnouncedSNI("cover.example")
	cli, _ := newTestSwarm(t)

	connect(t, cli, srv, srvTr)
	waitFor(t, "the server's SNI to reach the client", func() bool { return cli.GetPeerSNI(srv.selfID) == "cover.example" })
	assert.True(t, cli.IsServerPeer(srv.selfID))
}

func TestErrNoiseDialRefusedIsDistinct(t *testing.T) {
	assert.False(t, errors.Is(network.ErrPeerUnknown, ErrNoiseDialRefused))
}

// A bootstrap entry is the first thing a client dials. The SNI and the role in it are taken in
// before the dial: with the SNI it is REALITY, with only the role it is not dialed over noise.
func TestBootstrapEntryFactsAreTakenInBeforeTheDial(t *testing.T) {
	s, _ := newTestSwarm(t)
	cam, bare := idOf(0x41), idOf(0x42)
	s.Bootstrap([]string{
		"198.51.100.9:4242/" + cam.String() + "?sni=cover.example&srv=1",
		"198.51.100.10:4242/" + bare.String() + "?srv=1",
	})
	waitFor(t, "the facts of both entries", func() bool { return s.IsServerPeer(cam) && s.IsServerPeer(bare) })
	assert.Equal(t, "cover.example", s.GetPeerSNI(cam))
	assert.False(t, s.noiseDialRefused(cam, "198.51.100.9:4242"), "camouflaged: dialed as REALITY")
	assert.True(t, s.noiseDialRefused(bare, "198.51.100.10:4242"), "a server of unknown SNI is not dialed over noise")
}

// A user's node is told apart from a server by the node that passes its address on, so a server
// that learns it through the DHT or peer exchange, where it never saw its handshake, does not dial
// it over noise either.
func TestServerLearnsAUsersNodeFromTheAddress(t *testing.T) {
	// the node that knows the user's node from its handshake passes the address on with srv=0
	teller, _ := newTestSwarm(t)
	userPub := randomPub()
	user := types.PeerPubKeyToID(userPub)
	p, _ := addPrimaryPeer(context.Background(), teller, 9)
	_ = p
	teller.SetPeerClient(user, handshake.CapRelay|handshake.CapDHTServer)
	sent := teller.AnnotatedAddr(user, "198.51.100.40:4242")
	assert.Equal(t, "198.51.100.40:4242?srv=0", sent)

	// a server takes it in and then holds back
	server, _ := newTestSwarm(t)
	withRole(server, config.RoleServer)
	assert.False(t, server.noiseDialRefused(user, "198.51.100.40:4242"), "unknown until told")
	assert.Equal(t, "198.51.100.40:4242", server.LearnPeerAddr(user, sent))
	assert.True(t, server.isClientPeer(user))
	assert.True(t, server.noiseDialRefused(user, "198.51.100.40:4242"))

	// a server stays a server whatever a third party says
	srv := idOf(0x51)
	server.SetPeerServer(srv, true)
	server.LearnPeerAddr(srv, "198.51.100.41:4242?srv=0")
	assert.False(t, server.isClientPeer(srv))
	assert.False(t, server.noiseDialRefused(srv, "198.51.100.41:4242"))

	// this node's own address is a user's when it is not a server
	assert.Equal(t, "198.51.100.42:4242?srv=0", teller.AnnotatedAddr(teller.selfID, "198.51.100.42:4242"))
	assert.Empty(t, teller.AnnotatedAddr(teller.selfID, ""), "no entry about itself to put in a peer list")
}
