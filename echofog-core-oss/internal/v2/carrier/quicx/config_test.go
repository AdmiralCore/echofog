package quicx

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"sync"
	"testing"
	"time"
)

// serverCertificate returns the certificate the server config selects for a ClientHello that
// offers schemes.
func serverCertificate(t *testing.T, cfg *tls.Config, schemes ...tls.SignatureScheme) *x509.Certificate {
	t.Helper()
	if cfg.GetCertificate == nil {
		t.Fatal("server config has no GetCertificate callback")
	}
	cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{SignatureSchemes: schemes})
	if err != nil {
		t.Fatalf("GetCertificate(%v): %v", schemes, err)
	}
	if cert == nil || cert.Leaf == nil {
		t.Fatal("GetCertificate returned no parsed certificate")
	}
	return cert.Leaf
}

func TestNoise3TLSConfig(t *testing.T) {
	client, server, err := Noise3TLSConfig()
	if err != nil {
		t.Fatalf("Noise3TLSConfig: %v", err)
	}
	for name, cfg := range map[string]*tls.Config{"client": client, "server": server} {
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("%s MinVersion = %x, want TLS 1.3", name, cfg.MinVersion)
		}
		if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "h3" {
			t.Errorf("%s NextProtos = %v, want [h3]", name, cfg.NextProtos)
		}
		if len(cfg.CurvePreferences) != 1 || cfg.CurvePreferences[0] != tls.X25519MLKEM768 {
			t.Errorf("%s CurvePreferences = %v, want [X25519MLKEM768]", name, cfg.CurvePreferences)
		}
	}
	if !client.InsecureSkipVerify {
		t.Error("client does not skip certificate verification (wire.md §5.1: the Noise handshake authenticates)")
	}

	leaf := serverCertificate(t, server, tls.ECDSAWithP256AndSHA256)
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("default certificate key is %T, want ECDSA P-256 (the pinned Chrome hello has no Ed25519)", leaf.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("ECDSA curve = %v, want P-256", pub.Curve)
	}
	if len(leaf.Subject.Names) != 0 || leaf.Subject.CommonName != "" {
		t.Errorf("subject = %v, want empty", leaf.Subject)
	}
	if leaf.SerialNumber.Sign() == 0 {
		t.Error("serial number is zero")
	}

	// "random serial" (wire.md §5.1): a fresh config does not reuse it.
	_, server2, err := Noise3TLSConfig()
	if err != nil {
		t.Fatalf("second Noise3TLSConfig: %v", err)
	}
	if leaf.SerialNumber.Cmp(serverCertificate(t, server2, tls.ECDSAWithP256AndSHA256).SerialNumber) == 0 {
		t.Error("two certificates share a serial number")
	}
}

// TestNoise3CertificateSelection: Ed25519 is generated only for a profile that offers it, and a
// certificate is served only to a ClientHello that can verify it.
func TestNoise3CertificateSelection(t *testing.T) {
	// A profile without Ed25519 keeps the ECDSA default and never has an Ed25519 key.
	_, plain, err := Noise3TLSConfigForProfile([]tls.SignatureScheme{
		tls.ECDSAWithP256AndSHA256, tls.PSSWithSHA256,
	})
	if err != nil {
		t.Fatalf("Noise3TLSConfigForProfile without ed25519: %v", err)
	}
	if leaf := serverCertificate(t, plain, tls.ECDSAWithP256AndSHA256); leaf.PublicKey == nil {
		t.Fatal("no certificate for an ECDSA client")
	} else if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("certificate without an Ed25519 offer is %T, want ECDSA", leaf.PublicKey)
	}

	// A profile with Ed25519 prefers it when the client offers it and falls back to ECDSA when
	// the client does not (the pinned Chrome ClientHello).
	_, withEd, err := Noise3TLSConfigForProfile([]tls.SignatureScheme{
		tls.Ed25519, tls.ECDSAWithP256AndSHA256,
	})
	if err != nil {
		t.Fatalf("Noise3TLSConfigForProfile with ed25519: %v", err)
	}
	if leaf := serverCertificate(t, withEd, tls.Ed25519); leaf.PublicKey == nil {
		t.Fatal("no certificate for an Ed25519 client")
	} else if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		t.Fatalf("certificate for an Ed25519 client is %T, want ed25519", leaf.PublicKey)
	}
	if leaf := serverCertificate(t, withEd, tls.ECDSAWithP256AndSHA256); leaf.PublicKey == nil {
		t.Fatal("no fallback certificate for a client without Ed25519")
	} else if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("Ed25519 was chosen for a hello without it: %T", leaf.PublicKey)
	}
	if _, err := withEd.GetCertificate(&tls.ClientHelloInfo{
		SignatureSchemes: []tls.SignatureScheme{tls.PSSWithSHA256},
	}); err == nil {
		t.Error("a certificate was served for a ClientHello with no matching scheme")
	}
}

