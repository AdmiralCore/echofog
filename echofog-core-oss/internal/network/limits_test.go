package network

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var loopback = netip.MustParseAddr("127.0.0.1")

// A server makes every new address answer a QUIC Retry before any TLS work; the Retry goes out
// through the obfuscation layer like any reply, and the peer still connects.
func TestQUICRetryAlwaysThroughObfuscation(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "muxed"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			server, client := newObfsNode(t, direct, ""), newObfsNode(t, direct, "")
			server.tr.SetRetryAlways(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			require.NoError(t, server.tr.ListenEarly(ctx))

			client.tr.ExpectPeer(server.addr(), server.id)
			start := time.Now()
			require.NoError(t, dialWithin(client, server.addr(), 5*time.Second))
			t.Logf("connect with Retry took %v", time.Since(start))
			assert.Greater(t, server.tr.initials.Rate(), 0.0, "the server was asked whether to send a Retry")
			ev := nextConn(t, server.tr.ConnChan())
			assert.Equal(t, client.id, ev.PeerID)
			assert.Equal(t, loopback, ev.RemoteIP)
		})
	}
}

// Under a flood of new connections the Retry turns on by itself.
func TestQUICRetryUnderLoad(t *testing.T) {
	n := newObfsNode(t, false, "")
	assert.False(t, n.tr.verifySourceAddress(nil), "calm: no Retry")
	for i := 0; i < int(retryRate*4); i++ {
		n.tr.verifySourceAddress(nil)
	}
	assert.True(t, n.tr.verifySourceAddress(nil), "a burst of Initials asks for Retry")
}

func newPlainQUIC(t *testing.T, s testSigner) *QuicTransport {
	t.Helper()
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	tr, err := NewQUICTransport("127.0.0.1:0", tlsCfg, GetQuicConfig(), s, config.CurrentProtocolVersion)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })
	return tr
}

// A banned address is refused before its TLS handshake, and so is one that holds all the peers it
// may.
func TestQUICRefusesBannedAndFullAddressBeforeTLS(t *testing.T) {
	server := newPlainQUIC(t, newTestSigner(t))
	g := handshake.NewGuard(handshake.GuardConfig{CountLoopback: true, MaxPeersPerIP: 1})
	server.SetGuard(g)
	require.NoError(t, server.ListenEarly(context.Background()))

	g.Ban(loopback, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := newPlainQUIC(t, newTestSigner(t))
	assert.Error(t, client.DialEarly(ctx, server.Addr().String()), "banned")

	g2 := handshake.NewGuard(handshake.GuardConfig{CountLoopback: true, MaxPeersPerIP: 1})
	server.SetGuard(g2)
	release, err := g2.AdmitPeer(loopback, types.PeerID{1})
	require.NoError(t, err)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	assert.Error(t, newPlainQUIC(t, newTestSigner(t)).DialEarly(ctx2, server.Addr().String()), "address full")

	release()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel3()
	require.NoError(t, newPlainQUIC(t, newTestSigner(t)).DialEarly(ctx3, server.Addr().String()), "room again")
}

// A peer refused because the node is full did nothing wrong: it must not be counted towards a ban.
func TestQUICRefusalWhenFullIsNoFailure(t *testing.T) {
	server := newPlainQUIC(t, newTestSigner(t))
	g := handshake.NewGuard(handshake.GuardConfig{Burst: 1000, FailLimit: 2, BanFor: time.Hour, CountLoopback: true})
	server.SetGuard(g)
	server.SetScreen(func(types.PeerPublicKey) (uint32, bool) { return 0, true })
	require.NoError(t, server.ListenEarly(context.Background()))
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		assert.Error(t, newPlainQUIC(t, newTestSigner(t)).DialEarly(ctx, server.Addr().String()))
		cancel()
	}
	time.Sleep(50 * time.Millisecond)
	assert.False(t, g.Banned(loopback))
}

// The WebSocket transport has the same Guard as QUIC and TCP: a token holder whose authenticated
// handshake keeps failing is banned, and a banned address cannot get in.
func TestWSGuardBansAuthenticatedFailures(t *testing.T) {
	ss := newTestSigner(t)
	server := newWS(t, ss, "/p", true)
	g := handshake.NewGuard(handshake.GuardConfig{Burst: 1000, FailLimit: 3, BanFor: time.Hour, CountLoopback: true})
	server.SetGuard(g)
	require.NoError(t, server.Listen("127.0.0.1:0"))
	url := "ws://" + server.Addr().String() + "/p"

	for i := 0; i < 6 && !g.Banned(loopback); i++ {
		// an outdated protocol: the token is right, the peer handshake fails
		old := NewWSTransport(testCert(t), testMimicry(""), NewDefaultPeerAuthenticator(newTestSigner(t), 50, 2*time.Second), "/p")
		old.perNode, old.secret = true, ""
		old.ExpectPeer(url, ss.id())
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		_ = old.Dial(ctx, url)
		cancel()
		_ = old.Close()
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, g.Banned(loopback), "failed authenticated handshakes over WebSocket earn a ban")

	client := newWS(t, newTestSigner(t), "/p", true)
	client.ExpectPeer(url, ss.id())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	assert.Error(t, client.Dial(ctx, url), "a banned address does not get in over WebSocket either")
}

// Behind a CDN the client's own address comes from the configured header; without one, or with
// garbage in it, the connection's source address counts.
func TestWSClientIP(t *testing.T) {
	tr := newWS(t, newTestSigner(t), "/p", false)
	r := &http.Request{RemoteAddr: "198.51.100.1:5555", Header: http.Header{}}
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	assert.Equal(t, netip.MustParseAddr("198.51.100.1"), tr.clientIP(r), "no header configured")

	tr.SetClientIPHeader("CF-Connecting-IP")
	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), tr.clientIP(r))

	tr.SetClientIPHeader("X-Forwarded-For")
	r.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.1")
	assert.Equal(t, netip.MustParseAddr("203.0.113.10"), tr.clientIP(r))
	r.Header.Set("X-Forwarded-For", "nonsense")
	assert.Equal(t, netip.MustParseAddr("198.51.100.1"), tr.clientIP(r))
}

// TCP and QUIC of one node share one Guard, so a ban or a full address holds on both.
func TestFallbackSharesGuard(t *testing.T) {
	ss := newTestSigner(t)
	q := newPlainQUIC(t, ss)
	tcp := newNodeKeyedTCP(t, ss, "")
	f := NewFallbackTransport(q, tcp, ModeAuto, "")
	defer f.Close()
	assert.Same(t, q.Guard(), tcp.Guard())
	g := handshake.NewGuard(handshake.GuardConfig{})
	f.SetGuard(g)
	assert.Same(t, g, q.Guard())
	assert.Same(t, g, tcp.Guard())
}
