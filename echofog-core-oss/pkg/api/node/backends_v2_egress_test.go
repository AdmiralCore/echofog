package node

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/v2/app"
)

// egressApp reports a node that runs behind a proxy.
type egressApp struct {
	stubV2App
	st app.NetStatus
}

func (e *egressApp) NetStatus() app.NetStatus { return e.st }

// Proxy and anonymous mode on protocol 2 (T11.6a): saved for the next start, read back, and the mode in
// effect comes from the running node. Tor needs a proxy; the own-relay mode is shown but cannot be
// chosen yet, and the card says why.
func TestV2ProxyAndAnonymitySettings(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2(&egressApp{})

	ps, err := api.GetProxySettings()
	require.NoError(t, err)
	assert.Equal(t, "", ps.Proxy)
	assert.False(t, ps.Restart)

	require.Error(t, api.SetProxySettings("not a proxy", ""))
	require.Error(t, api.SetProxySettings("127.0.0.1:9050", "sideways"))
	require.Error(t, api.SetAnonymity(config.AnonTor, false), "Tor with no proxy cannot hold")

	require.NoError(t, api.SetProxySettings("socks5://127.0.0.1:9050", "relay"))
	ps, err = api.GetProxySettings()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9050", ps.Proxy)
	assert.Equal(t, "relay", ps.CallRoute)
	assert.True(t, ps.Restart, "what is saved differs from what runs")

	require.NoError(t, api.SetAnonymity(config.AnonTor, false))
	an, err := api.GetAnonymity()
	require.NoError(t, err)
	assert.Equal(t, config.AnonTor, an.Mode)
	assert.True(t, an.HasProxy)
	assert.True(t, an.Restart)

	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = api.node.Cfg.Storage.DatabasePath
	loadPersistedSettings(cfg)
	assert.Equal(t, "127.0.0.1:9050", cfg.Network.Proxy, "the next start reads it")
	assert.Equal(t, config.AnonTor, cfg.Network.Anonymous)

	// The own-relay mode is shown, not hidden, and not selectable on protocol 2 yet.
	require.False(t, an.RelayAvailable)
	require.Contains(t, an.RelayNote, "следующей версии")
	err = api.SetAnonymity(config.AnonRelay, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "следующей версии")
	require.NotErrorIs(t, err, ErrV2NotBuilt)
}

// What runs comes from the node: the proxy, the Tor flag, and calls forced through a relay.
func TestV2StatusShowsWhatRuns(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2(&egressApp{st: app.NetStatus{Running: true, Proxy: "127.0.0.1:9050", Tor: true, CallRoute: "relay"}})
	ps, err := api.GetProxySettings()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9050", ps.Running)
	assert.Equal(t, "relay", ps.RunningCallRoute)
	an, err := api.GetAnonymity()
	require.NoError(t, err)
	assert.Equal(t, config.AnonTor, an.Running)
	assert.Equal(t, "nobody", an.SeenBy)
	assert.Equal(t, "127.0.0.1:9050", an.Proxy)

	off := newFacadeNode(t, config.ProtocolV2)
	off.AttachV2(&egressApp{st: app.NetStatus{Running: true}})
	an, err = off.GetAnonymity()
	require.NoError(t, err)
	assert.Equal(t, "network", an.SeenBy)
}

// A mode that cannot hold connects nowhere: the saved Tor mode without a proxy is a startup error.
func TestV2EgressFromSettingsFailsClosed(t *testing.T) {
	cfg := config.DefaultConfig("t")
	cfg.Network.Anonymous = config.AnonTor
	_, err := EgressFromConfig(cfg)
	require.Error(t, err, "Tor without a proxy must not start")

	cfg.Network.Proxy = "127.0.0.1:9050"
	eg, err := EgressFromConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, eg)
	assert.True(t, eg.Tor)
	assert.Equal(t, "127.0.0.1:9050", eg.Proxy)

	cfg.Network.Anonymous = ""
	eg, err = EgressFromConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, eg)
	assert.False(t, eg.Tor, "a plain proxy is not Tor")

	cfg.Network.Proxy = ""
	eg, err = EgressFromConfig(cfg)
	require.NoError(t, err)
	assert.Nil(t, eg)

	cfg.Network.Anonymous = config.AnonRelay
	_, err = EgressFromConfig(cfg)
	require.Error(t, err, "the own-relay mode does not exist on protocol 2 yet: no silent fallback to open")
}

// The app's own lookups (updates, location) on protocol 2 go through the proxy as well, names
// unresolved, and not around it.
func TestV2OwnHTTPClientUsesTheProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	seen := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 16)
		n, _ := c.Read(buf)
		seen <- buf[:n]
	}()
	api := newFacadeNode(t, config.ProtocolV2)
	api.AttachV2(&egressApp{})
	api.node.Cfg.Network.Proxy = ln.Addr().String()
	cl := api.OwnHTTPClient()
	require.NotNil(t, cl)
	go func() { _, _ = cl.Get("http://lookup.example.invalid/") }()
	select {
	case hello := <-seen:
		require.NotEmpty(t, hello)
		assert.Equal(t, byte(5), hello[0], "the first bytes on the wire are a SOCKS5 greeting to the proxy")
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup did not go through the proxy")
	}
}
