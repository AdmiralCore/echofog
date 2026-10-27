package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultsAreSafeForARealNetwork(t *testing.T) {
	cfg := DefaultConfig("")

	assert.Equal(t, CurrentProtocolVersion, cfg.Network.ProtocolVersion)
	assert.True(t, cfg.Network.PortMapping, "reachability features are on by default")
	assert.True(t, cfg.Network.HolePunch)
	assert.Equal(t, "testnet", cfg.Ton.Network, "testers never pay real money until the vault contract is audited; a release build sets DefaultTonNetwork=mainnet")
	assert.Empty(t, cfg.AntiDPI.TLSServerName, "no SNI is baked in: a shared default on a residential/VPS IP is itself an SNI/IP-mismatch fingerprint; a public node sets its own")

	for _, addr := range cfg.Network.BootstrapNodes {
		assert.NotContains(t, addr, "127.0.0.1", "a release must not default to a local bootstrap node")
	}
	assert.Zero(t, cfg.Relay.Server.PricePerMB, "relays are free: nothing settles a relay price")
	assert.Empty(t, cfg.Payments.SupportAddress, "no project address is baked in: the feature stays off until configured")

	assert.Equal(t, 3, cfg.AntiDPI.FlowShaping.MaxConnections)
	assert.Equal(t, 8, cfg.AntiDPI.FlowShaping.MaxConcurrency)
	assert.Equal(t, int64(15*1024*1024), cfg.AntiDPI.FlowShaping.MaxBytesMin)
	assert.Equal(t, int64(45*1024*1024), cfg.AntiDPI.FlowShaping.MaxBytesMax)
	assert.Equal(t, 500, cfg.AntiDPI.FlowShaping.MaxStreamsPerConn)

	assert.False(t, cfg.AntiDPI.RegionAwareObfs, "region-aware obfs is opt-in: 'same country' is not safe where domestic traffic is censored too")
	assert.Empty(t, cfg.AntiDPI.HomeCountry, "the home country is placed by GeoIP unless an operator pins it")
}

func TestEachCallGetsItsOwnBootstrapSlice(t *testing.T) {
	old := DefaultBootstrapNodes
	DefaultBootstrapNodes = []string{"203.0.113.1:4242"}
	defer func() { DefaultBootstrapNodes = old }()

	a, b := DefaultConfig(""), DefaultConfig("")
	a.Network.BootstrapNodes[0] = "changed"
	assert.Equal(t, "203.0.113.1:4242", b.Network.BootstrapNodes[0], "editing one config must not edit the defaults")
	assert.Equal(t, "203.0.113.1:4242", DefaultBootstrapNodes[0])
}

func TestProfilePathsAreSeparate(t *testing.T) {
	a, b := DefaultConfig("alice"), DefaultConfig("bob")
	assert.NotEqual(t, a.Storage.DatabasePath, b.Storage.DatabasePath)
	assert.Equal(t, "db", filepath.Base(a.Storage.DatabasePath))
	assert.Equal(t, "alice", filepath.Base(filepath.Dir(a.Storage.DatabasePath)))
}

func TestAvailableProfilesListsDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	none, err := GetAvailableProfiles()
	require.NoError(t, err)
	assert.Empty(t, none)

	dir := filepath.Join(home, ".echofog", "profiles")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "alice"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bob"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray-file"), []byte("x"), 0o600))

	got, err := GetAvailableProfiles()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alice", "bob"}, got, "files are not profiles")
}