// TestNoise3Ed25519Handshake: a client that offers Ed25519 really gets an Ed25519 certificate.
func TestNoise3Ed25519Handshake(t *testing.T) {
	clientTLS, serverTLS, err := Noise3TLSConfigForProfile([]tls.SignatureScheme{
		tls.Ed25519, tls.ECDSAWithP256AndSHA256,
	})
	if err != nil {
		t.Fatalf("Noise3TLSConfigForProfile: %v", err)
	}
	conf, err := Noise3QUICConfig(48)
	if err != nil {
		t.Fatalf("Noise3QUICConfig: %v", err)
	}
	clientConn, _ := dialPair(t, clientTLS, serverTLS, conf.Clone(), conf.Clone())
	peers := clientConn.ConnectionState().TLS.PeerCertificates
	if len(peers) == 0 {
		t.Fatal("the client received no certificate")
	}
	if _, ok := peers[0].PublicKey.(ed25519.PublicKey); !ok {
		t.Fatalf("certificate key is %T, want ed25519 (the client offered it)", peers[0].PublicKey)
	}
}

// TestNoise3ChromeHelloHandshake: the pinned Chrome ClientHello (no Ed25519 in
// signature_algorithms) completes a noise3 handshake, and the certificate served to it uses a
// key type from its own signature_algorithms. This is the regression test for Ed25519 not being
// chosen for a hello without it.
func TestNoise3ChromeHelloHandshake(t *testing.T) {
	clientTLS, serverTLS, err := Noise3TLSConfigForProfile([]tls.SignatureScheme{
		tls.Ed25519, tls.ECDSAWithP256AndSHA256,
	})
	if err != nil {
		t.Fatalf("Noise3TLSConfigForProfile: %v", err)
	}
	var mu sync.Mutex
	var seen []tls.SignatureScheme
	serverTLS.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		seen = append([]tls.SignatureScheme(nil), chi.SignatureSchemes...)
		mu.Unlock()
		return serverTLS, nil
	}

	conf, err := Noise3QUICConfig(48)
	if err != nil {
		t.Fatalf("Noise3QUICConfig: %v", err)
	}
	clientConn, _ := dialPair(t, clientTLS, serverTLS, Reality2QUICConfig(), conf.Clone())

	mu.Lock()
	offered := append([]tls.SignatureScheme(nil), seen...)
	mu.Unlock()
	if len(offered) == 0 {
		t.Fatal("the server saw no signature_algorithms")
	}
	peers := clientConn.ConnectionState().TLS.PeerCertificates
	if len(peers) == 0 {
		t.Fatal("the client received no certificate")
	}
	if _, ok := peers[0].PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("certificate key is %T, want ECDSA P-256", peers[0].PublicKey)
	}
	if !hasSignatureScheme(offered, tls.ECDSAWithP256AndSHA256) {
		t.Fatalf("the served key type is not in the hello's signature_algorithms %v", offered)
	}
	if hasSignatureScheme(offered, tls.Ed25519) {
		t.Fatalf("the pinned Chrome hello unexpectedly offers Ed25519: %v", offered)
	}
}

