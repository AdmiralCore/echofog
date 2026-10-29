package swarm

import (
	"encoding/hex"
	"errors"
	"net"
	"strings"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// What the swarm knows about a node beyond its address: the camouflage SNI of a node that mimics a
// cover site, and whether it runs in the server role. Both travel with the address (join link,
// identity card, peer exchange, DHT) and are kept across restarts, because a client that has lost
// either has only one way left to reach a server: over noise, which a censor flags.

// ErrNoiseDialRefused: the peer is a server whose camouflage SNI is not known, it is across a
// border, and noise is the only way left to dial it. The dial is not made.
var ErrNoiseDialRefused = errors.New("swarm: not dialing across the border over noise (a server whose camouflage SNI is unknown, or a user's node from a server)")

const (
	peerFactsPrefix = "peerfacts:"
	// maxPeerFacts bounds what peers can make this node remember (it is fed by peer exchange).
	maxPeerFacts = 8192
)

type peerFact struct {
	sni    string
	server bool
	client bool // announced its capabilities without CapServer: an ordinary user's node
}

// SetPeerClient records that the peer announced capabilities and is not a server: a user's node,
// which has no cover site and sits behind a NAT. A server holds back from dialing it (see
// noiseDialRefused).
func (s *Swarm) SetPeerClient(id types.PeerID, caps handshake.Capability) {
	if caps == handshake.CapNone || caps.Has(handshake.CapServer) {
		return // a hidden node announces nothing; a server is not a client
	}
	s.peerSNIMu.Lock()
	if s.peerFacts == nil {
		s.peerFacts = make(map[types.PeerID]peerFact)
	}
	f, known := s.peerFacts[id]
	if (!known && len(s.peerFacts) >= maxPeerFacts) || f.client || f.server {
		s.peerSNIMu.Unlock()
		return
	}
	f.client = true
	s.peerFacts[id] = f
	s.peerSNIMu.Unlock()
	s.persistPeerFact(id, f)
}

// MarkExplicitPeer records that the operator listed the peer (a bootstrap node, an invitation): it
// is dialed whatever it announces.
func (s *Swarm) MarkExplicitPeer(id types.PeerID) {
	s.peerSNIMu.Lock()
	if s.explicit == nil {
		s.explicit = make(map[types.PeerID]struct{})
	}
	if len(s.explicit) < maxPeerFacts {
		s.explicit[id] = struct{}{}
	}
	s.peerSNIMu.Unlock()
}

func (s *Swarm) isExplicitPeer(id types.PeerID) bool {
	s.peerSNIMu.RLock()
	defer s.peerSNIMu.RUnlock()
	_, ok := s.explicit[id]
	return ok
}

// isClientPeer reports whether the peer is known to be an ordinary user's node.
func (s *Swarm) isClientPeer(id types.PeerID) bool {
	s.peerSNIMu.RLock()
	f := s.peerFacts[id]
	s.peerSNIMu.RUnlock()
	if f.server {
		return false
	}
	if f.client {
		return true
	}
	if p := s.GetPeer(id); p != nil {
		c := p.Caps()
		return c != handshake.CapNone && !c.Has(handshake.CapServer)
	}
	return false
}

// SetPeerServer records that the peer runs in the server role (or not).
func (s *Swarm) SetPeerServer(id types.PeerID, server bool) {
	s.peerSNIMu.Lock()
	if s.peerFacts == nil {
		s.peerFacts = make(map[types.PeerID]peerFact)
	}
	f, known := s.peerFacts[id]
	if !known && len(s.peerFacts) >= maxPeerFacts {
		s.peerSNIMu.Unlock()
		return
	}
	changed := f.server != server
	f.server = server
	s.peerFacts[id] = f
	s.peerSNIMu.Unlock()
	if changed {
		s.persistPeerFact(id, f)
	}
}

// IsServerPeer reports whether the peer is known to run in the server role: told so in an address,
// or announced in its handshake.
func (s *Swarm) IsServerPeer(id types.PeerID) bool {
	s.peerSNIMu.RLock()
	f := s.peerFacts[id]
	s.peerSNIMu.RUnlock()
	if f.server {
		return true
	}
	if p := s.GetPeer(id); p != nil {
		return p.Caps().Has(handshake.CapServer)
	}
	return false
}

// AnnotatedAddr is the address of a peer as it is passed on to other nodes: with its camouflage SNI
// and its server flag, so whoever learns the address learns how it may be dialed. This node's own
// facts come from its configuration.
func (s *Swarm) AnnotatedAddr(id types.PeerID, addr string) string {
	sni := s.GetPeerSNI(id)
	role := network.RoleUnknown
	switch {
	case s.IsServerPeer(id):
		role = network.RoleServer
	case s.isClientPeer(id):
		role = network.RoleClient
	}
	if id == s.selfID {
		if sni == "" {
			sni = s.GetAnnouncedSNI()
		}
		switch {
		case s.cfg != nil && s.cfg.IsServer():
			role = network.RoleServer
		case addr != "":
			role = network.RoleClient // our own address is a user's: say so, to the servers that would dial it
		}
	}
	return network.AnnotateAddrRole(addr, sni, role)
}

// LearnPeerAddr takes in an address another node passed on, "host:port[?sni=..&srv=1]": it records
// the SNI and the server flag it carries for id and returns the plain "host:port" to dial and store.
func (s *Swarm) LearnPeerAddr(id types.PeerID, addr string) string {
	info := network.ParseAddrInfo(addr)
	if info.SNI != "" {
		s.SetPeerSNI(id, info.SNI)
	}
	if info.Server {
		s.SetPeerServer(id, true)
	} else if info.Client {
		s.SetPeerClient(id, handshake.CapRelay) // any announced capabilities: it is a node, not a server
	}
	return info.Addr
}

// SetSameRegionFunc installs the region policy used by the noise guard: fn reports whether the node
// at addr is in this node's own region (see pkg/api/node.applyRegionObfs). Without it every public
// address counts as across the border.
func (s *Swarm) SetSameRegionFunc(fn func(addr string) bool) {
	s.peerSNIMu.Lock()
	s.sameRegion = fn
	s.peerSNIMu.Unlock()
}

// acrossBorder reports whether a dial to addr may cross a censoring border: any address that is not
// on the local network and not placed in this node's own region by the region policy, an address
// that cannot be placed (a name, an unknown country) included.
func (s *Swarm) acrossBorder(addr string) bool {
	if isWSAddr(addr) {
		return true
	}
	if onLocalNetwork(addr) {
		return false
	}
	s.peerSNIMu.RLock()
	fn := s.sameRegion
	s.peerSNIMu.RUnlock()
	return fn == nil || !fn(addr)
}

// onLocalNetwork reports whether addr (host:port or a bare host) is a private, loopback,
// link-local, unspecified or carrier-grade NAT address: one that no border lies on the way to.
func onLocalNetwork(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return true
	}
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64 // carrier-grade NAT
}

