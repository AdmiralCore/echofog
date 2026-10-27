package swarm

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// fakeMuxStream is a no-op bidirectional stream: the exit-pool tests only count streams, they
// never move bytes over them.
type fakeMuxStream struct{}

func (fakeMuxStream) Read([]byte) (int, error)         { return 0, nil }
func (fakeMuxStream) Write(p []byte) (int, error)      { return len(p), nil }
func (fakeMuxStream) Close() error                     { return nil }
func (fakeMuxStream) SetDeadline(time.Time) error      { return nil }
func (fakeMuxStream) SetReadDeadline(time.Time) error  { return nil }
func (fakeMuxStream) SetWriteDeadline(time.Time) error { return nil }
func (fakeMuxStream) CancelRead()                      {}
func (fakeMuxStream) CloseWrite() error                { return nil }

// fakeMuxConn is an in-memory MuxConn for a peer link. OpenStreamSync hands out no-op streams so a
// test can watch how many a link carries; the accept loops block until the connection is closed so
// PeerWrapper.StartLoops parks instead of spinning.
type fakeMuxConn struct {
	id      int
	ctx     context.Context
	cancel  context.CancelFunc
	opened  atomic.Int32
	openErr error
}

func newFakeMuxConn(parent context.Context, id int) *fakeMuxConn {
	ctx, cancel := context.WithCancel(parent)
	return &fakeMuxConn{id: id, ctx: ctx, cancel: cancel}
}

func (c *fakeMuxConn) OpenStreamSync(ctx context.Context) (network.MuxStream, error) {
	if c.openErr != nil {
		return nil, c.openErr
	}
	if c.ctx.Err() != nil {
		return nil, c.ctx.Err()
	}
	c.opened.Add(1)
	return &fakeMuxStream{}, nil
}

func (c *fakeMuxConn) block(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *fakeMuxConn) AcceptStream(ctx context.Context) (network.MuxStream, error) {
	return nil, c.block(ctx)
}
func (c *fakeMuxConn) OpenUniStreamSync(ctx context.Context) (network.MuxSendStream, error) {
	return nil, c.block(ctx)
}
func (c *fakeMuxConn) AcceptUniStream(ctx context.Context) (network.MuxRecvStream, error) {
	return nil, c.block(ctx)
}
func (c *fakeMuxConn) SendDatagram([]byte) error { return nil }
func (c *fakeMuxConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return nil, c.block(ctx)
}
func (c *fakeMuxConn) LocalAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10000 + c.id} }
func (c *fakeMuxConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443} }
func (c *fakeMuxConn) CloseWithError(network.QuicErrorCode, string) error {
	c.cancel()
	return nil
}
func (c *fakeMuxConn) Context() context.Context      { return c.ctx }
func (c *fakeMuxConn) Protocol() string              { return network.ProtocolQUIC }
func (c *fakeMuxConn) ChannelBinding() ([]byte, error) { return make([]byte, 32), nil }

var _ network.MuxConn = (*fakeMuxConn)(nil)

// newExitPoolSwarm builds a bare Swarm sufficient for the exit-pool paths (no real transport).
func newExitPoolSwarm(t *testing.T) (*Swarm, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Swarm{
		activePeers:  make(map[types.PeerID]*Peer),
		fastHandlers: make(map[net_types.MessageType]FastFrameHandler),
		exitPools:    make(map[types.PeerID]*exitPoolState),
		ctx:          ctx,
		cancel:       cancel,
	}
	return s, ctx
}

// addPrimaryPeer registers a peer with one connection (its primary transport), the way a mesh or
// first-contact exit connection would sit in the swarm.
func addPrimaryPeer(ctx context.Context, s *Swarm, connID int) (*Peer, *fakeMuxConn) {
	var pub types.PeerPublicKey
	pub[0] = byte(connID + 1)
	id := types.PeerPubKeyToID(pub)
	p := NewPeer(ctx, pub, dispatcher.NewDispatcher(ctx), "203.0.113.7:443", true)
	conn := newFakeMuxConn(ctx, connID)
	pw := network.NewPeerWrapper(p.ctx, conn, id)
	p.transport = pw // package-internal: skip the router, the tests open streams directly
	s.mu.Lock()
	s.activePeers[id] = p
	s.mu.Unlock()
	return p, conn
}

