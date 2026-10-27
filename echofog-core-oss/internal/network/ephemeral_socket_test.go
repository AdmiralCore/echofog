package network

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func portOf(t *testing.T, a net.Addr) int {
	t.Helper()
	ua, ok := a.(*net.UDPAddr)
	require.True(t, ok, "expected a UDP address, got %T", a)
	return ua.Port
}

// A noise dial normally leaves from the node's shared main socket, so a rotated exit tunnel would
// reuse that one 5-tuple and a censor would still see one continuous flow. WithEphemeralSocket
// gives the dial its own socket: the connection still authenticates (the obfuscation keys exactly
// as the main socket would), but every rotated flow has a distinct 5-tuple.
func TestEphemeralSocket_NoiseDialRotatesFiveTuple(t *testing.T) {
	server := newObfsNode(t, false, "")
	client := newObfsNode(t, false, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, server.tr.ListenEarly(ctx))

	client.tr.ExpectPeer(server.addr(), server.id)

	dial := func(ctx context.Context) int {
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		defer dcancel()
		require.NoError(t, client.tr.DialEarly(dctx, server.addr()))
		in := nextConn(t, server.tr.ConnChan())
		require.Equal(t, client.id, in.PeerID)
		return portOf(t, in.Conn.RemoteAddr())
	}

	// Ordinary noise dial: it goes out from the main shared socket.
	ordinary := dial(context.Background())
	assert.Equal(t, client.tr.UDPPort(), ordinary, "an ordinary noise dial uses the main socket")

	// Rotated noise dials: each gets its own ephemeral socket, so a fresh 5-tuple every time, and
	// never the main socket's.
	eph1 := dial(WithEphemeralSocket(context.Background()))
	eph2 := dial(WithEphemeralSocket(context.Background()))
	assert.NotEqual(t, client.tr.UDPPort(), eph1, "a rotated noise dial must not reuse the main socket")
	assert.NotEqual(t, client.tr.UDPPort(), eph2, "a rotated noise dial must not reuse the main socket")
	assert.NotEqual(t, eph1, eph2, "each rotated noise dial must be a distinct 5-tuple")
}

// A pool link is announced in the handshake: the exit sees the client's rotation dial flagged as a
// pool link (and keeps it beside the existing connection), while an ordinary dial is not flagged.
func TestPoolLink_FlagReachesBothEnds(t *testing.T) {
	server := newObfsNode(t, false, "")
	client := newObfsNode(t, false, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, server.tr.ListenEarly(ctx))
	client.tr.ExpectPeer(server.addr(), server.id)

	dial := func(ctx context.Context) (out, in NewConnEvent) {
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		defer dcancel()
		require.NoError(t, client.tr.DialEarly(dctx, server.addr()))
		return nextConn(t, client.tr.ConnChan()), nextConn(t, server.tr.ConnChan())
	}

	out, in := dial(context.Background())
	assert.False(t, out.PoolLink, "an ordinary dial is not a pool link")
	assert.False(t, in.PoolLink, "an ordinary dial is not announced as a pool link")

	out, in = dial(WithPoolLink(WithEphemeralSocket(context.Background())))
	assert.True(t, out.PoolLink, "the dialer knows its pool link")
	assert.True(t, in.PoolLink, "the exit learns from the handshake that it is a pool link")
	assert.Equal(t, client.id, in.PeerID)
}
