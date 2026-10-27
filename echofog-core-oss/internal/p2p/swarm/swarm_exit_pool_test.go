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
	proto   string
}

func newFakeMuxConn(parent context.Context, id int) *fakeMuxConn {
	ctx, cancel := context.WithCancel(parent)
	return &fakeMuxConn{id: id, ctx: ctx, cancel: cancel, proto: network.ProtocolQUIC}
}

// newFakeTCPConn is a fake TCP/mimicry link — camouflaged like the real TCP fallback.
func newFakeTCPConn(parent context.Context, id int) *fakeMuxConn {
	c := newFakeMuxConn(parent, id)
	c.proto = network.ProtocolTCP
	return c
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
func (c *fakeMuxConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10000 + c.id}
}
func (c *fakeMuxConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443}
}
func (c *fakeMuxConn) CloseWithError(network.QuicErrorCode, string) error {
	c.cancel()
	return nil
}
func (c *fakeMuxConn) Context() context.Context        { return c.ctx }
func (c *fakeMuxConn) Protocol() string                { return c.proto }
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

	// Two rotation dials attach as extra links alongside the primary: 3 connections coexist.
	for i := 1; i <= 2; i++ {
		conn := newFakeMuxConn(ctx, i)
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn, IsOut: true, PoolLink: true}),
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

	for i := 1; i <= 2; i++ {
		conn := newFakeMuxConn(ctx, i)
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn, IsOut: true, PoolLink: true}))
	}
	require.Equal(t, 3, p.LinkCount())
	firstExtra := extraConns(p)[0]

	// A further rotation while every link is idle drains the oldest extra link and adds the fresh
	// one: still 3 connections, and the drained one is closed.
	fresh := newFakeMuxConn(ctx, 3)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: fresh, IsOut: true, PoolLink: true}))
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
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: rejected, IsOut: true, PoolLink: true}),
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

	conn1 := newFakeMuxConn(ctx, 1)
	conn2 := newFakeMuxConn(ctx, 2)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn1, IsOut: true, PoolLink: true}))
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn2, IsOut: true, PoolLink: true}))
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

func TestExitPool_OnlyPoolLinksAccumulate(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	peerID := p.ID()
	s.MarkExitPeer(peerID, 3)

	// An ordinary connection — a mesh redial, or the noise->REALITY upgrade of the first link to a
	// camouflage exit — is never attached, in either direction: it goes to addPeer and replaces the
	// one connection, as the mesh rule says.
	for _, out := range []bool{true, false} {
		conn := newFakeMuxConn(ctx, 1)
		require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: peerID, Conn: conn, IsOut: out}),
			"an ordinary connection (out=%v) must not accumulate", out)
	}
	require.Equal(t, 1, p.LinkCount())

	// A pool link with no live connection to the peer yet is not attached either: it becomes the
	// primary through addPeer.
	var pub types.PeerPublicKey
	pub[0] = 99
	stranger := types.PeerPubKeyToID(pub)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: stranger, Conn: newFakeMuxConn(ctx, 2), IsOut: false, PoolLink: true}))

	// Our own pool dial to an exit that is no longer marked (the VPN stopped) is not attached.
	s.UnmarkExitPeer(peerID)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: peerID, Conn: newFakeMuxConn(ctx, 3), IsOut: true, PoolLink: true}))
}

