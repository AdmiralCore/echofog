package quicx

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/carrier/carriertest"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

const handshakeTimeout = 10 * time.Second

// dialPair establishes one QUIC client/server connection over IPv4 loopback and registers its
// teardown. Every conformance subtest gets its own listener and socket.
func dialPair(t *testing.T, clientTLS, serverTLS *tls.Config, clientConf, serverConf *quic.Config) (client, server *quic.Conn) {
	t.Helper()

	serverUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	ln, err := quic.Listen(serverUDP, serverTLS, serverConf)
	if err != nil {
		_ = serverUDP.Close()
		t.Fatalf("quic.Listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = serverUDP.Close()
	})

	clientUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen client udp: %v", err)
	}
	t.Cleanup(func() { _ = clientUDP.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), handshakeTimeout)
	defer cancel()

	type accepted struct {
		conn *quic.Conn
		err  error
	}
	acceptCh := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept(ctx)
		acceptCh <- accepted{conn: c, err: err}
	}()

	client, err = quic.Dial(ctx, clientUDP, ln.Addr(), clientTLS, clientConf)
	if err != nil {
		t.Fatalf("quic.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseWithError(0, "") })

	a := <-acceptCh
	if a.err != nil {
		t.Fatalf("Accept: %v", a.err)
	}
	server = a.conn
	t.Cleanup(func() { _ = server.CloseWithError(0, "") })
	return client, server
}

// testServerCert returns an ECDSA P-256 self-signed certificate, the shape a real cover uses.
// The noise3 certificate is Ed25519 by spec; the pinned Chrome ClientHello does not offer an
// Ed25519 signature scheme, so it cannot be used for the ChromeParrot path.
func testServerCert(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating P-256 key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatalf("generating serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cover.example"},
		DNSNames:              []string{"cover.example"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func noise3Pair(t *testing.T) (clientTLS, serverTLS *tls.Config, conf *quic.Config) {
	t.Helper()
	clientTLS, serverTLS, err := Noise3TLSConfig()
	if err != nil {
		t.Fatalf("Noise3TLSConfig: %v", err)
	}
	conf, err = Noise3QUICConfig(48)
	if err != nil {
		t.Fatalf("Noise3QUICConfig: %v", err)
	}
	return clientTLS, serverTLS, conf
}

// TestCarrierConformance runs the shared mux.Carrier suite on the noise3 path: X25519MLKEM768,
// not sealed, native streams and DATAGRAM frames.
func TestCarrierConformanceNoise3(t *testing.T) {
	clientTLS, serverTLS, conf := noise3Pair(t)
	carriertest.Run(t, func(t *testing.T) (mux.Carrier, mux.Carrier) {
		clientConn, serverConn := dialPair(t, clientTLS, serverTLS, conf.Clone(), conf.Clone())
		return WrapNoise3(clientConn, true), WrapNoise3(serverConn, false)
	})
}

// TestCarrierConformanceChromeParrot runs the same suite on the uTLS (ChromeParrot) client with
// a classical group: every operation must work with the fork's group report, and both sides
// must agree that the path is sealed.
func TestCarrierConformanceChromeParrot(t *testing.T) {
	cert := testServerCert(t)
	clientTLS, serverTLS := Reality2TLSConfig(tls.X25519, cert)
	clientTLS.ServerName = "cover.example"
	serverConf := &quic.Config{EnableDatagrams: true}
	carriertest.Run(t, func(t *testing.T) (mux.Carrier, mux.Carrier) {
		clientConn, serverConn := dialPair(t, clientTLS, serverTLS, Reality2QUICConfig(), serverConf.Clone())
		return Wrap(clientConn, true), Wrap(serverConn, false)
	})
}

// TestSealedMatchesNegotiatedGroup is the patch #3 regression test: a uTLS client and a
// crypto/tls server must read the same CurveID from their ConnectionState, and therefore report
// the same sealed mode, for both a classical and a post-quantum group.
func TestSealedMatchesNegotiatedGroup(t *testing.T) {
	cases := []struct {
		name   string
		group  tls.CurveID
		sealed bool
	}{
		{"x25519", tls.X25519, true},
		{"x25519mlkem768", tls.X25519MLKEM768, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clientTLS, serverTLS := Reality2TLSConfig(tc.group, testServerCert(t))
			clientTLS.ServerName = "cover.example"
			clientConn, serverConn := dialPair(t, clientTLS, serverTLS,
				Reality2QUICConfig(), &quic.Config{EnableDatagrams: true})

			if got := clientConn.ConnectionState().TLS.CurveID; got != tc.group {
				t.Errorf("client CurveID = %d, want %d (patch #3)", got, tc.group)
			}
			if got := serverConn.ConnectionState().TLS.CurveID; got != tc.group {
				t.Errorf("server CurveID = %d, want %d", got, tc.group)
			}

			client, server := Wrap(clientConn, true), Wrap(serverConn, false)
			if got := client.Sealed(); got != tc.sealed {
				t.Errorf("client.Sealed() = %v, want %v", got, tc.sealed)
			}
			if got := server.Sealed(); got != tc.sealed {
				t.Errorf("server.Sealed() = %v, want %v", got, tc.sealed)
			}
			if client.ChannelBinding() != server.ChannelBinding() {
				t.Error("channel binding differs between the two sides")
			}

			// The reported sealing is not just a label: a stream round trip works in both
			// modes on the connection the two sides just described.
			ctx, cancel := context.WithTimeout(t.Context(), handshakeTimeout)
			defer cancel()
			cs, err := client.OpenStream(ctx)
			if err != nil {
				t.Fatalf("client.OpenStream: %v", err)
			}
			if _, err := cs.Write([]byte("sealed or not")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := cs.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite: %v", err)
			}
			ss, err := server.AcceptStream(ctx)
			if err != nil {
				t.Fatalf("server.AcceptStream: %v", err)
			}
			if err := ss.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
				t.Fatalf("SetReadDeadline: %v", err)
			}
			got, err := io.ReadAll(ss)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != "sealed or not" {
				t.Fatalf("read = %q", got)
			}
		})
	}
}

// TestChannelBindingLabel pins the exporter label, context and length of handshake.md §3.2.
func TestChannelBindingLabel(t *testing.T) {
	clientTLS, serverTLS, conf := noise3Pair(t)
	clientConn, serverConn := dialPair(t, clientTLS, serverTLS, conf.Clone(), conf.Clone())

	want, err := clientConn.ExportKeyingMaterial("EXPORTER-echofog/hs2/channel", nil, 32)
	if err != nil {
		t.Fatalf("ExportKeyingMaterial: %v", err)
	}
	client, server := WrapNoise3(clientConn, true), WrapNoise3(serverConn, false)
	if got := client.ChannelBinding(); !bytes.Equal(got[:], want) {
		t.Fatalf("ChannelBinding = %x, want %x", got, want)
	}
	if client.ChannelBinding() != server.ChannelBinding() {
		t.Fatal("channel binding differs between the two sides")
	}
}

// TestWrapNoise3ClosesClassicalGroup: a classical group is fatal for noise3 (wire.md §5.1,
// §8.5), and the peer must see the close too.
func TestWrapNoise3ClosesClassicalGroup(t *testing.T) {
	clientTLS, serverTLS := Reality2TLSConfig(tls.X25519, testServerCert(t))
	clientTLS.ServerName = "cover.example"
	clientConn, serverConn := dialPair(t, clientTLS, serverTLS,
		Reality2QUICConfig(), &quic.Config{EnableDatagrams: true})

	client := WrapNoise3(clientConn, true)
	waitClosed(t, client.Done(), "client carrier")
	if _, err := client.OpenStream(t.Context()); !errors.Is(err, ErrClassicalGroup) {
		t.Fatalf("OpenStream error = %v, want ErrClassicalGroup", err)
	}
	waitClosed(t, serverConn.Context().Done(), "peer connection")
}

func waitClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not close", what)
	}
}
