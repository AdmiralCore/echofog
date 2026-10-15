package node

import (
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicListenerRefusesToStartWithoutAMimicryTarget(t *testing.T) {
	base := func() *config.AppConfig {
		c := config.DefaultConfig("")
		c.Network.TCPListenAddr = "" // default: pure client
		c.Network.WSListenAddr = ""
		return c
	}

	t.Run("pure client needs no domain", func(t *testing.T) {
		require.NoError(t, publicListenerConfigError(base()))
	})

	t.Run("client with tcp fallback off needs no domain", func(t *testing.T) {
		c := base()
		c.Network.TCPListenAddr = "off"
		require.NoError(t, publicListenerConfigError(c))
	})

	t.Run("explicit TCP listener without a domain is refused", func(t *testing.T) {
		c := base()
		c.Network.TCPListenAddr = ":443"
		err := publicListenerConfigError(c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tls_server_name")
		assert.Contains(t, err.Error(), "camouflage_addr")
	})

	t.Run("explicit WS listener without a domain is refused", func(t *testing.T) {
		c := base()
		c.Network.WSListenAddr = ":8443"
		err := publicListenerConfigError(c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "WebSocket")
	})

	t.Run("public listener with both fields set starts", func(t *testing.T) {
		c := base()
		c.Network.TCPListenAddr = ":443"
		c.AntiDPI.TLSServerName = "neighbour.example.de"
		c.AntiDPI.CamouflageAddr = "203.0.113.7:443"
		require.NoError(t, publicListenerConfigError(c))
	})

	t.Run("a domain without a camouflage target is still refused", func(t *testing.T) {
		c := base()
		c.Network.TCPListenAddr = ":443"
		c.AntiDPI.TLSServerName = "neighbour.example.de"
		require.Error(t, publicListenerConfigError(c), "both the SNI and the forward target are required")
	})
}

func TestIsPublicNode(t *testing.T) {
	c := config.DefaultConfig("")
	assert.False(t, isPublicNode(c), "a default client exposes no public listener")
	c.Network.TCPListenAddr = "off"
	assert.False(t, isPublicNode(c), "tcp fallback off is not a public listener")
	c.Network.TCPListenAddr = ":443"
	assert.True(t, isPublicNode(c))
	c.Network.TCPListenAddr = ""
	c.Network.WSListenAddr = ":8443"
	assert.True(t, isPublicNode(c))

	// behind a CDN or reverse proxy the listener sits on loopback: the world sees the CDN's IP and
	// the operator's domain, so no mimicry target is needed
	c.Network.WSListenAddr = "127.0.0.1:8080"
	assert.False(t, isPublicNode(c), "a WS listener on loopback (CDN in front) is not public")
	c.Network.WSListenAddr = "[::1]:8080"
	assert.False(t, isPublicNode(c))
	c.Network.WSListenAddr = ""
	c.Network.TCPListenAddr = "localhost:443"
	assert.False(t, isPublicNode(c))
	c.Network.TCPListenAddr = "0.0.0.0:443"
	assert.True(t, isPublicNode(c), "every interface is public")
	require.NoError(t, publicListenerConfigError(func() *config.AppConfig {
		x := config.DefaultConfig("")
		x.Network.WSListenAddr = "127.0.0.1:8080"
		return x
	}()), "a CDN-fronted WS node starts without a camouflage target")
}

func TestNeighborCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile", "anti_dpi_neighbour.json")

	_, ok := loadNeighborCache(path)
	assert.False(t, ok, "no cache file yet")

	nb := mimicry.Neighbor{ServerName: "real.neighbour.example", Addr: "203.0.113.9:443"}
	require.NoError(t, saveNeighborCache(path, nb))

	got, ok := loadNeighborCache(path)
	require.True(t, ok)
	assert.Equal(t, nb, got)
}

func TestNeighborCacheRejectsIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anti_dpi_neighbour.json")
	require.NoError(t, saveNeighborCache(path, mimicry.Neighbor{ServerName: "x", Addr: ""}))
	_, ok := loadNeighborCache(path)
	assert.False(t, ok, "a cache entry missing the forward address must be ignored, not used blindly")
}
