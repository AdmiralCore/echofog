package mimicry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startSite runs a local TLS site that completes handshakes with serverTLS, standing in for a
// neighbour the scanner probes. It returns its address.
func startSite(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				time.Sleep(300 * time.Millisecond)
				_ = c.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

// trust makes the scanner accept cert (self-signed) as if a public CA had issued it.
func trust(t *testing.T, cert tls.Certificate) {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	prev := neighborRoots
	neighborRoots = x509.NewCertPool()
	neighborRoots.AddCert(leaf)
	t.Cleanup(func() { neighborRoots = prev })
}

func ourTestProfile(t *testing.T, cert tls.Certificate) ServerProfile {
	t.Helper()
	p, err := OwnServerProfile(BuildServerTLSConfig(cert))
	require.NoError(t, err)
	return p
}

func TestProbeNeighborAcceptsAMatchingSite(t *testing.T) {
	cert, err := GenerateCamouflageCert("neighbour-site.com")
	require.NoError(t, err)
	serverTLS := BuildServerTLSConfig(cert)
	trust(t, cert)
	addr := startSite(t, serverTLS)
	ours := ourTestProfile(t, cert)

	nb, ok := probeNeighbor(context.Background(), addr, ours)
	require.True(t, ok, "a TLS 1.3 site that answers like us must be accepted")
	assert.Equal(t, "neighbour-site.com", nb.ServerName, "the SNI is taken from the site's certificate")
	assert.Equal(t, addr, nb.Addr)
}

func TestProbeNeighborRejectsMismatchedServerHello(t *testing.T) {
	cert, err := GenerateCamouflageCert("neighbour-site.com")
	require.NoError(t, err)
	ours := ourTestProfile(t, cert) // ours: AES-128-GCM, X25519MLKEM768

	// A site restricted to a different cipher and curve: a passive observer would see a different
	// ServerHello, so it must be rejected even though it is a real TLS 1.3 site.
	odd := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		MaxVersion:       tls.VersionTLS13,
		CipherSuites:     []uint16{tls.TLS_CHACHA20_POLY1305_SHA256},
		CurvePreferences: []tls.CurveID{tls.CurveP256},
		NextProtos:       []string{"h2", "http/1.1"},
	}
	addr := startSite(t, odd)

	_, ok := probeNeighbor(context.Background(), addr, ours)
	assert.False(t, ok, "a site whose ServerHello differs from ours must not be adopted")
}

func TestVerifyNeighbor(t *testing.T) {
	cert, err := GenerateCamouflageCert("neighbour-site.com")
	require.NoError(t, err)
	trust(t, cert)
	addr := startSite(t, BuildServerTLSConfig(cert))
	ours := ourTestProfile(t, cert)

	assert.True(t, VerifyNeighbor(context.Background(), Neighbor{ServerName: "neighbour-site.com", Addr: addr}, ours))
	assert.False(t, VerifyNeighbor(context.Background(), Neighbor{ServerName: "someone.else", Addr: addr}, ours),
		"a neighbour whose certificate no longer names the expected domain is not verified")
	assert.False(t, VerifyNeighbor(context.Background(), Neighbor{ServerName: "x", Addr: "127.0.0.1:1"}, ours),
		"a dead neighbour is not verified")
}

func TestNearbyCandidatesStayInSubnetAndExcludeSelf(t *testing.T) {
	own := netip.MustParseAddr("203.0.113.40")
	cands := nearbyCandidates(own, 20)
	require.NotEmpty(t, cands)
	seen := map[netip.Addr]bool{}
	for _, c := range cands {
		b := c.As4()
		assert.Equal(t, [3]byte{203, 0, 113}, [3]byte{b[0], b[1], b[2]}, "candidate %s left the /24", c)
		assert.NotEqual(t, own, c, "the node must not probe its own address")
		assert.GreaterOrEqual(t, int(b[3]), 1)
		assert.LessOrEqual(t, int(b[3]), 254)
		assert.False(t, seen[c], "duplicate candidate %s", c)
		seen[c] = true
	}
	// ±20 around .40 is .20..60 minus .40 = 40 addresses, all in range
	assert.Len(t, cands, 40)
}

func TestNearbyCandidatesClampNearEdges(t *testing.T) {
	cands := nearbyCandidates(netip.MustParseAddr("203.0.113.250"), 20)
	for _, c := range cands {
		assert.LessOrEqual(t, int(c.As4()[3]), 254, "no .255 broadcast, no overflow")
	}
	assert.NotEmpty(t, cands)
}

func TestScanSkipsPrivateAddresses(t *testing.T) {
	for _, s := range []string{"10.0.0.5", "192.168.1.9", "172.16.3.4", "100.64.1.1", "127.0.0.1", "169.254.1.1"} {
		ip := netip.MustParseAddr(s)
		_, ok := ScanForNeighbor(context.Background(), ip, ServerProfile{}, nil)
		assert.Falsef(t, ok, "scanning the neighbours of the non-public address %s is pointless", s)
	}
}

// A name is taken only when it is a public domain the certificate is trusted for: a plain name
// before a wildcard (which becomes www.), never an internal or IP name, never an untrusted chain.
func TestPickServerName(t *testing.T) {
	mk := func(names ...string) (tls.ConnectionState, tls.Certificate) {
		cert, err := GenerateCamouflageCert(names[0])
		require.NoError(t, err)
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		require.NoError(t, err)
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, cert
	}
	now := time.Now()
	cs, cert := mk("real-site.com")
	assert.Equal(t, "", pickServerName(cs, now), "a self-signed certificate is no public site")
	trust(t, cert)
	assert.Equal(t, "real-site.com", pickServerName(cs, now))

	for _, bad := range []string{"kubernetes", "kubernetes.default.svc", "34.12.240.83", "db.cluster.local", "host.internal"} {
		cs, cert := mk(bad)
		trust(t, cert)
		assert.Equalf(t, "", pickServerName(cs, now), "%q is not a site a browser visits", bad)
	}
	assert.Equal(t, "", pickServerName(tls.ConnectionState{}, now))
}

func TestUsableServerName(t *testing.T) {
	for _, ok := range []string{"aifraud.ru", "www.example-shop.de", "a.b.co.uk", "xn--80ak6aa92e.xn--p1ai"} {
		assert.Truef(t, usableServerName(ok), "%s", ok)
	}
	for _, bad := range []string{"kubernetes", "kubernetes.default.svc", "10.0.0.1", "34.12.240.83", "x.local", "-a.com", "a..com", "site.test", "my_host.com"} {
		assert.Falsef(t, usableServerName(bad), "%s", bad)
	}
}

func TestWideCandidatesCoverTheBlockOwnFirst(t *testing.T) {
	own := netip.MustParseAddr("34.158.189.116")
	cands := WideCandidates(own)
	assert.Len(t, cands, 253+4*254)
	seen := map[netip.Addr]bool{}
	for i, c := range cands {
		b := c.As4()
		assert.NotEqual(t, own, c)
		assert.False(t, seen[c], "duplicate %s", c)
		seen[c] = true
		if i < 253 {
			assert.Equal(t, byte(189), b[2], "the own /24 comes first")
		} else {
			assert.Contains(t, []byte{187, 188, 190, 191}, b[2])
		}
	}
}
