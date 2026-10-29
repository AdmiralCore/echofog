package network

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// socksServer is a minimal SOCKS5 proxy (no auth, CONNECT only) that records where it was asked
// to connect.
type socksServer struct {
	ln      net.Listener
	mu      sync.Mutex
	targets []string
}

func newSocksServer(t *testing.T) *socksServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &socksServer{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *socksServer) addr() string { return s.ln.Addr().String() }

func (s *socksServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

func (s *socksServer) serve(c net.Conn) {
	defer c.Close()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, hdr[1])); err != nil {
		return
	}
	_, _ = c.Write([]byte{5, 0}) // no auth
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		_, _ = io.ReadFull(c, b)
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		b := make([]byte, l[0])
		_, _ = io.ReadFull(c, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		_, _ = io.ReadFull(c, b)
		host = net.IP(b).String()
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(c, pb)
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	go func() { _, _ = io.Copy(up, c) }()
	_, _ = io.Copy(c, up)
}

func withProxy(t *testing.T, addr string, localToo bool) {
	require.NoError(t, SetOutboundProxy(addr))
	proxyLocalNet = localToo
	t.Cleanup(func() { _ = SetOutboundProxy(""); proxyLocalNet = false })
}

func TestNormalizeProxyAddr(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		" 127.0.0.1:9150 ":          "127.0.0.1:9150",
		"socks5://127.0.0.1:9050":   "127.0.0.1:9050",
		"SOCKS5H://localhost:1080/": "localhost:1080",
		"socks5://[::1]:9050":       "[::1]:9050",
	} {
		got, err := NormalizeProxyAddr(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"localhost", "127.0.0.1:0", "127.0.0.1:99999", ":9050", "http://x"} {
		_, err := NormalizeProxyAddr(bad)
		assert.Error(t, err, bad)
	}
}

// What goes through the proxy: public addresses; not the local network, not a dial marked direct,
// and nothing at all with no proxy set.
func TestProxiedRules(t *testing.T) {
	ctx := context.Background()
	assert.False(t, Proxied(ctx, "203.0.113.5:443"), "no proxy")
	assert.False(t, proxyResolvesName("relay.example:443"))

	withProxy(t, "127.0.0.1:9150", false)
	assert.True(t, Proxied(ctx, "203.0.113.5:443"))
	assert.True(t, Proxied(ctx, "relay.example:443"))
	assert.True(t, Proxied(ctx, ""), "no address known: through the proxy")
	assert.False(t, Proxied(ctx, "192.168.1.20:4242"), "the local network is dialed directly")
	assert.False(t, Proxied(ctx, "127.0.0.1:4242"))
	assert.False(t, Proxied(WithDirect(ctx), "203.0.113.5:443"), "a direct call skips the proxy")
	assert.True(t, proxyResolvesName("relay.example:443"))
	assert.False(t, proxyResolvesName("203.0.113.5:443"))
}

func newTestQUIC(t *testing.T, s testSigner) *QuicTransport {
	cert := testCert(t)
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"echo-fog-protocole"}, InsecureSkipVerify: true, ClientAuth: tls.RequestClientCert}
	q, err := NewQUICTransport("127.0.0.1:0", cfg, GetQuicConfig(), s, 101)
	require.NoError(t, err)
	return q
}

// Behind a proxy the node dials over TCP through it (not QUIC beside it), and a dial marked direct
// goes around it.
func TestFallbackTransportDialsThroughProxy(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcpAddr := probe.Addr().String()
	probe.Close()

	server := NewFallbackTransport(newTestQUIC(t, ss), newTCP(t, ss, ""), ModeAuto, tcpAddr)
	client := NewFallbackTransport(newTestQUIC(t, cs), newTCP(t, cs, ""), ModeAuto, "off-test")
	t.Cleanup(func() { server.Close(); client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, server.ListenEarly(ctx))

	socks := newSocksServer(t)
	withProxy(t, socks.addr(), true)

	require.NoError(t, client.DialEarly(ctx, tcpAddr))
	ev := nextConn(t, client.ConnChan())
	assert.Equal(t, ProtocolTCP, ev.Conn.Protocol(), "TCP only behind a proxy")
	assert.Equal(t, ss.id(), ev.PeerID)
	assert.Equal(t, []string{tcpAddr}, socks.seen(), "the dial went through the proxy")

	// a dial the user marked direct (a call) does not touch the proxy
	cs2 := newTestSigner(t)
	direct := NewFallbackTransport(newTestQUIC(t, cs2), newTCP(t, cs2, ""), ModeTCP, "off-test")
	t.Cleanup(func() { direct.Close() })
	require.NoError(t, direct.DialEarly(WithDirect(ctx), tcpAddr))
	nextConn(t, direct.ConnChan())
	assert.Len(t, socks.seen(), 1, "the direct dial went around the proxy")
}

// A proxy that is not running is reported by CheckProxy; a running one passes.
func TestCheckProxy(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	socks := newSocksServer(t)
	ctx := context.Background()
	require.NoError(t, CheckProxy(ctx, "socks5://"+socks.addr(), target.Addr().String()))
	assert.Equal(t, []string{target.Addr().String()}, socks.seen())

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := dead.Addr().String()
	dead.Close()
	assert.Error(t, CheckProxy(ctx, deadAddr, target.Addr().String()))
	assert.Error(t, CheckProxy(ctx, "", target.Addr().String()))
}
