package mimicry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateTestCert(name string) tls.Certificate {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// serverInterceptorConn wraps the server's PacketConn to verify the stealth token carried in the
// ClientHello random. The token is bound to the ClientHello's key_share, which (with ML-KEM) spans
// both of a client's Initials, so the interceptor buffers a source's Initials until the ClientHello
// reassembles, then verifies the whole flight once. A verified flight is replayed to quic-go unchanged
// (the token is a genuine part of the handshake, nothing to strip); a stranger's is dropped (a real
// node forwards it to the cover site). This mirrors QuicRealityConn's buffered gated routing in
// miniature.
type serverInterceptorConn struct {
	net.PacketConn
	authKey    []byte
	mu         sync.Mutex
	authed     map[string]bool
	authedDCID map[string]bool
	pending    map[string]*intPending
	drain      []intPkt
}

type intPending struct {
	frags map[int][]byte
	raws  [][]byte
	dcid  []byte
}

type intPkt struct {
	b    []byte
	addr net.Addr
}

func (s *serverInterceptorConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return s.PacketConn.WriteTo(p, addr)
}

func (s *serverInterceptorConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		s.mu.Lock()
		if len(s.drain) > 0 {
			d := s.drain[0]
			s.drain = s.drain[1:]
			s.mu.Unlock()
			return copy(p, d.b), d.addr, nil
		}
		s.mu.Unlock()

		n, addr, err := s.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if !IsQUICInitial(p[:n]) {
			return n, addr, nil
		}

		key := addr.String()
		dcid, frags, okDec := QUICInitialCrypto(p[:n])
		raw := append([]byte(nil), p[:n]...)

		s.mu.Lock()
		if s.authed[key] || (len(dcid) > 0 && s.authedDCID[string(dcid)]) {
			s.mu.Unlock()
			return n, addr, nil
		}
		if s.pending == nil {
			s.pending = map[string]*intPending{}
		}
		ph := s.pending[key]
		if ph == nil {
			ph = &intPending{frags: map[int][]byte{}}
			s.pending[key] = ph
		}
		ph.raws = append(ph.raws, raw)
		if okDec {
			if ph.dcid == nil && len(dcid) > 0 {
				ph.dcid = dcid
			}
			ph.frags = MergeCryptoFrags(ph.frags, frags)
		}
		hello := AssembleClientHello(ph.frags)
		if !QUICClientHelloComplete(hello) {
			s.mu.Unlock()
			continue // keep buffering
		}
		delete(s.pending, key)
		if !VerifyQUICClientHelloToken(hello, s.authKey) {
			s.mu.Unlock()
			continue // stranger: a real node forwards it to the cover site
		}
		s.authed[key] = true
		if len(ph.dcid) > 0 {
			s.authedDCID[string(ph.dcid)] = true
		}
		for _, r := range ph.raws {
			s.drain = append(s.drain, intPkt{b: r, addr: addr})
		}
		s.mu.Unlock()
	}
}

func TestQUICReality_FullHandshake(t *testing.T) {
	authKey := NodeStealthKey("test-reality-key", [32]byte{77})
	camoCert := generateTestCert("cover.lab")

	srvUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer srvUDP.Close()

	srvConn := &serverInterceptorConn{
		PacketConn: srvUDP,
		authKey:    authKey,
		authed:     make(map[string]bool),
		authedDCID: make(map[string]bool),
	}
	srvTr := &quic.Transport{Conn: srvConn}
	defer srvTr.Close()

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{camoCert},
		NextProtos:   []string{"h3", "echofog"},
	}
	ln, err := srvTr.ListenEarly(serverTLS, &quic.Config{
		MaxIncomingStreams: 100,
	})
	require.NoError(t, err)
	defer ln.Close()

	// Server accept goroutine
	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			srvErr <- err
			return
		}
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			srvErr <- err
			return
		}
		_, err = io.WriteString(stream, "hello from stealth server")
		_ = stream.Close()
		srvErr <- err
	}()

	// Client setup
	cliUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer cliUDP.Close()

	cliTr := &quic.Transport{Conn: cliUDP}
	defer cliTr.Close()

	// The stealth token travels in the ClientHello random, seeded through tls.Config.Rand, exactly
	// as the production REALITY dial does; no per-packet injection. QUICInitialRand predicts the
	// ChromeParrot key_share from a deterministic stream and binds the token to it.
	initialRand, err := QUICInitialRand(authKey, []string{"h3"})
	require.NoError(t, err)
	clientTLS := &tls.Config{
		ServerName:         "cover.lab",
		NextProtos:         []string{"h3"},
		InsecureSkipVerify: true,
		Rand:               initialRand,
	}
	// ChromeParrot with 1250-byte Initials, exactly as the production REALITY dial does: uTLS reads the
	// ClientHello random from tls.Config.Rand first, so the token lands there, and the same deterministic
	// stream reproduces the key_share the token is bound to.
	clientQUIC := &quic.Config{
		InitialPacketSize: 1250,
		ChromeParrot:      true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := cliTr.Dial(ctx, srvUDP.LocalAddr(), clientTLS, clientQUIC)
	require.NoError(t, err, "ChromeParrot client dialing stealth server must succeed")
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(ctx)
	require.NoError(t, err)
	_, err = stream.Write([]byte("ping"))
	require.NoError(t, err)

	reply, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, "hello from stealth server", string(reply))

	require.NoError(t, <-srvErr)
	assert.True(t, srvConn.authed[cliUDP.LocalAddr().String()])
}