func TestNoise3QUICConfig(t *testing.T) {
	const pad = 48
	cfg, err := Noise3QUICConfig(pad)
	if err != nil {
		t.Fatalf("Noise3QUICConfig: %v", err)
	}
	inner := PathMTU - Noise3DataOverhead - pad

	if cfg.MaxIdleTimeout != 60*time.Second {
		t.Errorf("MaxIdleTimeout = %v, want 60s", cfg.MaxIdleTimeout)
	}
	if cfg.KeepAlivePeriod != 0 {
		t.Errorf("KeepAlivePeriod = %v, want 0 (L2 PING keeps the path alive)", cfg.KeepAlivePeriod)
	}
	if !cfg.EnableDatagrams {
		t.Error("datagrams are disabled")
	}
	if cfg.Allow0RTT {
		t.Error("0-RTT is allowed")
	}
	if cfg.MaxIncomingStreams != 256 {
		t.Errorf("MaxIncomingStreams = %d, want 256", cfg.MaxIncomingStreams)
	}
	if cfg.MaxIncomingUniStreams != -1 {
		t.Errorf("MaxIncomingUniStreams = %d, want -1 (no unidirectional streams)", cfg.MaxIncomingUniStreams)
	}
	if cfg.InitialPacketSize != uint16(inner) {
		t.Errorf("InitialPacketSize = %d, want %d", cfg.InitialPacketSize, inner)
	}
	if cfg.MaxUDPPayloadSize != uint16(inner) {
		t.Errorf("MaxUDPPayloadSize = %d, want %d", cfg.MaxUDPPayloadSize, inner)
	}
	if !cfg.DisablePathMTUDiscovery {
		t.Error("path MTU discovery is enabled: it would grow packets past the L0 budget")
	}
	if cfg.MaxDatagramFrameSize != int64(inner) {
		t.Errorf("MaxDatagramFrameSize = %d, want %d", cfg.MaxDatagramFrameSize, inner)
	}
}

func TestNoise3QUICConfigBounds(t *testing.T) {
	if _, err := Noise3QUICConfig(PathMTU - Noise3DataOverhead - 1200); err != nil {
		t.Errorf("a profile leaving exactly 1200 bytes: %v", err)
	}
	if _, err := Noise3QUICConfig(PathMTU - Noise3DataOverhead - 1200 + 1); err == nil {
		t.Error("a profile leaving less than a QUIC Initial was accepted")
	}
	if _, err := Noise3QUICConfig(-1); err == nil {
		t.Error("a negative padding budget was accepted")
	}
}

func TestReality2TLSConfig(t *testing.T) {
	cert := testServerCert(t)
	for _, group := range []tls.CurveID{tls.X25519, tls.X25519MLKEM768} {
		client, server := Reality2TLSConfig(group, cert)
		for name, cfg := range map[string]*tls.Config{"client": client, "server": server} {
			if cfg.MinVersion != tls.VersionTLS13 {
				t.Errorf("group %d: %s MinVersion = %x, want TLS 1.3", group, name, cfg.MinVersion)
			}
			if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "h3" {
				t.Errorf("group %d: %s NextProtos = %v, want [h3]", group, name, cfg.NextProtos)
			}
			if len(cfg.CurvePreferences) != 1 || cfg.CurvePreferences[0] != group {
				t.Errorf("group %d: %s CurvePreferences = %v", group, name, cfg.CurvePreferences)
			}
		}
		if !client.InsecureSkipVerify {
			t.Errorf("group %d: client verifies the cover's certificate", group)
		}
		if len(server.Certificates) != 1 {
			t.Errorf("group %d: server has %d certificates, want 1", group, len(server.Certificates))
		}
	}
}

func TestReality2TLSConfigWithoutGroup(t *testing.T) {
	client, server := Reality2TLSConfig(0, nil)
	if len(client.CurvePreferences) != 0 || len(server.CurvePreferences) != 0 {
		t.Error("group 0 restricted the curve preferences")
	}
	if len(server.Certificates) != 0 {
		t.Error("a nil server certificate was installed")
	}
}

func TestReality2QUICConfig(t *testing.T) {
	cfg := Reality2QUICConfig()
	if !cfg.ChromeParrot {
		t.Error("ChromeParrot is off: the cover mirrors Chrome")
	}
	if !cfg.EnableDatagrams {
		t.Error("datagrams are disabled")
	}
}