// noiseDialRefused reports whether a dial to the peer at addr must not be made. For a client: the
// peer is a server (its address said so, or its handshake did) with no known camouflage SNI, so the
// only way to reach it is noise, and addr is across a border. For a server: the peer is not known
// to be a server and was not listed explicitly, so it is a user's node or an unknown one, and
// addr is across a border. Unknown country counts as across. anti_dpi.allow_noise_to_servers lifts the rule for people who run their
// own servers without a cover site outside any censored network.
func (s *Swarm) noiseDialRefused(id types.PeerID, addr string) bool {
	if s.cfg == nil || s.cfg.AntiDPI.AllowNoiseToServers {
		return false
	}
	if s.GetPeerSNI(id) != "" {
		return false // camouflage: REALITY over QUIC, or browser TLS over TCP
	}
	if info := network.ParseAddrInfo(addr); info.SNI != "" {
		return false
	}
	if s.cfg.IsServer() {
		// A server dials servers, and the nodes it was told to (its bootstrap list). It has no use
		// for dialing a user's node, or a node that nobody has said is a server: users dial servers,
		// and a link the server opens to a user crosses the user's border as noise, the very thing a
		// censor flags. (A user behind a NAT cannot be reached that way anyway.) A node of an older
		// version does not say it is a server, so it is dialed only when listed explicitly; it can
		// dial this one.
		if s.IsServerPeer(id) || s.isExplicitPeer(id) {
			return false
		}
		return s.acrossBorder(network.ParseAddrInfo(addr).Addr)
	}
	if !s.IsServerPeer(id) {
		return false
	}
	return s.acrossBorder(network.ParseAddrInfo(addr).Addr)
}

// persistence: key peerfacts:<blinded id>, value id(32) flags(1) sni

func (s *Swarm) persistPeerFact(id types.PeerID, f peerFact) {
	if s.storage == nil {
		return
	}
	val := make([]byte, 0, 33+len(f.sni))
	val = append(val, id[:]...)
	flags := byte(0)
	if f.server {
		flags |= 1
	}
	if f.client {
		flags |= 2
	}
	val = append(val, flags)
	val = append(val, f.sni...)
	key := []byte(peerFactsPrefix + storage.BlindID(s.storage, hex.EncodeToString(id[:])))
	_ = s.storage.Set(key, val)
}

// loadPeerFacts reads back what earlier runs learned, so a restart does not forget which servers
// are camouflaged (and so end up dialing them over noise).
func (s *Swarm) loadPeerFacts() {
	if s.storage == nil {
		return
	}
	entries, err := s.storage.FindValues([]byte(peerFactsPrefix), maxPeerFacts)
	if err != nil {
		return
	}
	s.peerSNIMu.Lock()
	defer s.peerSNIMu.Unlock()
	for _, e := range entries {
		if len(e.Value) < 33 {
			continue
		}
		var id types.PeerID
		copy(id[:], e.Value[:32])
		f := peerFact{server: e.Value[32]&1 != 0, client: e.Value[32]&2 != 0, sni: string(e.Value[33:])}
		if strings.ContainsAny(f.sni, "/?& \x00") {
			continue
		}
		if s.peerFacts == nil {
			s.peerFacts = make(map[types.PeerID]peerFact)
		}
		s.peerFacts[id] = f
	}
}
