package network

import (
	"bytes"
	"context"
	"math/bits"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type obfsNode struct {
	tr *QuicTransport
	id types.PeerID
}

func newObfsNode(t *testing.T, direct bool, secret string) obfsNode {
	t.Helper()
	s := newTestSigner(t)
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	tr, err := NewQUICTransportWithOptions("127.0.0.1:0", tlsCfg, GetQuicConfig(), s, config.CurrentProtocolVersion, TransportOptions{
		DirectSocket: direct,
		Obfs:         true,
		OwnID:        s.id(),
		ObfsSecret:   secret,
	})
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })
	return obfsNode{tr: tr, id: s.id()}
}

func (n obfsNode) addr() string { return n.tr.Addr().String() }

func dialWithin(n obfsNode, addr string, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return n.tr.DialEarly(ctx, addr)
}

func TestPerNodeObfs_PeersConnect(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "muxed"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			server, client := newObfsNode(t, direct, ""), newObfsNode(t, direct, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			require.NoError(t, server.tr.ListenEarly(ctx))

			client.tr.ExpectPeer(server.addr(), server.id)
			require.NoError(t, dialWithin(client, server.addr(), 5*time.Second))

			in := nextConn(t, server.tr.ConnChan())
			out := nextConn(t, client.tr.ConnChan())
			assert.Equal(t, client.id, in.PeerID)
			assert.Equal(t, server.id, out.PeerID)
		})
	}
}

func TestPerNodeObfs_DialNeedsThePeerID(t *testing.T) {
	server, client := newObfsNode(t, false, ""), newObfsNode(t, false, "")
	require.NoError(t, server.tr.ListenEarly(context.Background()))
	err := dialWithin(client, server.addr(), 2*time.Second)
	assert.ErrorIs(t, err, ErrPeerUnknown)
}

// The ID of another node gives another key: the server cannot even read the first packet.
func TestPerNodeObfs_WrongIDNeverConnects(t *testing.T) {
	server, client, stranger := newObfsNode(t, false, ""), newObfsNode(t, false, ""), newObfsNode(t, false, "")
	require.NoError(t, server.tr.ListenEarly(context.Background()))
	client.tr.ExpectPeer(server.addr(), stranger.id)
	assert.Error(t, dialWithin(client, server.addr(), 1500*time.Millisecond))
	select {
	case <-server.tr.ConnChan():
		t.Fatal("the server saw a connection that used another node's key")
	case <-time.After(200 * time.Millisecond):
	}
}

// Nodes of a private network share an extra secret; without it nobody gets in, even knowing the ID.
func TestPerNodeObfs_PrivateNetworkSecret(t *testing.T) {
	server := newObfsNode(t, false, "our-network")
	insider := newObfsNode(t, false, "our-network")
	outsider := newObfsNode(t, false, "")
	require.NoError(t, server.tr.ListenEarly(context.Background()))

	outsider.tr.ExpectPeer(server.addr(), server.id)
	assert.Error(t, dialWithin(outsider, server.addr(), 1500*time.Millisecond))

	insider.tr.ExpectPeer(server.addr(), server.id)
	assert.NoError(t, dialWithin(insider, server.addr(), 5*time.Second))
}

// The peer that answers must be the one the address was announced for.
func TestPerNodeObfs_DetectsImpostorOnOpenPort(t *testing.T) {
	// obfuscation off on both ends, so only the identity check can catch the swap
	s1, s2 := newTestSigner(t), newTestSigner(t)
	_, tls1 := testutil.GetRandomKeyAndTls(t)
	_, tls2 := testutil.GetRandomKeyAndTls(t)
	server, err := NewQUICTransport("127.0.0.1:0", tls1, GetQuicConfig(), s1, config.CurrentProtocolVersion)
	require.NoError(t, err)
	defer server.Close()
	client, err := NewQUICTransport("127.0.0.1:0", tls2, GetQuicConfig(), s2, config.CurrentProtocolVersion)
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, server.ListenEarly(context.Background()))

	other := newTestSigner(t).id()
	client.ExpectPeer(server.Addr().String(), other)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = client.DialEarly(ctx, server.Addr().String())
	assert.ErrorIs(t, err, ErrPeerMismatch)
	var mismatch *PeerMismatchError
	if assert.ErrorAs(t, err, &mismatch) {
		assert.Equal(t, s1.id(), mismatch.Got, "the error names who did answer")
	}
}

