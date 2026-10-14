package network

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/handshake"
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

func TestTCPServerLimitsConnectionBurstsOfOneAddress(t *testing.T) {
	server := newTCP(t, newTestSigner(t), "")
	server.SetGuard(tightGuard(3, 100))
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	held := make([]net.Conn, 0, 3)
	for i := 0; i < 3; i++ {
		c := rawOpen(t, addr)
		held = append(held, c)
	}
	turnedAway := 0
	for i := 0; i < 4; i++ {
		if closedByServer(rawOpen(t, addr)) {
			turnedAway++
		}
	}
	assert.Equal(t, 4, turnedAway, "after the burst the server hangs up at once")
	for _, c := range held {
		assert.False(t, closedByServer(c), "the first connections were let in")
	}
}

// A scanner that sends garbage again and again gets banned; a real peer is unaffected.
func TestTCPServerBansAScannerButNotAPeer(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server := newNodeKeyedTCP(t, ss, "")
	server.SetGuard(handshake.NewGuard(handshake.GuardConfig{Burst: 1000, FailLimit: 3, BanFor: time.Hour, CountLoopback: true}))
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	for i := 0; i < 3; i++ {
		c := rawOpen(t, addr)
		_, _ = c.Write([]byte("GET / HTTP/1.1\r\n\r\n")) // not TLS: the handshake fails
		_ = closedByServer(c)
	}
	time.Sleep(100 * time.Millisecond)
	assert.True(t, closedByServer(rawOpen(t, addr)), "the scanner is banned")

	// the ban is by address, and all of this runs from 127.0.0.1, so even the peer is turned away
	// now; with the ban lifted (a fresh guard) a real peer connects
	server.SetGuard(tightGuard(1000, 3))
	client := newNodeKeyedTCP(t, cs, "")
	client.ExpectPeer(addr, ss.id())
	require.NoError(t, client.Dial(context.Background(), addr))
}
