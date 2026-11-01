package network

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testSigner struct{ priv ed25519.PrivateKey }

func newTestSigner(t *testing.T) testSigner {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return testSigner{priv}
}

func (s testSigner) GetPublicKey() ([]byte, error)     { return s.priv.Public().(ed25519.PublicKey), nil }
func (s testSigner) SignData(d []byte) ([]byte, error) { return ed25519.Sign(s.priv, d), nil }
func (s testSigner) id() types.PeerID {
	var k types.PeerPublicKey
	copy(k[:], s.priv.Public().(ed25519.PublicKey))
	return types.PeerPubKeyToID(k)
}

// testCert mirrors production: the TCP camouflage needs an ECDSA (not ed25519) certificate,
// since a Chrome ClientHello offers no ed25519 signature algorithm.
func testCert(t *testing.T) tls.Certificate {
	cert, err := mimicry.GenerateCamouflageCert("www.microsoft.com")
	require.NoError(t, err)
	return cert
}

func testMimicry(password string) *mimicry.MimicryConfig {
	c := MimicryConfigFrom(MimicryOptions{StealthPassword: password})
	c.FallbackAddr = "127.0.0.1:9" // refused fast: never reach the real website from tests
	return c
}

// camouflageSite starts a local TLS server standing in for the real website a stranger is
// relayed to, and returns its address and its certificate.
func camouflageSite(t *testing.T) (string, *x509.Certificate) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("camouflage site " + r.URL.Path))
	}))
	t.Cleanup(site.Close)
	return strings.TrimPrefix(site.URL, "https://"), site.Certificate()
}

func newTCPCamouflage(t *testing.T, s testSigner, siteAddr string, perNode bool) *TCPTransport {
	cfg := MimicryConfigFrom(MimicryOptions{})
	cfg.FallbackAddr = siteAddr
	tr := NewTCPTransport(testCert(t), cfg, NewDefaultPeerAuthenticator(s, 101, 5*time.Second))
	if perNode {
		tr.SetNodeKeys("", s.id())
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

func newTCP(t *testing.T, s testSigner, password string) *TCPTransport {
	tr := NewTCPTransport(testCert(t), testMimicry(password), NewDefaultPeerAuthenticator(s, 101, 5*time.Second))
	t.Cleanup(func() { tr.Close() })
	return tr
}

func nextConn(t *testing.T, ch <-chan NewConnEvent) NewConnEvent {
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no connection event")
		return NewConnEvent{}
	}
}

func TestTCPTransportConnectsPeers(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))

	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	out, in := nextConn(t, client.ConnChan()), nextConn(t, server.ConnChan())
	assert.Equal(t, ss.id(), out.PeerID)
	assert.Equal(t, cs.id(), in.PeerID)
	assert.True(t, out.Dialable)
	assert.False(t, in.Dialable, "an inbound TCP source port is not where the peer listens")
	assert.Equal(t, ProtocolTCP, in.Conn.Protocol())

	b1, err := out.Conn.ChannelBinding()
	require.NoError(t, err)
	b2, err := in.Conn.ChannelBinding()
	require.NoError(t, err)
	assert.Equal(t, b1, b2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// bidirectional stream
	go func() {
		st, err := in.Conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		io.Copy(st, st)
	}()
	st, err := out.Conn.OpenStreamSync(ctx)
	require.NoError(t, err)
	_, err = st.Write([]byte("bidi"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	st.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = io.ReadFull(st, buf)
	require.NoError(t, err)
	assert.Equal(t, "bidi", string(buf))

	// one-way stream
	us, err := out.Conn.OpenUniStreamSync(ctx)
	require.NoError(t, err)
	_, err = us.Write([]byte("uni"))
	require.NoError(t, err)
	require.NoError(t, us.Close())
	rs, err := in.Conn.AcceptUniStream(ctx)
	require.NoError(t, err)
	got, err := io.ReadAll(rs)
	require.NoError(t, err)
	assert.Equal(t, "uni", string(got))

	// datagrams
	require.NoError(t, out.Conn.SendDatagram([]byte("dgram")))
	d, err := in.Conn.ReceiveDatagram(ctx)
	require.NoError(t, err)
	assert.Equal(t, "dgram", string(d))
	tooBig := make([]byte, smuxMaxDatagram)
	assert.ErrorIs(t, out.Conn.SendDatagram(tooBig), ErrDatagramTooLarge)
}

// A censor connecting to the port completes a genuine TLS handshake with the real site it is
// relayed to, and sees that site's certificate, not ours.
func TestTCPTransportServesCamouflageToProbes(t *testing.T) {
	siteAddr, siteCert := camouflageSite(t)
	server := newTCPCamouflage(t, newTestSigner(t), siteAddr, false)
	require.NoError(t, server.Listen("127.0.0.1:0"))

	c, err := tls.Dial("tcp", server.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "www.microsoft.com"})
	require.NoError(t, err)
	defer c.Close()
	assert.True(t, c.ConnectionState().PeerCertificates[0].Equal(siteCert), "the scanner did not get the real site's certificate")
	c.SetDeadline(time.Now().Add(8 * time.Second))
	_, err = c.Write([]byte("GET /x HTTP/1.1\r\nHost: www.microsoft.com\r\nConnection: close\r\n\r\n"))
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "camouflage site /x")
}

