package mimicry

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// generateTestCertificate generates an in-memory self-signed TLS certificate for unit testing.
func generateTestCertificate(t *testing.T) tls.Certificate {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ecdsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"EchoFog Test"},
			CommonName:   "www.microsoft.com",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"www.microsoft.com", "localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("failed to parse x509 key pair: %v", err)
	}
	return tlsCert
}

func TestTCP_TLS_MimicryHandshake_Authenticated(t *testing.T) {
	tlsCert := generateTestCertificate(t)
	serverTLSConfig := BuildServerTLSConfig(tlsCert)

	authKey := []byte("secret_auth_key_for_testing_32b")
	cfg := &MimicryConfig{
		ServerName:         "www.microsoft.com",
		Profile:            ProfileChrome,
		AuthKey:            authKey,
		InsecureSkipVerify: true,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on TCP: %v", err)
	}
	defer listener.Close()

	serverAddr := listener.Addr().String()

	// Server worker
	type accepted struct {
		conn    net.Conn
		binding []byte
	}
	serverAccepted := make(chan accepted, 1)
	go func() {
		rawConn, err := listener.Accept()
		if err != nil {
			return
		}

		conn, isPeer, binding, err := ServerHandshake(rawConn, serverTLSConfig, cfg)
		if err != nil {
			t.Errorf("server handshake error: %v", err)
			return
		}
		if !isPeer {
			t.Errorf("expected isPeer to be true for authenticated peer")
			return
		}
		serverAccepted <- accepted{conn, binding}
	}()

	// Client connection with browser mimicry
	rawClient, err := net.Dial("tcp", serverAddr)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer rawClient.Close()

	clientConn, clientBinding, err := ClientHandshake(rawClient, cfg)
	if err != nil {
		t.Fatalf("client handshake failed: %v", err)
	}
	defer clientConn.Close()

	var srv accepted
	select {
	case srv = <-serverAccepted:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for server handshake")
	}
	defer srv.conn.Close()

	// Both ends derive the same per-connection secret from the TLS session: the peer handshake signs
	// it, so a relay terminating TLS in the middle (two different sessions) cannot be spliced in.
	if len(clientBinding) != ChannelBindingLen || !bytes.Equal(clientBinding, srv.binding) {
		t.Fatalf("channel bindings differ or are empty: client %x server %x", clientBinding, srv.binding)
	}

	// data flows both ways through the camouflage
	testMsg := []byte("E2EE_SIGNAL_DOUBLE_RATCHET_OVER_TCP_TLS_MIMICRY")
	if _, err := clientConn.Write(testMsg); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	got := make([]byte, len(testMsg))
	if _, err := io.ReadFull(srv.conn, got); err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if !bytes.Equal(got, testMsg) {
		t.Fatalf("expected payload %s, got %s", testMsg, got)
	}
}

// A stranger (a censor's scanner) that connects without a valid token does a genuine TLS
// handshake with the real site, relayed through us, and sees the real site's certificate and
// content. Our own self-signed certificate is never shown.
func TestScannerSeesTheRealSiteCertificate(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("real site saw " + r.Method + " " + r.URL.Path))
	}))
	defer site.Close()
	siteLeaf := site.Certificate() // the genuine cert of the camouflage site

	ourCert := generateTestCertificate(t)
	cfg := DefaultMimicryConfig()
	cfg.AuthKey = NodeStealthKey("", [32]byte{9})
	cfg.FallbackAddr = strings.TrimPrefix(site.URL, "https://")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		conn, isPeer, _, err := ServerHandshake(raw, BuildServerTLSConfig(ourCert), cfg)
		if err != nil || isPeer {
			raw.Close()
			return
		}
		HandleActiveProbe(conn, cfg)
	}()

	start := time.Now()
	scanner, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("scanner dial failed: %v", err)
	}
	defer scanner.Close()

	// the certificate the scanner received is the real site's, not ours
	got := scanner.ConnectionState().PeerCertificates[0]
	if !bytes.Equal(got.Raw, siteLeaf.Raw) {
		t.Fatal("the scanner did not get the real site's certificate")
	}
	if bytes.Equal(got.Raw, ourCert.Leaf.Raw) {
		t.Fatal("the scanner saw our own certificate")
	}

	scanner.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := scanner.Write([]byte("GET /probe HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(scanner), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "real site saw GET /probe") {
		t.Fatalf("the request did not reach the real site intact: %q", body)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("handling the scanner took %v: a delay would single our port out", time.Since(start))
	}
}

func TestStealthTokenTimeAndReplay(t *testing.T) {
	aead, err := DeriveAEAD(NodeStealthKey("", [32]byte{7}))
	if err != nil {
		t.Fatal(err)
	}
	cache := newReplayCache()
	now := time.Now().Unix()
	tok, err := GenerateStealthToken(aead)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyStealthToken(aead, tok, cache, now); err != nil {
		t.Fatalf("a fresh token was refused: %v", err)
	}
	if err := verifyStealthToken(aead, tok, cache, now); err != ErrReplayDetected {
		t.Fatalf("a replayed token was accepted: %v", err)
	}
	// another node's key does not open it
	other, _ := DeriveAEAD(NodeStealthKey("", [32]byte{8}))
	if err := verifyStealthToken(other, tok, newReplayCache(), now); err != ErrInvalidStealthToken {
		t.Fatalf("a token verified under another node's key: %v", err)
	}
	// outside the clock window
	fresh, _ := GenerateStealthToken(aead)
	if err := verifyStealthToken(aead, fresh, newReplayCache(), now+ReplayWindowSeconds+5); err != ErrReplayDetected {
		t.Fatalf("an expired token was accepted: %v", err)
	}
	// a tampered byte breaks the tag
	fresh2, _ := GenerateStealthToken(aead)
	fresh2[len(fresh2)-1] ^= 1
	if err := verifyStealthToken(aead, fresh2, newReplayCache(), now); err != ErrInvalidStealthToken {
		t.Fatalf("a tampered token was accepted: %v", err)
	}
}

// The token travels in the ClientHello session id, where a TLS 1.3 compatibility session id
// normally sits, and it is extracted from the raw record byte for byte.
func TestTokenCarriedInClientHelloSessionID(t *testing.T) {
	cfg := DefaultMimicryConfig()
	cfg.AuthKey = NodeStealthKey("", [32]byte{3})

	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	go func() { _, _, _ = ClientHandshake(cli, cfg) }()

	rec, ok := readClientHelloRecord(srv)
	if !ok {
		t.Fatal("did not read a ClientHello record")
	}
	sid, has := sessionIDFromClientHello(rec)
	if !has || len(sid) != StealthTokenLen {
		t.Fatalf("session id not found or wrong length: %d", len(sid))
	}
	aead, _ := DeriveAEAD(cfg.AuthKey)
	if err := verifyStealthToken(aead, sid, newReplayCache(), time.Now().Unix()); err != nil {
		t.Fatalf("the session id was not a valid token: %v", err)
	}
}