// The exit side: a client announces its rotation dials as pool links, and the exit keeps them beside
// the client's existing connection instead of replacing (and closing) it. Before, every parallel
// link the client opened made the exit close the previous one — on the client that was the primary,
// so the peer was dropped with all its links and the VPN failed over to another exit.
func TestExitPool_ExitKeepsClientPoolLinks(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, primary := addPrimaryPeer(ctx, s, 0) // the client, as the exit sees it; never marked here
	client := p.ID()

	links := make([]*fakeMuxConn, 0, maxInboundPoolLinks-1)
	for i := 1; i < maxInboundPoolLinks; i++ {
		conn := newFakeMuxConn(ctx, i)
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: client, Conn: conn, IsOut: false, PoolLink: true}),
			"inbound pool link %d should attach beside the primary", i)
		links = append(links, conn)
	}
	require.Equal(t, maxInboundPoolLinks, p.LinkCount())
	require.NoError(t, primary.Context().Err(), "the client's first connection must stay open")
	for _, c := range links {
		require.NoError(t, c.Context().Err())
	}
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

	// A REALITY rotation dial attaches as a pool link.
	realityConn := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: realityConn, IsOut: true, Reality: true, PoolLink: true}))
	require.Equal(t, 2, p.LinkCount())

	// A noise connection to the same camouflage exit is refused outright and closed — it must never
	// carry tunnel bytes a censor could flag.
	noiseConn := newFakeMuxConn(ctx, 2)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: noiseConn, IsOut: true, Reality: false, PoolLink: true}),
		"a noise link to a camouflage exit is owned (and closed), never attached")
	require.Equal(t, 2, p.LinkCount(), "the noise connection must not become a pool link")
	require.Error(t, noiseConn.Context().Err(), "the noise connection should be closed")

	// The primary is REALITY and the only connections that were ever admitted as extra links were
	// REALITY ones (the noise connection above was rejected), so every pool link is REALITY.
	require.True(t, p.primaryReality, "the primary to a camouflage exit must be REALITY")
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

func TestExitPool_TCPFallbackLinkToCamouflageExitIsEligible(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)

	// c1's path: the only connection to the camouflage exit is a TCP-fallback (mimicry) link, not
	// QUIC-REALITY. It must still be usable — the whole TCP transport is camouflaged.
	var pub types.PeerPublicKey
	pub[0] = 42
	exit := types.PeerPubKeyToID(pub)
	p := NewPeer(ctx, pub, dispatcher.NewDispatcher(ctx), "203.0.113.7:443", true)
	tcpConn := newFakeTCPConn(ctx, 1)
	p.transport = network.NewPeerWrapper(p.ctx, tcpConn, exit)
	p.primaryReality = false // TCP mimicry, not QUIC-REALITY, but still camouflaged
	s.mu.Lock()
	s.activePeers[exit] = p
	s.mu.Unlock()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)

	st, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err, "a TCP-fallback link to a camouflage exit must carry the tunnel")
	require.NotNil(t, st)
	require.Equal(t, int32(1), tcpConn.opened.Load(), "the stream rode the TCP link")
	st.Close()

	// A QUIC noise link to the same camouflage exit is still never used.
	noiseConn := newFakeMuxConn(ctx, 2) // QUIC
	p.addExitLink(network.NewPeerWrapper(p.ctx, noiseConn, exit), false)
	for i := 0; i < 6; i++ {
		st, err := s.OpenExitStream(ctx, exit)
		require.NoError(t, err)
		st.Close()
	}
	require.Equal(t, int32(0), noiseConn.opened.Load(), "a QUIC noise link to a camouflage exit is never used")
}

func TestExitPool_RealityConnReplacesNoisePrimaryForCamouflageExit(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0) // primaryReality defaults to false (a noise bootstrap link)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)

	// While the primary is still noise, a REALITY connection is NOT accumulated beside it: it is
	// handed back to addPeer to replace (upgrade) the primary. tryAttachExitLink returns false.
	realityConn := newFakeMuxConn(ctx, 1)
	require.False(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: realityConn, IsOut: true, Reality: true, PoolLink: true}),
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

// lateRegisterTransport answers a dial at once and registers the connection it made only later,
// as the real transport does (the dial returns, registrationLoop adds the peer afterwards). onDial
// gets the 1-based dial number and does that registration.
type lateRegisterTransport struct {
	answerTransport
	dials  atomic.Int32
	onDial func(n int)
}

func (l *lateRegisterTransport) DialEarly(context.Context, string) error {
	n := int(l.dials.Add(1))
	go l.onDial(n)
	return nil
}

// replacePrimary swaps the peer's primary connection, as addPeer does when a newer connection
// replaces the old one.
func replacePrimary(p *Peer, conn *fakeMuxConn, reality bool) {
	p.mu.Lock()
	p.transport = network.NewPeerWrapper(p.ctx, conn, p.ID())
	p.primaryReality = reality
	p.mu.Unlock()
}