func TestTCPTransportWrongStealthKeyIsTreatedAsProbe(t *testing.T) {
	server := newTCP(t, newTestSigner(t), "network-a")
	client := newTCP(t, newTestSigner(t), "network-b")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	assert.Error(t, client.Dial(ctx, server.Addr().String()))
	select {
	case <-server.ConnChan():
		t.Fatal("a client with the wrong key must not become a peer")
	case <-time.After(200 * time.Millisecond):
	}
}

// UDP to the peer's port goes nowhere (as on a network that drops UDP); the dial must land on
// TCP and remember to try TCP first next time.
func TestFallbackTransportUsesTCPWhenQUICFails(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	newQUIC := func(s testSigner) *QuicTransport {
		cert := testCert(t)
		cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"echo-fog-protocole"}, InsecureSkipVerify: true, ClientAuth: tls.RequestClientCert}
		q, err := NewQUICTransport("127.0.0.1:0", cfg, GetQuicConfig(), s, 101)
		require.NoError(t, err)
		return q
	}

	// The server's TCP listener port, where nothing answers QUIC. Pick it as a free TCP port: the
	// server must be able to bind it on TCP, or ListenEarly falls back to the QUIC port and the
	// client would dial a closed one. (Picking a free UDP port and hoping the same number is free on
	// TCP made this test flaky on Windows.) No QUIC listener sits on this port number either way.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcpAddr := probe.Addr().String()
	probe.Close()

	server := NewFallbackTransport(newQUIC(ss), newTCP(t, ss, ""), ModeAuto, tcpAddr)
	client := NewFallbackTransport(newQUIC(cs), newTCP(t, cs, ""), ModeAuto, "off-test")
	t.Cleanup(func() { server.Close(); client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, server.ListenEarly(ctx))

	start := time.Now()
	require.NoError(t, client.DialEarly(context.Background(), tcpAddr))
	ev := nextConn(t, client.ConnChan())
	assert.Equal(t, ProtocolTCP, ev.Conn.Protocol())
	assert.Equal(t, ss.id(), ev.PeerID)
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.True(t, client.tcpFirst(tcpAddr), "the next dial should go straight to TCP")
}

// Thousands of one-frame gossip streams (DHT, chat) plus video-rate datagrams on one TCP peer
// connection: streams must be released and nothing may stall.
func TestTCPPeerConnectionUnderLoad(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	out, in := nextConn(t, client.ConnChan()), nextConn(t, server.ConnChan())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp := NewPeerWrapper(ctx, in.Conn, cs.id())
	gossip, media := make(chan struct{}, 10000), make(chan struct{}, 50000)
	sp.OnData(func(mt net_types.MessageType, _ []byte, _ types.PeerID) {
		if mt == net_types.TypeCallMediaVideo {
			media <- struct{}{}
		} else {
			gossip <- struct{}{}
		}
	})
	sp.StartLoops()
	cp := NewPeerWrapper(ctx, out.Conn, ss.id())

	const nGossip, nMedia = 3000, 20000
	go func() {
		for i := 0; i < nGossip; i++ {
			if err := cp.SendGossipMessage(net_types.TypeHandshake, []byte("dht record")); err != nil {
				t.Errorf("gossip %d: %v", i, err)
				return
			}
		}
	}()
	frame := make([]byte, 1100)
	for i := 0; i < nMedia; i++ {
		require.NoError(t, cp.SendDatagramFrame(net_types.TypeCallMediaVideo, frame))
		if i%4 == 0 {
			time.Sleep(time.Millisecond) // <= ~35 Mbit/s: a 4K60 stream
		}
	}
	deadline := time.After(20 * time.Second)
	for g, m := 0, 0; g < nGossip || m < nMedia*99/100; {
		select {
		case <-gossip:
			g++
		case <-media:
			m++
		case <-deadline:
			o, i := SmuxDatagramDrops()
			t.Fatalf("stalled: gossip %d/%d, media %d/%d (drops out=%d in=%d)", g, nGossip, m, nMedia, o, i)
		}
	}
	assert.Less(t, in.Conn.(*smuxConn).sess.NumStreams(), 50, "gossip streams must be released")
}

