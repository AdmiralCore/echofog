package network

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// While the condition holds the node announces no port, so the peer it dials cannot spread its
// address; once it stops holding the port is announced again.
func TestHideListenWhen(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	client.SetAdvertisedPort(45443)
	var hide atomic.Bool
	hide.Store(true)
	client.SetHideListenWhen(func(net.Addr) bool { return hide.Load() })

	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	in := nextConn(t, server.ConnChan())
	assert.False(t, in.Dialable, "a hidden port was announced")

	hide.Store(false)
	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	in = nextConn(t, server.ConnChan())
	assert.True(t, in.Dialable, "the port is announced again once the condition is gone")
}

// A hidden node announces no port over TCP either: the port sent after authentication follows
// the handshake's.
func TestHiddenNodeAnnouncesNoTCPPort(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	client.SetAdvertisedPort(45443)
	client.SetAccess(nil, nil, true)

	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	in := nextConn(t, server.ConnChan())
	assert.False(t, in.Dialable, "a hidden node's port was announced")
}
