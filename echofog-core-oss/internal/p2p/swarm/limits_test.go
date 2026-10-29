package swarm

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/abuse"
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/google/uuid"
)

// fakeInbound puts a peer into the active set as if it had dialed in from ip, connected since at.
func fakeInbound(s *Swarm, ip netip.Addr, at time.Time) *Peer {
	pub := randomPub()
	p := NewPeer(s.ctx, pub, s.dispatcher, "", false)
	p.ip = ip
	p.connectedAt = at
	s.mu.Lock()
	s.activePeers[p.id] = p
	s.mu.Unlock()
	return p
}

func inboundEvent(pub types.PeerPublicKey, ip netip.Addr) network.NewConnEvent {
	return network.NewConnEvent{PeerID: types.PeerPubKeyToID(pub), PeerPubKey: pub, RemoteIP: ip}
}

func newLimitedSwarm(t *testing.T, edit func(*config.AppConfig)) *Swarm {
	t.Helper()
	s, _ := newTestSwarm(t)
	cfg := *s.cfg
	cfg.AntiDPI.GuardMode = config.GuardOff // the limits alone: guard mode refuses the test addresses as from abroad
	edit(&cfg)
	s.cfg = &cfg
	n := cfg.Network
	s.Guard().SetPeerLimits(n.MaxPeersPerIP, n.MaxPeersPerSubnet, n.MaxIDsPerIP, n.MaxIDsPerSubnet)
	return s
}

// A full node turns strangers away already in the handshake, before it signs anything; a peer it
// knows takes the slot of the youngest stranger, and a long-lived peer is never pushed out.
func TestInboundCapPrefersKnownAndLongLivedPeers(t *testing.T) {
	s := newLimitedSwarm(t, func(c *config.AppConfig) { c.Network.MaxInboundPeers = 2 })
	old := fakeInbound(s, netip.MustParseAddr("203.0.113.1"), time.Now().Add(-time.Hour))
	young := fakeInbound(s, netip.MustParseAddr("203.0.113.2"), time.Now())

	stranger := randomPub()
	if _, refuse := s.screenPeer(stranger); !refuse {
		t.Fatal("a full node must refuse a stranger in the handshake")
	}
	if _, err := s.admitInbound(inboundEvent(stranger, netip.MustParseAddr("198.51.100.9"))); err == nil {
		t.Fatal("a full node must not admit a stranger")
	}

	friend := randomPub()
	friendID := types.PeerPubKeyToID(friend)
	s.SetTrustedPeers(func(id types.PeerID) bool { return id == friendID })
	if _, refuse := s.screenPeer(friend); refuse {
		t.Fatal("a known peer passes the screen of a full node")
	}
	release, err := s.admitInbound(inboundEvent(friend, netip.MustParseAddr("198.51.100.10")))
	if err != nil {
		t.Fatalf("a known peer is admitted to a full node: %v", err)
	}
	defer release()
	if s.ThisIsActivePeer(young.id) {
		t.Fatal("the youngest stranger should have made room")
	}
	if !s.ThisIsActivePeer(old.id) {
		t.Fatal("a long-lived peer must not be evicted")
	}

	// both slots held by long-lived peers: a second friend finds nobody to replace
	fakeInbound(s, netip.MustParseAddr("203.0.113.4"), time.Now().Add(-time.Hour))
	other := randomPub()
	otherID := types.PeerPubKeyToID(other)
	s.SetTrustedPeers(func(id types.PeerID) bool { return id == friendID || id == otherID })
	if _, err := s.admitInbound(inboundEvent(other, netip.MustParseAddr("198.51.100.11"))); err == nil {
		t.Fatal("with only long-lived peers there is nobody to evict")
	}
}

