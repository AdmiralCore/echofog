package network

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tightGuard(burst, failLimit int) *handshake.Guard {
	return handshake.NewGuard(handshake.GuardConfig{Burst: burst, Refill: 0.001, FailLimit: failLimit, BanFor: time.Hour, CountLoopback: true})
}

// One address that opens connection after connection is let through only so far.
func TestQUICServerLimitsConnectionBurstsOfOneAddress(t *testing.T) {
	s := newTestSigner(t)
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	server, err := NewQUICTransport("127.0.0.1:0", tlsCfg, GetQuicConfig(), s, config.CurrentProtocolVersion)
	require.NoError(t, err)
	defer server.Close()
	server.SetGuard(tightGuard(3, 100))
	require.NoError(t, server.ListenEarly(context.Background()))

	accepted, refused := 0, 0
	for i := 0; i < 8; i++ {
		cs := newTestSigner(t)
		_, ctls := testutil.GetRandomKeyAndTls(t)
		client, err := NewQUICTransport("127.0.0.1:0", ctls, GetQuicConfig(), cs, config.CurrentProtocolVersion)
		require.NoError(t, err)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		if err := client.DialEarly(ctx, server.Addr().String()); err == nil {
			accepted++
		} else {
			refused++
		}
		cancel()
	}
	assert.Equal(t, 3, accepted, "the burst allowance")
	assert.Equal(t, 5, refused)
}

func rawOpen(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

// closedByServer reports whether the server hung up on the connection without waiting for data.
func closedByServer(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	return errors.Is(err, io.EOF) || (err != nil && !isTimeout(err))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// A visitor that never presents a valid stealth token is forwarded to the real site and is never
// rate-limited, however many connections it opens. A real www.microsoft.com does not reset ordinary
// HTTPS clients, and neither may we: resetting them (a prober opening a handful of connections, or a
// NAT/CGNAT behind which many clients share one address) would single the node out under probing.
func TestTCPServerForwardsEveryVisitorWithoutRateLimiting(t *testing.T) {
	siteAddr, siteCert := camouflageSite(t)
	server := newTCPCamouflage(t, newTestSigner(t), siteAddr, false)
	// A burst allowance that the old code applied to EVERY address would have reset visitors 3+.
	server.SetGuard(tightGuard(2, 100))
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	const visitors = 8 // well past the burst of 2
	for i := 0; i < visitors; i++ {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "www.microsoft.com"})
		require.NoErrorf(t, err, "visitor %d was reset instead of forwarded to the real site", i)
		require.Truef(t, c.ConnectionState().PeerCertificates[0].Equal(siteCert), "visitor %d must see the real site's certificate, not ours", i)
		_, err = c.Write([]byte("GET /v HTTP/1.1\r\nHost: www.microsoft.com\r\nConnection: close\r\n\r\n"))
		require.NoError(t, err)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(body), "camouflage site /v")
		_ = c.Close()
	}
}

// A forwarded visitor must never cause the per-address Guard to ban the address: the ban is only
// ever earned by a connection that presented a valid stealth token and then failed the authenticated
// handshake. Here a stream of token-less probes from 127.0.0.1 must not lock out a real peer sharing
// that address.
func TestTCPServerNeverBansForwardedVisitors(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server := newNodeKeyedTCP(t, ss, "")
	// Strict ban threshold: under the old code these probes would ban 127.0.0.1 after 3 failures.
	server.SetGuard(handshake.NewGuard(handshake.GuardConfig{Burst: 1000, FailLimit: 3, BanFor: time.Hour, CountLoopback: true}))
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	for i := 0; i < 10; i++ {
		c := rawOpen(t, addr)
		_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")) // not our TLS token: forwarded, never counted
		_ = closedByServer(c)
	}
	time.Sleep(100 * time.Millisecond)

	// The address is not banned: a real peer from the same 127.0.0.1 still authenticates.
	client := newNodeKeyedTCP(t, cs, "")
	client.ExpectPeer(addr, ss.id())
	require.NoError(t, client.Dial(context.Background(), addr), "a forwarded visitor must never get the address banned")
}

// The Guard still bans a holder of a valid stealth token whose authenticated handshake keeps
// failing: that is a real attacker or broken peer, not an innocent visitor. (Here the attacker is
// on the same network as the server, so it derives the valid node token but then speaks a wrong
// protocol version, failing authentication.)
func TestTCPServerStillBansAuthenticatedFailures(t *testing.T) {
	ss := newTestSigner(t)
	server := newNodeKeyedTCP(t, ss, "")
	server.SetGuard(handshake.NewGuard(handshake.GuardConfig{Burst: 1000, FailLimit: 3, BanFor: time.Hour, CountLoopback: true}))
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	loopback := netip.MustParseAddr("127.0.0.1")
	// An attacker that knows the node token (so it clears the stealth TLS and is NOT merely
	// forwarded) but then hangs up before completing the peer handshake: a failed *authenticated*
	// handshake, which is exactly what the Guard is allowed to punish.
	cfg := MimicryConfigFrom(MimicryOptions{})
	cfg.AuthKey = mimicry.NodeStealthKey("", ss.id())
	for i := 0; i < 10 && !server.guard.Load().Banned(loopback); i++ {
		raw, err := net.DialTimeout("tcp", addr, time.Second)
		require.NoError(t, err)
		if conn, _, herr := mimicry.ClientHandshake(raw, cfg); herr == nil {
			_ = conn.Close() // valid token presented, then drop before peer auth: an authenticated failure
		}
		_ = raw.Close()
		time.Sleep(40 * time.Millisecond)
	}
	assert.True(t, server.guard.Load().Banned(loopback), "repeated authenticated-handshake failures from one address must earn a ban")
}
