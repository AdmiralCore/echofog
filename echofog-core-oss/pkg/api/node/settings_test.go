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
