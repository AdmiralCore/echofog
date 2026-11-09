package node

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bootMask = "0202020202020202020202020202020202020202020202020202020202020202"
	bootDev  = "0101010101010101010101010101010101010101010101010101010101010101"
	bootLine = "noise3://127.0.0.1:9?mask=" + bootMask
)

// TestV2BootstrapUserEntriesPersist: the user's own entry is saved in the profile (node.json) and
// is listed again after a restart; the OSS build has no built-in entries.
func TestV2BootstrapUserEntriesPersist(t *testing.T) {
	p := newV2Peer(t, "boot")
	p.start()

	st, err := p.api.GetBootstrapNodes()
	require.NoError(t, err)
	assert.Equal(t, 2, st.Protocol)
	assert.Empty(t, st.BuiltIn, "a source build carries no addresses")
	assert.Empty(t, st.Custom)

	require.NoError(t, p.api.AddBootstrapNode(bootLine))
	st, err = p.api.GetBootstrapNodes()
	require.NoError(t, err)
	require.Len(t, st.Custom, 1)
	assert.Equal(t, bootLine, st.Custom[0].Link)
	assert.Equal(t, "127.0.0.1:9", st.Custom[0].Addr)
	assert.Equal(t, bootDev, st.Custom[0].PeerID)
	assert.Equal(t, "device", st.Custom[0].Kind)
	assert.True(t, st.Custom[0].Enabled)

	// the entry is in the profile, not only in memory
	p.api.StopNode()
	require.NoError(t, p.api.StartNode())
	st, err = p.api.GetBootstrapNodes()
	require.NoError(t, err)
	require.Len(t, st.Custom, 1, "the user's entry survives a restart")
	assert.Equal(t, bootLine, st.Custom[0].Link)

	require.NoError(t, p.api.RemoveBootstrapNode(bootLine))
	st, err = p.api.GetBootstrapNodes()
	require.NoError(t, err)
	assert.Empty(t, st.Custom)
}

// TestV2BootstrapCheckKeepsAddressOutOfLog: the "Проверить" button reports an unreachable node and
// leaves no address in the log.
func TestV2BootstrapCheckKeepsAddressOutOfLog(t *testing.T) {
	p := newV2Peer(t, "bootlog")
	logs := &syncLog{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p.api.Logger = logger
	p.node.Logger = logger
	p.start()

	ok, err := p.api.CheckBootstrapNode(bootLine)
	require.NoError(t, err)
	assert.False(t, ok, "nothing listens on port 9")
	text := logs.String()
	assert.False(t, strings.Contains(text, "127.0.0.1"), "the log carries an address: %s", text)
	assert.False(t, strings.Contains(text, bootMask), "the log carries a key: %s", text)
}

// TestV2BootstrapToggleNeedsABuiltinEntry: the toggle addresses the official list by number, so an
// empty list refuses it rather than silently doing nothing.
func TestV2BootstrapToggleNeedsABuiltinEntry(t *testing.T) {
	p := newV2Peer(t, "boottoggle")
	p.start()
	err := p.api.SetBootstrapNodeEnabled(1, false)
	require.Error(t, err)
}
