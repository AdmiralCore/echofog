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

// clientInterceptorConn wraps client's PacketConn to inject the stealth token into Initial packets
type clientInterceptorConn struct {
	net.PacketConn
	authKey []byte
}

func (c *clientInterceptorConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if IsQUICInitial(p) {
		if modified, _, ok := InjectInitialToken(p, c.authKey); ok {
			p = modified
		}
	}
	return c.PacketConn.WriteTo(p, addr)
}

func (c *clientInterceptorConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return c.PacketConn.ReadFrom(p)
}

// serverInterceptorConn wraps server's PacketConn to verify and strip the stealth token
type serverInterceptorConn struct {
	net.PacketConn
	authKey    []byte
	mu         sync.Mutex
	authed     map[string]bool
	authedDCID map[string]bool
}

func (s *serverInterceptorConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return s.PacketConn.WriteTo(p, addr)
}

func (s *serverInterceptorConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := s.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if IsQUICInitial(p[:n]) {
			valid, dcid := VerifyQUICInitialToken(p[:n], s.authKey)
			dcidStr := string(dcid)

			s.mu.Lock()
			isAuthed := s.authed[addr.String()] || (len(dcid) > 0 && s.authedDCID[dcidStr])
			s.mu.Unlock()

			if !valid && !isAuthed {
				// Reject or forward active probe
				continue
			}

			s.mu.Lock()
			s.authed[addr.String()] = true
			if len(dcid) > 0 {
				s.authedDCID[dcidStr] = true
			}
			s.mu.Unlock()

			if clean, ok := StripInitialToken(p[:n]); ok {
				copy(p, clean)
				return len(clean), addr, nil
			}
			return n, addr, nil
		}
		return n, addr, nil
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

	cliConn := &clientInterceptorConn{
		PacketConn: cliUDP,
		authKey:    authKey,
	}
	cliTr := &quic.Transport{Conn: cliConn}
	defer cliTr.Close()

	clientTLS := &tls.Config{
		ServerName:         "cover.lab",
		NextProtos:         []string{"h3"},
		InsecureSkipVerify: true,
	}
	clientQUIC := &quic.Config{
		ChromeParrot: true,
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
