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

func ourTestProfile(t *testing.T, cert tls.Certificate) ServerProfile {
	t.Helper()
	p, err := OwnServerProfile(BuildServerTLSConfig(cert))
	require.NoError(t, err)
	return p
}

func TestProbeNeighborAcceptsAMatchingSite(t *testing.T) {
	cert, err := GenerateCamouflageCert("neighbour.example")
	require.NoError(t, err)
	serverTLS := BuildServerTLSConfig(cert)
	addr := startSite(t, serverTLS)
	ours := ourTestProfile(t, cert)

	nb, ok := probeNeighbor(context.Background(), addr, ours)
	require.True(t, ok, "a TLS 1.3 site that answers like us must be accepted")
	assert.Equal(t, "neighbour.example", nb.ServerName, "the SNI is taken from the site's certificate")
	assert.Equal(t, addr, nb.Addr)
}

func TestProbeNeighborRejectsMismatchedServerHello(t *testing.T) {
	cert, err := GenerateCamouflageCert("neighbour.example")
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
	cert, err := GenerateCamouflageCert("neighbour.example")
	require.NoError(t, err)
	addr := startSite(t, BuildServerTLSConfig(cert))
	ours := ourTestProfile(t, cert)

	assert.True(t, VerifyNeighbor(context.Background(), Neighbor{ServerName: "neighbour.example", Addr: addr}, ours))
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

func TestPickServerNamePrefersConcreteThenWildcard(t *testing.T) {
	mk := func(names ...string) tls.ConnectionState {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{{DNSNames: names}}}
	}
	assert.Equal(t, "real.example", pickServerName(mk("real.example")))
	assert.Equal(t, "www.wild.example", pickServerName(mk("*.wild.example")), "a wildcard becomes a concrete www host")
	assert.Equal(t, "concrete.example", pickServerName(mk("*.wild.example", "concrete.example")), "a concrete name wins over a wildcard")
	assert.Equal(t, "", pickServerName(mk()))
}