// Peers that connected before (peer store) or stayed long count as known.
func TestKnownPeers(t *testing.T) {
	s, _ := newTestSwarm(t)
	pub := randomPub()
	id := types.PeerPubKeyToID(pub)
	if s.IsKnownPeer(id) {
		t.Fatal("a fresh identity is a stranger")
	}
	s.peerStore.SaveDiscovered(pub, "203.0.113.5:4242")
	if s.IsKnownPeer(id) {
		t.Fatal("a peer we only heard of is still a stranger")
	}
	_ = s.peerStore.SavePeer(pub, "203.0.113.5:4242", 100)
	if !s.IsKnownPeer(id) {
		t.Fatal("a peer we were connected to is known")
	}

	p := fakeInbound(s, netip.MustParseAddr("203.0.113.6"), time.Now().Add(-2*longLivedPeer))
	s.RemovePeer(p.id)
	s.noteLongLived(p)
	if !s.IsKnownPeer(p.id) {
		t.Fatal("a peer that stayed connected for long is known for a while after")
	}
}

// A server asks strangers for a puzzle even when calm, and nothing of peers it knows; a banned
// peer is refused.
func TestServerAsksStrangersForPuzzle(t *testing.T) {
	s := newLimitedSwarm(t, func(c *config.AppConfig) { c.Role = config.RoleServer })
	stranger := randomPub()
	if bits, refuse := s.screenPeer(stranger); refuse || bits != handshake.StrangerPoWBits {
		t.Fatalf("server, stranger: want a %d-bit puzzle, got %d (refused %v)", handshake.StrangerPoWBits, bits, refuse)
	}
	known := randomPub()
	_ = s.peerStore.SavePeer(known, "203.0.113.7:4242", 100)
	if bits, refuse := s.screenPeer(known); refuse || bits != 0 {
		t.Fatalf("server, known peer: want no puzzle, got %d (refused %v)", bits, refuse)
	}
	s.BanPeerFor(types.PeerPubKeyToID(stranger), time.Hour)
	if _, refuse := s.screenPeer(stranger); !refuse {
		t.Fatal("a banned peer is refused in the handshake")
	}

	client, _ := newTestSwarm(t)
	if bits, _ := client.screenPeer(randomPub()); bits != 0 {
		t.Fatal("a client asks no puzzle while calm")
	}
}

// Reports add up to a time-limited ban; several bad identities from one address ban the address.
func TestMisbehaviourBansPeerAndAddress(t *testing.T) {
	s, _ := newTestSwarm(t)
	ip := netip.MustParseAddr("203.0.113.20")
	p := fakeInbound(s, ip, time.Now())
	for i := 0; i < 4; i++ {
		s.ReportPeer(p.id, abuse.BadSignature)
	}
	if !s.CheckOnBan(p.id) {
		t.Fatal("four bad signatures should ban the peer")
	}
	if s.ThisIsActivePeer(p.id) {
		t.Fatal("a banned peer is disconnected")
	}
	if s.Guard().Banned(ip) {
		t.Fatal("one bad peer alone does not ban its address")
	}

	for i := 0; i < 8 && !s.Guard().Banned(ip); i++ {
		q := fakeInbound(s, ip, time.Now())
		s.ReportPeer(q.id, abuse.BadSignature)
		s.ReportPeer(q.id, abuse.BadSignature)
	}
	if !s.Guard().Banned(ip) {
		t.Fatal("an address whose identities keep misbehaving is banned")
	}
	for _, q := range s.GetAllPeers() {
		if q.ip == ip {
			t.Fatal("peers from a banned address are disconnected")
		}
	}

	friend := fakeInbound(s, netip.MustParseAddr("203.0.113.21"), time.Now())
	s.SetTrustedPeers(func(id types.PeerID) bool { return id == friend.id })
	for i := 0; i < 10; i++ {
		s.ReportPeer(friend.id, abuse.BadSignature)
	}
	if s.CheckOnBan(friend.id) {
		t.Fatal("trusted peers are not banned")
	}
}

