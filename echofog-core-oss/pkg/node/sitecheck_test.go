package node_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestNode(t *testing.T, name string) *node.Node {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	n := node.NewNode(context.Background(), cfg, name)
	_, err := n.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	return n
}

// A site is checked with a real TLS handshake, directly and through an exit: it opens when the
// certificate verifies, and a closed port or a certificate nobody vouches for count as blocked.
func TestCheckSitesDirectAndThroughAnExit(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer site.Close()
	roots := x509.NewCertPool()
	roots.AddCert(site.Certificate())
	node.SetSiteCheckRoots(roots)
	defer node.SetSiteCheckRoots(nil)

	// a site whose certificate nobody vouches for: what an intercepting box looks like
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	defer other.Close()

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddr := closed.Addr().String()
	closed.Close()

	client := startTestNode(t, "client")
	exit := startTestNode(t, "exit")
	exit.ExitService.AllowPrivateTargets(true)
	exit.ExitService.SetPolicy(exit_gateway.Policy{})
	exit.ExitService.SetEnabled(true)
	client.Swarm.Bootstrap([]string{network.JoinPeerAddr(exit.Transport.Addr().String(), exit.ID)})
	require.Eventually(t, func() bool { return client.Swarm.GetPeer(exit.ID) != nil }, 5*time.Second, 50*time.Millisecond)

	good := "https://" + site.Listener.Addr().String() + "/watch?v=1"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := client.CheckSites(ctx, &exit.ID, []string{good, other.Listener.Addr().String(), closedAddr})
	require.NoError(t, err)
	require.Len(t, res, 3)

	assert.Equal(t, site.Listener.Addr().String(), res[0].Host, "the link is reduced to host:port")
	assert.True(t, res[0].Direct.OK, "direct: %s", res[0].Direct.Reason)
	require.NotNil(t, res[0].Exit)
	assert.True(t, res[0].Exit.OK, "through the exit: %s", res[0].Exit.Reason)

	assert.False(t, res[1].Direct.OK)
	assert.Equal(t, "wrong certificate (intercepted)", res[1].Direct.Reason)
	assert.False(t, res[1].Exit.OK)
	assert.Equal(t, "wrong certificate (intercepted)", res[1].Exit.Reason)

	assert.False(t, res[2].Direct.OK)
	assert.Equal(t, "refused", res[2].Direct.Reason)
	assert.False(t, res[2].Exit.OK)
	assert.Equal(t, "the exit could not reach it", res[2].Exit.Reason)

	// without an exit only the direct check runs
	res, err = client.CheckSites(ctx, nil, []string{good})
	require.NoError(t, err)
	assert.True(t, res[0].Direct.OK)
	assert.Nil(t, res[0].Exit)

	_, err = client.CheckSites(ctx, nil, []string{"bad host name"})
	assert.Error(t, err)
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "interceptor"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
