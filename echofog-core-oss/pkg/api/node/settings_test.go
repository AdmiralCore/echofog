package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettings_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")

	api := &NodeApi{node: &node.Node{Cfg: cfg}}

	cur, err := api.GetSettings()
	require.NoError(t, err)

	want := cur
	want.DirectSocket = !cur.DirectSocket
	want.SocketBufferMB = 48
	require.NoError(t, api.SetSettings(want))

	_, err = os.Stat(filepath.Join(dir, settingsFileName))
	require.NoError(t, err)

	fresh := config.DefaultConfig("t")
	fresh.Storage.DatabasePath = cfg.Storage.DatabasePath
	loadPersistedSettings(fresh)
	assert.Equal(t, want, settingsFromConfig(fresh))
	assert.Equal(t, want.SocketBufferMB, cfg.Network.SocketBufferMB)
}

func TestSettings_RejectsInvalid(t *testing.T) {
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	api := &NodeApi{node: &node.Node{Cfg: cfg}}

	cur, _ := api.GetSettings()
	cur.SocketBufferMB = 0
	assert.Error(t, api.SetSettings(cur))
	cur.SocketBufferMB = 9999
	assert.Error(t, api.SetSettings(cur))

	empty := &NodeApi{}
	_, err := empty.GetSettings()
	assert.Error(t, err)
}

// Guard mode is saved in the settings file, applied to the config live, read back, reloaded on the
// next start, and only auto/on/off are accepted.
func TestGuardMode_PersistAndApply(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")
	api := &NodeApi{node: &node.Node{Cfg: cfg}}

	st, err := api.GetGuardStatus()
	require.NoError(t, err)
	assert.Equal(t, config.GuardAuto, st.Mode, "auto by default")

	require.NoError(t, api.SetGuardMode(config.GuardOff))
	assert.Equal(t, config.GuardOff, cfg.AntiDPI.GuardMode, "applied to the live config")
	st, _ = api.GetGuardStatus()
	assert.Equal(t, config.GuardOff, st.Mode)

	assert.Error(t, api.SetGuardMode("sometimes"))

	fresh := config.DefaultConfig("t")
	fresh.Storage.DatabasePath = cfg.Storage.DatabasePath
	loadPersistedSettings(fresh)
	assert.Equal(t, config.GuardOff, fresh.AntiDPI.GuardMode, "reloaded on the next start")

	empty := &NodeApi{}
	_, err = empty.GetGuardStatus()
	assert.Error(t, err)
	assert.Error(t, empty.SetGuardMode(config.GuardOn))
}

func TestPinnedGuards_PersistAndApply(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")
	api := &NodeApi{node: &node.Node{Cfg: cfg}}

	const pid = "5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324e"
	entry := "198.51.100.9:4001/" + pid + "?sni=www.cloudflare.com&srv=1"

	// a relay without a cover site (sni) is refused: the node could not reach it in disguise
	assert.Error(t, api.PinGuard("198.51.100.9:4001/"+pid))

	require.NoError(t, api.PinGuard(entry))
	require.Len(t, cfg.AntiDPI.PinnedGuards, 1, "applied to the live config")

	st, err := api.GetGuardStatus()
	require.NoError(t, err)
	require.Len(t, st.Pinned, 1)
	assert.Equal(t, pid, st.Pinned[0].PeerID)

	// the same relay again does not double it
	require.NoError(t, api.PinGuard(entry))
	assert.Len(t, cfg.AntiDPI.PinnedGuards, 1)

	// reloaded on the next start
	fresh := config.DefaultConfig("t")
	fresh.Storage.DatabasePath = cfg.Storage.DatabasePath
	loadPersistedSettings(fresh)
	require.Len(t, fresh.AntiDPI.PinnedGuards, 1, "reloaded on the next start")

	// unpin, and it stays gone across a restart
	require.NoError(t, api.UnpinGuard(entry))
	assert.Empty(t, cfg.AntiDPI.PinnedGuards)
	fresh2 := config.DefaultConfig("t")
	fresh2.Storage.DatabasePath = cfg.Storage.DatabasePath
	loadPersistedSettings(fresh2)
	assert.Empty(t, fresh2.AntiDPI.PinnedGuards, "stays unpinned after restart")

	empty := &NodeApi{}
	assert.Error(t, empty.PinGuard(entry))
	assert.Error(t, empty.UnpinGuard(entry))
}