// The launchpad failure: the only link to the camouflage exit is a noise one, and the REALITY dial
// registers its connection only after the dial returned. The tunnel stream must wait for that
// REALITY link instead of failing at once — a failing exit makes the VPN fail over to another
// exit, whose tunnel then runs over noise.
func TestExitPool_StreamWaitsForLateRealityLinkToCamouflageExit(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, noiseConn := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)

	realityConn := newFakeMuxConn(ctx, 1)
	tr := &lateRegisterTransport{}
	tr.onDial = func(int) {
		time.Sleep(100 * time.Millisecond)
		replacePrimary(p, realityConn, true)
	}
	s.netTransport = tr

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.OpenExitStream(sctx, exit)
	require.NoError(t, err, "the stream must wait for the REALITY link, not fail the exit")
	st.Close()
	require.Equal(t, int32(1), realityConn.opened.Load(), "the stream rode the REALITY link")
	require.Equal(t, int32(0), noiseConn.opened.Load(), "never the noise link")
	require.Equal(t, int32(1), tr.dials.Load())
}

// A REALITY dial that joins a noise dial already in flight to the same address gets that noise
// connection back. The swarm must notice the exit still has no camouflaged link and dial again.
func TestExitPool_RedialsWhenUpgradeDialYieldsNoise(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)

	noiseAgain := newFakeMuxConn(ctx, 1)
	realityConn := newFakeMuxConn(ctx, 2)
	tr := &lateRegisterTransport{}
	tr.onDial = func(n int) {
		if n == 1 {
			replacePrimary(p, noiseAgain, false) // the joined mesh dial: noise again
			return
		}
		replacePrimary(p, realityConn, true)
	}
	s.netTransport = tr

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.OpenExitStream(sctx, exit)
	require.NoError(t, err)
	st.Close()
	require.Equal(t, int32(2), tr.dials.Load(), "dialed again after the first dial brought noise")
	require.Equal(t, int32(1), realityConn.opened.Load())
	require.Equal(t, int32(0), noiseAgain.opened.Load())
}

// When no camouflaged link comes up, the stream fails once its context ends — it never falls back
// to the noise link.
func TestExitPool_NoCamouflagedLinkFailsWithoutUsingNoise(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, noiseConn := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)
	tr := &lateRegisterTransport{onDial: func(int) {}}
	s.netTransport = tr

	sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err := s.OpenExitStream(sctx, exit)
	require.Error(t, err)
	require.Equal(t, int32(0), noiseConn.opened.Load())
	require.GreaterOrEqual(t, tr.dials.Load(), int32(1))
}

// An ordinary (non-camouflage) exit is never redialed: its noise link carries the tunnel as before.
func TestExitPool_OrdinaryExitOpensOnNoiseWithoutDialing(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, noiseConn := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	tr := &lateRegisterTransport{onDial: func(int) {}}
	s.netTransport = tr

	st, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err)
	st.Close()
	require.Equal(t, int32(1), noiseConn.opened.Load())
	require.Equal(t, int32(0), tr.dials.Load())
}

// When the primary connection dies while a pool link is up, the peer is kept and the pool link
// becomes the primary; only when the last link dies is the peer dropped.
func TestExitPool_PoolLinkTakesOverWhenPrimaryDies(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, primary := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	go s.watchPeerLinks(p, exit, primary) // what addPeer starts for every peer
	link := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: link, IsOut: true, PoolLink: true}))

	_ = primary.CloseWithError(network.ErrCodeNormalClose, "killed")
	require.Eventually(t, func() bool {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.transport.GetConn() == network.MuxConn(link)
	}, 2*time.Second, 10*time.Millisecond, "the pool link should become the primary")
	require.Same(t, p, s.GetPeer(exit), "the peer must survive its first connection")

	st, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err)
	st.Close()
	require.Equal(t, int32(1), link.opened.Load())

	_ = link.CloseWithError(network.ErrCodeNormalClose, "killed")
	require.Eventually(t, func() bool { return s.GetPeer(exit) == nil }, 2*time.Second, 10*time.Millisecond,
		"with no link left the peer is dropped")
}

// Once the proxy stops using an exit, its pool links are closed; the primary stays as a mesh link.
func TestExitPool_UnmarkClosesPoolLinks(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, primary := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	link := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: link, IsOut: true, PoolLink: true}))
	require.Equal(t, 2, p.LinkCount())

	s.UnmarkExitPeer(exit)
	require.Error(t, link.Context().Err(), "the pool link should be closed")
	require.NoError(t, primary.Context().Err(), "the primary stays")
	require.Equal(t, 1, p.LinkCount())
	require.Nil(t, s.exitPoolFor(exit))
}

