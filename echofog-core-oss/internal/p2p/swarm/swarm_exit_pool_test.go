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
	// gate, when set, holds every OpenStreamSync until it is closed; entered is signalled when an
	// open starts waiting. Lets a test catch a link with a stream being opened on it.
	gate    chan struct{}
	entered chan struct{}
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
	if c.gate != nil {
		c.entered <- struct{}{}
		select {
		case <-c.gate:
		case <-c.ctx.Done():
		}
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

// Flow shaping over the pool: the fresh link a rotation dials takes the new streams, and an older
// link is closed as soon as it carries nothing — the primary too — so no single connection to the
// exit lives on carrying gigabytes. A link still carrying a transfer is kept until it ends.
func TestExitPool_FreshLinkTakesStreamsOldLinksRetireWhenIdle(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, primary := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)

	transfer, err := s.OpenExitStream(ctx, exit) // a long download on the primary
	require.NoError(t, err)
	require.Equal(t, int32(1), primary.opened.Load())

	fresh := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: fresh, IsOut: true, PoolLink: true}))
	require.Equal(t, 2, p.LinkCount(), "the busy primary stays beside the fresh link")
	require.NoError(t, primary.Context().Err())

	for i := 0; i < 3; i++ {
		st, err := s.OpenExitStream(ctx, exit)
		require.NoError(t, err)
		st.Close()
	}
	require.Equal(t, int32(3), fresh.opened.Load(), "new streams ride the fresh link")
	require.Equal(t, int32(1), primary.opened.Load(), "the old link takes no new stream")

	transfer.Close()
	p.retireOldLinks(false)
	require.Error(t, primary.Context().Err(), "the old link is closed once its transfer ended")
	require.NoError(t, fresh.Context().Err())
}

// A full pool whose links all carry transfers refuses a further link instead of closing one.
func TestExitPool_FullPoolOfBusyLinksRefusesNewLink(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)

	busy := make([]*network.Stream, 0, 3)
	open := func() {
		st, err := s.OpenExitStream(ctx, exit)
		require.NoError(t, err)
		busy = append(busy, st)
	}
	open() // on the primary
	for i := 1; i <= 2; i++ {
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: newFakeMuxConn(ctx, i), IsOut: true, PoolLink: true}))
		open() // on the link just added
	}
	require.Equal(t, 3, p.LinkCount())

	rejected := newFakeMuxConn(ctx, 9)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: rejected, IsOut: true, PoolLink: true}),
		"a full, all-busy pool still owns (and closes) the incoming connection")
	require.Equal(t, 3, p.LinkCount(), "a rejected dial must not add a link")
	require.Error(t, rejected.Context().Err(), "the rejected connection should be closed")
	for _, st := range busy {
		st.Close()
	}
}

// The race that sent clients to another exit: a stream was being opened on a link (chosen, not
// yet registered, so the link looked idle) when a fresh link arrived and the old one was closed as
// idle — the stream died, the exit check failed, and the VPN failed over. A link with a stream
// being opened on it is never retired.
func TestExitPool_LinkWithStreamBeingOpenedIsNotRetired(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, primary := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	primary.gate = make(chan struct{})
	primary.entered = make(chan struct{}, 1)

	done := make(chan error, 1)
	go func() {
		st, err := s.OpenExitStream(ctx, exit)
		if err == nil {
			st.Close()
		}
		done <- err
	}()
	<-primary.entered // the stream is being opened on the primary

	fresh := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: fresh, IsOut: true, PoolLink: true}))
	p.retireOldLinks(false)
	require.NoError(t, primary.Context().Err(), "the link a stream is being opened on must stay")

	close(primary.gate)
	require.NoError(t, <-done, "the stream opens normally")
	p.retireOldLinks(false)
	require.Error(t, primary.Context().Err(), "once the stream is done the old link is retired")
}

func TestExitPool_DeadLinkIsPrunedWhileOthersCarryOn(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)

	keep, err := s.OpenExitStream(ctx, exit) // keeps the primary busy
	require.NoError(t, err)
	conn1 := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn1, IsOut: true, PoolLink: true}))
	keep1, err := s.OpenExitStream(ctx, exit) // keeps conn1 busy
	require.NoError(t, err)
	conn2 := newFakeMuxConn(ctx, 2)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: conn2, IsOut: true, PoolLink: true}))
	require.Equal(t, 3, p.LinkCount())

	// Kill the newest link, as a censor throttling a flow to death would: it is pruned and the
	// remaining connections keep serving streams.
	_ = conn2.CloseWithError(network.ErrCodeNormalClose, "killed")
	require.Eventually(t, func() bool { return p.LinkCount() == 2 }, time.Second, 10*time.Millisecond,
		"the dead link should be pruned, leaving the other connections")

	st, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err, "the VPN keeps carrying traffic on the surviving connections")
	require.Equal(t, int32(2), conn1.opened.Load(), "the newest surviving link takes it")
	st.Close()
	keep.Close()
	keep1.Close()
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
	defer func() {
		// At its cap the exit refuses a further link rather than close one the client may be using.
		extra := newFakeMuxConn(ctx, 99)
		require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: client, Conn: extra, IsOut: false, PoolLink: true}))
		require.Error(t, extra.Context().Err(), "the link over the cap is refused")
		require.Equal(t, maxInboundPoolLinks, p.LinkCount())
		require.NoError(t, primary.Context().Err())
	}()

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

	// A camouflage exit with a REALITY primary, busy with a transfer so it stays.
	s.SetPeerSNI(exit, "www.example.com")
	require.True(t, s.IsCamouflagePeer(exit))
	p.mu.Lock()
	p.primaryReality = true
	p.mu.Unlock()

	s.MarkExitPeer(exit, 3)
	transfer, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err)
	defer transfer.Close()

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
	transfer, err := s.OpenExitStream(ctx, exit) // keeps the primary from being retired
	require.NoError(t, err)
	defer transfer.Close()
	link := newFakeMuxConn(ctx, 1)
	require.True(t, s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: link, IsOut: true, PoolLink: true}))
	require.Equal(t, 2, p.LinkCount())

	s.UnmarkExitPeer(exit)
	require.Error(t, link.Context().Err(), "the pool link should be closed")
	require.NoError(t, primary.Context().Err(), "the primary stays")
	require.Equal(t, 1, p.LinkCount())
	require.Nil(t, s.exitPoolFor(exit))
}