func TestExitPool_ParallelLinksCoexistAndStreamsSpread(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()

	const maxConns = 3
	s.MarkExitPeer(exit, maxConns)
	s.SetExitAccumulate(exit)

	// Two rotation dials attach as extra links alongside the primary: 3 connections coexist.
	for i := 1; i <= 2; i++ {
		conn := newFakeMuxConn(ctx, i)
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn, IsOut: true}),
			"rotation dial %d should attach as an extra link", i)
	}
	require.Equal(t, maxConns, p.LinkCount(), "primary + 2 extra links should coexist")

	// Streams spread across the live links: 6 streams over 3 links => 2 each (least-loaded).
	streams := make([]*network.Stream, 0, 6)
	for i := 0; i < 6; i++ {
		st, err := s.OpenExitStream(ctx, exit)
		require.NoError(t, err)
		require.NotNil(t, st)
		streams = append(streams, st)
	}

	loads := linkLoads(p)
	require.Len(t, loads, maxConns)
	for _, n := range loads {
		require.Equal(t, 2, n, "each of the %d connections should carry an equal share of streams: %v", maxConns, loads)
	}

	for _, st := range streams {
		st.Close()
	}
}

func TestExitPool_RotationDrainsIdleLinkAtCapacity(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	s.SetExitAccumulate(exit)

	for i := 1; i <= 2; i++ {
		conn := newFakeMuxConn(ctx, i)
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn, IsOut: true}))
	}
	require.Equal(t, 3, p.LinkCount())
	firstExtra := extraConns(p)[0]

	// A further rotation while every link is idle drains the oldest extra link and adds the fresh
	// one: still 3 connections, and the drained one is closed.
	fresh := newFakeMuxConn(ctx, 3)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: fresh, IsOut: true}))
	require.Equal(t, 3, p.LinkCount(), "capacity stays at max after draining an idle link")
	require.Eventually(t, func() bool { return firstExtra.GetConn().Context().Err() != nil },
		time.Second, 10*time.Millisecond, "the drained idle link should be closed")

	// With every link busy, a rotation dial is rejected (never cut off an in-flight stream).
	busy := make([]*network.Stream, 0)
	for i := 0; i < 3; i++ {
		st, err := s.OpenExitStream(ctx, exit)
		require.NoError(t, err)
		busy = append(busy, st)
	}
	require.Nil(t, p.oldestIdleExtraLink(), "all links carry a stream now")

	rejected := newFakeMuxConn(ctx, 9)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: rejected, IsOut: true}),
		"a full, all-busy pool still owns (and closes) the incoming connection")
	require.Equal(t, 3, p.LinkCount(), "a rejected dial must not add a link")
	require.Error(t, rejected.Context().Err(), "the rejected connection should be closed")

	for _, st := range busy {
		st.Close()
	}
}

func TestExitPool_DeadLinkIsPrunedWhileOthersCarryOn(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	s.SetExitAccumulate(exit)

	conn1 := newFakeMuxConn(ctx, 1)
	conn2 := newFakeMuxConn(ctx, 2)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn1, IsOut: true}))
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn2, IsOut: true}))
	require.Equal(t, 3, p.LinkCount())

	// Kill one extra link, as a censor throttling a flow to death would: it is pruned and the
	// remaining connections keep serving streams.
	_ = conn1.CloseWithError(network.ErrCodeNormalClose, "killed")
	require.Eventually(t, func() bool { return p.LinkCount() == 2 }, time.Second, 10*time.Millisecond,
		"the dead link should be pruned, leaving the other connections")

	st, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err, "the VPN keeps carrying traffic on the surviving connections")
	require.NotNil(t, st)
	st.Close()
}

func TestExitPool_MeshPeerStaysSingleConnection(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	peerID := p.ID()

	// An unmarked (mesh) peer is never accumulated: tryAttachExitLink declines, so the connection
	// falls through to addPeer's unchanged one-connection-per-peer path.
	conn := newFakeMuxConn(ctx, 1)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: peerID, Conn: conn, IsOut: true}),
		"a mesh peer must not accumulate extra links")
	require.Equal(t, 1, p.LinkCount())

	// A marked exit before the tunnel is established (accumulate not latched) also declines, so the
	// first connection — e.g. the noise->REALITY upgrade of a camouflage exit — is set up normally.
	s.MarkExitPeer(peerID, 3)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: peerID, Conn: conn, IsOut: true}),
		"first contact (accumulate not latched) must not accumulate")
	require.Equal(t, 1, p.LinkCount())

	// Inbound connections never accumulate even once accumulating.
	s.SetExitAccumulate(peerID)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: peerID, Conn: conn, IsOut: false}),
		"an inbound connection follows the mesh rule")
	require.Equal(t, 1, p.LinkCount())
}