// Fresh identities from one address are turned away after the configured number.
func TestAdmitInboundLimitsIdentitiesPerAddress(t *testing.T) {
	s := newLimitedSwarm(t, func(c *config.AppConfig) { c.Network.MaxIDsPerIP = 3 })
	ip := netip.MustParseAddr("203.0.113.30")
	for i := 0; i < 3; i++ {
		release, err := s.admitInbound(inboundEvent(randomPub(), ip))
		if err != nil {
			t.Fatalf("identity %d: %v", i, err)
		}
		release()
	}
	if _, err := s.admitInbound(inboundEvent(randomPub(), ip)); err == nil {
		t.Fatal("a fourth fresh identity within the window is refused")
	}
}

// A peer that opens streams and sends nothing on them holds only so many; the rest are closed
// and the flood is reported.
func TestStreamsWithoutFirstFrameAreBounded(t *testing.T) {
	srv, srvTr := newTestSwarm(t)
	cli, _ := newTestSwarm(t)
	connect(t, cli, srv, srvTr)
	conn := cli.GetPeer(srv.selfID).transport.GetConn()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < maxPendingStreamsPerPeer+40; i++ {
		st, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		_, _ = st.Write([]byte{0}) // one byte of a length prefix: the frame never completes
	}
	waitFor(t, "flood reported", func() bool { return srv.lim.scorer.PeerScore(cli.selfID) > 0 })
	p := srv.GetPeer(cli.selfID)
	if p == nil {
		t.Fatal("one flood is no ban")
	}
	if n := p.pendingStreams.Load(); n > maxPendingStreamsPerPeer {
		t.Fatalf("%d streams wait for their first frame, the limit is %d", n, maxPendingStreamsPerPeer)
	}
}

func dagRequest(sid uuid.UUID) *internal_pb.MessageData {
	return &internal_pb.MessageData{Payload: &internal_pb.MessageData_SessionReq{SessionReq: &internal_pb.SessionInitRequest{
		Type: internal_pb.SessionType_SESSION_DAG, SessionId: sid[:], DesiredStreams: 2,
	}}}
}

// A peer may open only so many sessions, never reuse another's session ID, and never attach a
// stream to a session of another peer.
func TestDagSessionLimits(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	var reported []abuse.Reason
	sm.report = func(_ types.PeerID, r abuse.Reason) { reported = append(reported, r) }
	peer, other := types.PeerID{1}, types.PeerID{2}

	var first uuid.UUID
	for i := 0; i < MaxSessionsPerPeer; i++ {
		sid := uuid.New()
		if i == 0 {
			first = sid
		}
		if _, _, err := sm.HandleDagSessionReq(dagRequest(sid), peer); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, _, err := sm.HandleDagSessionReq(dagRequest(uuid.New()), peer); err == nil {
		t.Fatal("one session over the per-peer limit is refused")
	}
	if len(reported) != 1 || reported[0] != abuse.Refused {
		t.Fatalf("the refusal is reported, got %v", reported)
	}
	if _, _, err := sm.HandleDagSessionReq(dagRequest(first), other); err == nil {
		t.Fatal("another peer must not take over an existing session ID")
	}
	if got := sm.Get(types.SessionID(first)); got == nil || got.PeerID != peer {
		t.Fatal("the session still belongs to its peer")
	}
	if sm.AddStream(types.SessionID(first), &network.Stream{RemoteID: other}) {
		t.Fatal("a stream from another peer must not join the session")
	}
	if _, _, err := sm.HandleDagSessionReq(dagRequest(uuid.New()), other); err != nil {
		t.Fatalf("another peer has its own allowance: %v", err)
	}

	total := NewSessionManager()
	total.ctx = context.Background()
	for i := 0; i < MaxSessions; i++ {
		if _, _, err := total.HandleDagSessionReq(dagRequest(uuid.New()), types.PeerID{byte(i), byte(i >> 8), 9}); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, _, err := total.HandleDagSessionReq(dagRequest(uuid.New()), types.PeerID{0xff, 0xff}); err == nil {
		t.Fatal("the node as a whole holds only so many sessions")
	}
}