// A scanner firing QUIC packets, or random bytes, at an obfuscating node gets no reply at all.
func TestPerNodeObfs_ScannerGetsSilence(t *testing.T) {
	server := newObfsNode(t, false, "")
	require.NoError(t, server.tr.ListenEarly(context.Background()))

	scan, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer scan.Close()
	to := server.tr.Addr()

	initial := make([]byte, 1200)
	initial[0] = 0xC3 // QUIC Initial, long header
	copy(initial[1:5], []byte{0, 0, 0, 1})
	_, _ = scan.WriteTo(initial, to)
	_, _ = scan.WriteTo(bytes.Repeat([]byte{0x16, 0x03, 0x01}, 100), to) // a TLS-looking blob
	_, _ = scan.WriteTo(bytes.Repeat([]byte{0xAB}, 64), to)

	_ = scan.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, _, err := scan.ReadFrom(make([]byte, 2048))
	assert.Error(t, err, "the node answered a scanner with %d bytes", n)
}

// udpTap forwards UDP between a client and a server and keeps a copy of every datagram, like a
// DPI box sitting on the path.
type udpTap struct {
	conn    *net.UDPConn
	server  *net.UDPAddr
	mu      sync.Mutex
	client  *net.UDPAddr
	packets [][]byte
}

func newUDPTap(t *testing.T, server net.Addr) *udpTap {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	tap := &udpTap{conn: c, server: server.(*net.UDPAddr)}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			tap.mu.Lock()
			tap.packets = append(tap.packets, append([]byte(nil), buf[:n]...))
			if from.Port != tap.server.Port {
				tap.client = from
			}
			dst := tap.server
			if from.Port == tap.server.Port {
				dst = tap.client
			}
			tap.mu.Unlock()
			if dst != nil {
				_, _ = c.WriteToUDP(buf[:n], dst)
			}
		}
	}()
	return tap
}

func (p *udpTap) seen() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.packets...)
}

// Everything a observer on the path sees of a whole connection is noise.
func TestPerNodeObfs_WireIsNoise(t *testing.T) {
	server, client := newObfsNode(t, false, ""), newObfsNode(t, false, "")
	require.NoError(t, server.tr.ListenEarly(context.Background()))
	tap := newUDPTap(t, server.tr.Addr())

	tapAddr := tap.conn.LocalAddr().String()
	client.tr.ExpectPeer(tapAddr, server.id)
	require.NoError(t, dialWithin(client, tapAddr, 5*time.Second))
	nextConn(t, server.tr.ConnChan())
	time.Sleep(200 * time.Millisecond)

	pkts := tap.seen()
	require.GreaterOrEqual(t, len(pkts), 4, "a handshake takes several datagrams")
	var all []byte
	ones, total := 0, 0
	for _, p := range pkts {
		all = append(all, p...)
		for _, b := range p {
			ones += bits.OnesCount8(b)
		}
		total += len(p) * 8
	}
	// no text that identifies the protocol or the identity certificate
	for _, marker := range [][]byte{[]byte("echo-fog"), []byte("EchoFog"), []byte("quic-go"), []byte("microsoft")} {
		assert.False(t, bytes.Contains(all, marker), "found %q on the wire", marker)
	}
	// QUIC long headers have a fixed shape; obfuscated first bytes must not
	long := 0
	for _, p := range pkts {
		if p[0]&0xC0 == 0xC0 {
			long++
		}
	}
	assert.Less(t, long, len(pkts)*3/4+1, "too many datagrams look like QUIC long headers")
	if total >= 8*3000 { // enough data for the bit balance to mean something
		ratio := float64(ones) / float64(total)
		assert.InDelta(t, 0.5, ratio, 0.02)
	}
}
