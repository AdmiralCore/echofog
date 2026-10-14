package network

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWS(t *testing.T, s testSigner, path string, perNode bool) *WSTransport {
	tr := NewWSTransport(testCert(t), testMimicry(""), NewDefaultPeerAuthenticator(s, 101, 5*time.Second), path)
	if perNode {
		tr.SetNodeKeys("", s.id())
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

// A peer reaches another through a WebSocket (as it would through a CDN), authenticates, and data
// flows both ways inside the inner TLS the CDN cannot see into.
func TestWSTransportConnectsPeers(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server := newWS(t, ss, "/echofog", true)
	client := newWS(t, cs, "/echofog", true)
	require.NoError(t, server.Listen("127.0.0.1:0"))

	url := "ws://" + server.Addr().String() + "/echofog"
	client.ExpectPeer(url, ss.id())
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	require.NoError(t, client.Dial(ctx, url))

	out := nextConn(t, client.ConnChan())
	in := nextConn(t, server.ConnChan())
	assert.Equal(t, ss.id(), out.PeerID)
	assert.Equal(t, cs.id(), in.PeerID)
	assert.Equal(t, ProtocolTCP, out.Conn.Protocol())

	// a bidirectional stream carries data, through the vision padding and the inner TLS
	go func() {
		st, err := out.Conn.OpenStreamSync(ctx)
		if err != nil {
			return
		}
		_, _ = st.Write([]byte("hello over a websocket"))
		_ = st.Close()
	}()
	st, err := in.Conn.AcceptStream(ctx)
	require.NoError(t, err)
	got, err := io.ReadAll(st)
	require.NoError(t, err)
	assert.Equal(t, "hello over a websocket", string(got))
}

// A plain HTTP request (a scanner, or a CDN health check) to the endpoint gets an ordinary 404,
// not a hint of the protocol.
func TestWSTransportServesNotFoundToPlainHTTP(t *testing.T) {
	server := newWS(t, newTestSigner(t), "/echofog", false)
	require.NoError(t, server.Listen("127.0.0.1:0"))
	resp, err := http.Get("http://" + server.Addr().String() + "/echofog")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A dial to the wrong peer ID over WS is refused, like over TCP.
func TestWSTransportDetectsWrongPeer(t *testing.T) {
	ss, cs, announced := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	server := newWS(t, ss, "/p", false)
	server.SetNodeKeys("", announced.id())
	client := newWS(t, cs, "/p", true)
	require.NoError(t, server.Listen("127.0.0.1:0"))
	url := "ws://" + server.Addr().String() + "/p"
	client.ExpectPeer(url, announced.id())
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	assert.ErrorIs(t, client.Dial(ctx, url), ErrPeerMismatch)
}