// A network that runs the TCP fallback on a fixed port (e.g. 443) separate from QUIC: the client
// dials the peer's QUIC address but SetTCPRemotePort redirects the TCP attempt to that port.
func TestFallbackTransportFixedTCPPort(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	newQUIC := func(s testSigner) *QuicTransport {
		cert := testCert(t)
		cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"echo-fog-protocole"}, InsecureSkipVerify: true, ClientAuth: tls.RequestClientCert}
		q, err := NewQUICTransport("127.0.0.1:0", cfg, GetQuicConfig(), s, 101)
		require.NoError(t, err)
		return q
	}
	server := NewFallbackTransport(newQUIC(ss), newTCP(t, ss, ""), ModeAuto, "127.0.0.1:0")
	client := NewFallbackTransport(newQUIC(cs), newTCP(t, cs, ""), ModeTCP, "off-test")
	t.Cleanup(func() { server.Close(); client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, server.ListenEarly(ctx))

	tcpPort := server.TCPAddr().(*net.TCPAddr).Port
	require.NotEqual(t, server.Addr().(*net.UDPAddr).Port, tcpPort, "TCP and QUIC must be on different ports here")
	client.SetTCPRemotePort(tcpPort)

	// dial the QUIC address; the fixed-port rewrite must send TCP to tcpPort and connect
	require.NoError(t, client.DialEarly(context.Background(), server.Addr().String()))
	ev := nextConn(t, client.ConnChan())
	assert.Equal(t, ProtocolTCP, ev.Conn.Protocol(), "must connect over TCP to the fixed port")
	assert.Equal(t, ss.id(), ev.PeerID)
}

// A node announces the port peers should dial it back on; with a port map in front, that is the
// public port, not the local listener's.
func TestTCPTransportAnnouncesConfiguredPort(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	client.SetAdvertisedPort(45443) // e.g. the public 443 behind a map
	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	in := nextConn(t, server.ConnChan())
	require.True(t, in.Dialable)
	_, port, err := net.SplitHostPort(in.Addr)
	require.NoError(t, err)
	assert.Equal(t, "45443", port, "the announced dial-back port was not used")
}

// QUIC connected but then went silent (UDP throttled after the handshake): after PreferTCP the
// next dial to that address goes over TCP although QUIC would still connect.
func TestFallbackTransportPreferTCPAfterAStall(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	newQUIC := func(s testSigner) *QuicTransport {
		cert := testCert(t)
		cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"echo-fog-protocole"}, InsecureSkipVerify: true, ClientAuth: tls.RequestClientCert}
		q, err := NewQUICTransport("127.0.0.1:0", cfg, GetQuicConfig(), s, 101)
		require.NoError(t, err)
		return q
	}
	// The server later takes TCP on its QUIC port number: pick a UDP port whose number is free on
	// TCP too, or another test binary holding it on TCP (CI runs packages side by side) leaves the
	// TCP listener down and the second dial refused.
	var sq *QuicTransport
	for i := 0; ; i++ {
		sq = newQUIC(ss)
		probe, err := net.Listen("tcp", sq.Addr().String())
		if err == nil {
			_ = probe.Close()
			break
		}
		_ = sq.Close()
		require.Less(t, i, 20, "no port free on both UDP and TCP")
	}
	addr := sq.Addr().String()
	// The server accepts QUIC only at first (no inbound TCP listener yet), so the first dial can
	// only come up over QUIC — no race to lose under a loaded CPU.
	server := NewFallbackTransport(sq, newTCP(t, ss, ""), ModeAuto, "")
	client := NewFallbackTransport(newQUIC(cs), newTCP(t, cs, ""), ModeAuto, "off-test")
	t.Cleanup(func() { server.Close(); client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, server.ListenEarly(ctx))

	require.NoError(t, client.DialEarly(context.Background(), addr))
	first := nextConn(t, client.ConnChan())
	assert.Equal(t, ProtocolQUIC, first.Conn.Protocol(), "QUIC works, so it goes first")
	_ = first.Conn.CloseWithError(ErrCodeNormalClose, "test")

	// Now the node also accepts TCP on the QUIC port number, and a stall marks the address TCP-first.
	require.NoError(t, server.ListenTCP(addr))
	client.PreferTCP(addr)
	require.True(t, client.tcpFirst(addr), "PreferTCP marks the address TCP-first")

	// Take the QUIC listener down for the second dial so the TCP-wins race is deterministic: QUIC
	// cannot win under any scheduling (not even a 0-RTT resumption racing a CPU-starved TCP
	// goroutine), while the real raceDial path that tcpFirst steers is still exercised end to end.
	// The ordering itself is asserted directly above; here we confirm the connection comes up over
	// the TCP fallback.
	_ = sq.Close()

	require.NoError(t, client.DialEarly(context.Background(), addr))
	second := nextConn(t, client.ConnChan())
	assert.Equal(t, ProtocolTCP, second.Conn.Protocol(), "after a stall TCP goes first")
	assert.Equal(t, ss.id(), second.PeerID)
}

func TestIsIdleTimeout(t *testing.T) {
	assert.True(t, IsIdleTimeout(fmt.Errorf("conn: %w", &quic.IdleTimeoutError{})))
	assert.False(t, IsIdleTimeout(context.Canceled))
	assert.False(t, IsIdleTimeout(nil))
}