// A join address with ?sni= (as echofog-node prints it for a camouflage node) registers the SNI, so
// the bootstrap dial and every later one to that node use REALITY.
func TestBootstrapKeepsSNIFromJoinAddress(t *testing.T) {
	s, _ := newExitPoolSwarm(t)
	s.netTransport = &lateRegisterTransport{onDial: func(int) {}}
	var pub types.PeerPublicKey
	pub[0] = 5
	id := types.PeerPubKeyToID(pub)

	s.Bootstrap([]string{network.JoinPeerAddrWithSNI("203.0.113.15:4242", id, "cover.lab")})
	s.wg.Wait()
	require.Equal(t, "cover.lab", s.GetPeerSNI(id))
}

// The link to the exit died and the peer was dropped (UDP cut: the QUIC link idled out). The next
// tunnel stream redials the exit at the address it was last reached at — over TCP mimicry, as a
// blocked network would leave — instead of failing until the VPN failover wakes up.
func TestExitPool_RedialsExitAfterItsPeerIsGone(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, _ := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.SetPeerSNI(exit, "www.example.com")
	s.MarkExitPeer(exit, 3)
	replacePrimary(p, newFakeMuxConn(ctx, 1), true)
	st, err := s.OpenExitStream(ctx, exit) // remembers where the exit is
	require.NoError(t, err)
	st.Close()

	s.mu.Lock()
	delete(s.activePeers, exit)
	s.mu.Unlock()

	tcp := newFakeTCPConn(ctx, 2)
	var dialedAddr atomic.Value
	tr := &lateRegisterTransport{}
	tr.onDial = func(int) {
		np := NewPeer(ctx, p.pubKey, dispatcher.NewDispatcher(ctx), "203.0.113.7:443", true)
		np.transport = network.NewPeerWrapper(np.ctx, tcp, exit)
		s.mu.Lock()
		s.activePeers[exit] = np
		s.mu.Unlock()
	}
	s.netTransport = &recordingTransport{lateRegisterTransport: tr, addr: &dialedAddr}

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err = s.OpenExitStream(sctx, exit)
	require.NoError(t, err, "the stream should redial the exit, not fail")
	st.Close()
	require.Equal(t, "203.0.113.7:443", dialedAddr.Load())
	require.Equal(t, int32(1), tcp.opened.Load())
}

type recordingTransport struct {
	*lateRegisterTransport
	addr *atomic.Value
}

func (r *recordingTransport) DialEarly(ctx context.Context, addr string) error {
	r.addr.Store(addr)
	return r.lateRegisterTransport.DialEarly(ctx, addr)
}

// A link that has been up longer than freshLinkAge takes no new stream: a fresh pool link is dialed
// first and the stream rides it, so an old link left alone never wakes up to carry a burst.
func TestExitPool_StaleLinkGetsAFreshLinkBeforeANewStream(t *testing.T) {
	s, ctx := newExitPoolSwarm(t)
	p, old := addPrimaryPeer(ctx, s, 0)
	exit := p.ID()
	s.MarkExitPeer(exit, 3)
	p.mu.Lock()
	p.primarySince = time.Now().Add(-2 * freshLinkAge)
	p.mu.Unlock()

	fresh := newFakeMuxConn(ctx, 1)
	tr := &lateRegisterTransport{}
	tr.onDial = func(int) {
		s.tryAttachExitLink(network.NewConnEvent{PeerID: exit, Conn: fresh, IsOut: true, PoolLink: true})
	}
	s.netTransport = tr

	st, err := s.OpenExitStream(ctx, exit)
	require.NoError(t, err)
	st.Close()
	require.Equal(t, int32(1), tr.dials.Load(), "a fresh pool link was dialed")
	require.Equal(t, int32(1), fresh.opened.Load(), "the stream rode the fresh link")
	require.Equal(t, int32(0), old.opened.Load(), "never the stale one")

	// The fresh link is young: the next stream needs no dial.
	st, err = s.OpenExitStream(ctx, exit)
	require.NoError(t, err)
	st.Close()
	require.Equal(t, int32(1), tr.dials.Load())
}