func TestExitPool_CamouflageExitKeepsOnlyRealityLinks(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()

	// A camouflage exit with a REALITY primary.
	s.SetPeerSNI(exit, "www.example.com")
	require.True(t, s.IsCamouflagePeer(exit))
	p.mu.Lock()
	p.primaryReality = true
	p.mu.Unlock()

	s.MarkExitPeer(exit, 3)
	s.SetExitAccumulate(exit)

	// A REALITY rotation dial attaches as a pool link.
	realityConn := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: realityConn, IsOut: true, Reality: true}))
	require.Equal(t, 2, p.LinkCount())

	// A noise connection to the same camouflage exit is refused outright and closed — it must never
	// carry tunnel bytes a censor could flag.
	noiseConn := newFakeMuxConn(ctx, 2)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: noiseConn, IsOut: true, Reality: false}),
		"a noise link to a camouflage exit is owned (and closed), never attached")
	require.Equal(t, 2, p.LinkCount(), "the noise connection must not become a pool link")
	require.Error(t, noiseConn.Context().Err(), "the noise connection should be closed")

	// The primary is REALITY and the only connections that were ever admitted as extra links were
	// REALITY ones (the noise connection above was rejected), so every pool link is REALITY.
	require.True(t, p.primaryIsReality(), "the primary to a camouflage exit must be REALITY")
}

func TestExitPool_TunnelStreamsNeverRideNoiseLinkToCamouflageExit(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, primaryConn := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	p.mu.Lock()
	p.primaryReality = true // the primary is REALITY (EnsureExitReality ran before the pool)
	p.mu.Unlock()
	s.MarkExitPeer(exit, 3)

	// Simulate a noise link having slipped into the pool (e.g. dialed during an early window before
	// the SNI was known). A tunnel stream must still never be opened on it.
	noiseConn := newFakeMuxConn(ctx, 7)
	noisePW := network.NewPeerWrapper(p.ctx, noiseConn, exit)
	p.addExitLink(noisePW, false)

	// Add a second REALITY link so spreading has a real choice of REALITY targets.
	realityConn := newFakeMuxConn(ctx, 8)
	realityPW := network.NewPeerWrapper(p.ctx, realityConn, exit)
	p.addExitLink(realityPW, true)

	streams := make([]*network.Stream, 0, 12)
	for i := 0; i < 12; i++ {
		st, err := s.OpenExitStream(ctx, exit)
		require.NoError(t, err)
		streams = append(streams, st)
	}

	require.Equal(t, int32(0), noiseConn.opened.Load(),
		"no tunnel stream may be opened on a noise link to a camouflage exit")
	require.Equal(t, int32(12), primaryConn.opened.Load()+realityConn.opened.Load(),
		"every stream rode a REALITY link (primary or REALITY extra)")

	for _, st := range streams {
		st.Close()
	}
}

func TestExitPool_RealityConnReplacesNoisePrimaryForCamouflageExit(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0) // primaryReality defaults to false (a noise bootstrap link)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)
	s.SetExitAccumulate(exit)

	// While the primary is still noise, a REALITY connection is NOT accumulated beside it: it is
	// handed back to addPeer to replace (upgrade) the primary. tryAttachExitLink returns false.
	realityConn := newFakeMuxConn(ctx, 1)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: realityConn, IsOut: true, Reality: true}),
		"a REALITY link must replace a noise primary, not sit beside it")
	require.Equal(t, 1, p.LinkCount(), "no extra link added; the replacement happens in addPeer")
}

// linkLoads returns the active-stream count of every live link (primary first, then extras).
func linkLoads(p *Peer) []int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	loads := []int{p.transport.ActiveStreams()}
	for _, l := range p.extraLinks {
		loads = append(loads, l.pw.ActiveStreams())
	}
	return loads
}

func extraConns(p *Peer) []*network.PeerWrapper {
	p.mu.RLock()
	defer p.mu.RUnlock()
	res := make([]*network.PeerWrapper, 0, len(p.extraLinks))
	for _, l := range p.extraLinks {
		res = append(res, l.pw)
	}
	return res
}
