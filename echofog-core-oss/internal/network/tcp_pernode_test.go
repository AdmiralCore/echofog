package network

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newNodeKeyedTCP(t *testing.T, s testSigner, secret string) *TCPTransport {
	tr := newTCP(t, s, "")
	tr.SetNodeKeys(secret, s.id())
	return tr
}

func TestTCPPerNodeStealthConnects(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newNodeKeyedTCP(t, ss, ""), newNodeKeyedTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	client.ExpectPeer(addr, ss.id())
	require.NoError(t, client.Dial(context.Background(), addr))
	out, in := nextConn(t, client.ConnChan()), nextConn(t, server.ConnChan())
	assert.Equal(t, ss.id(), out.PeerID)
	assert.Equal(t, cs.id(), in.PeerID)
}

func TestTCPPerNodeStealthNeedsThePeerID(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newNodeKeyedTCP(t, ss, ""), newNodeKeyedTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	assert.ErrorIs(t, client.Dial(context.Background(), server.Addr().String()), ErrPeerUnknown)
}

// Another node's ID is another key: the server treats the client as a visitor and shows the
// camouflage site, and the dial fails.
func TestTCPPerNodeStealthWrongIDSeesCamouflage(t *testing.T) {
	ss, cs, other := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	server, client := newNodeKeyedTCP(t, ss, ""), newNodeKeyedTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	client.ExpectPeer(addr, other.id())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.Error(t, client.Dial(ctx, addr))
	select {
	case <-server.ConnChan():
		t.Fatal("a client with the wrong key got in")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTCPPerNodeStealthPrivateSecret(t *testing.T) {
	ss, cs, os := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	server := newNodeKeyedTCP(t, ss, "our-network")
	insider := newNodeKeyedTCP(t, cs, "our-network")
	outsider := newNodeKeyedTCP(t, os, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	outsider.ExpectPeer(addr, ss.id())
	assert.Error(t, outsider.Dial(context.Background(), addr))
	insider.ExpectPeer(addr, ss.id())
	assert.NoError(t, insider.Dial(context.Background(), addr))
}

func TestTCPPerNodeStealthProbeStillGetsCamouflage(t *testing.T) {
	siteAddr, _ := camouflageSite(t)
	server := newTCPCamouflage(t, newTestSigner(t), siteAddr, true)
	require.NoError(t, server.Listen("127.0.0.1:0"))

	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, server.Addr().String())
		}},
	}
	resp, err := hc.Get("https://www.microsoft.com/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "camouflage site")
}

// A node that knows the key of ID X but authenticates as another peer is not the node the
// address was announced for.
func TestTCPPerNodeStealthDetectsImpostor(t *testing.T) {
	ss, cs, announced := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	server := newTCP(t, ss, "")
	server.SetNodeKeys("", announced.id()) // accepts the key that belongs to the announced ID
	client := newNodeKeyedTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	addr := server.Addr().String()

	client.ExpectPeer(addr, announced.id())
	assert.ErrorIs(t, client.Dial(context.Background(), addr), ErrPeerMismatch)
}
