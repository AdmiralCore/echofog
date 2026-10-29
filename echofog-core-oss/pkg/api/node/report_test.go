package node

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What could point at a person or a place does not reach the report.
func TestCleanReportText(t *testing.T) {
	in := "dial 203.0.113.7:4001 to 98466572ebb322301eec59937bebc6677023c909471344ff493d036dbb9f8ad0 via echofog://relay/1.2.3.4:443/abc " +
		"lookup relay.example.org:443 failed; peer 5f0909b6648e after 1.5s, see swarm_guards.go:120"
	out := cleanReportText(in)
	for _, leak := range []string{"203.0.113.7", "98466572", "echofog://", "relay.example.org", "5f0909b6648e"} {
		assert.NotContains(t, out, leak)
	}
	assert.Contains(t, out, "1.5s", "durations stay")
	assert.Contains(t, out, "swarm_guards.go:120", "source positions stay")
	assert.Contains(t, out, ":4001", "the port stays: it says which transport")
}

// The report carries the latest warnings and errors with their addresses, IDs and hosts left out,
// and says what it knows about the node; on a node that is not connected it still comes out.
func TestConnectionReportLeavesNothingIdentifying(t *testing.T) {
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	api := NewNodeApi(node.NewNode(t.Context(), cfg, "t"))
	// NewNode sets the process log up; write to it after
	slog.Warn("exit is not serving us", "exit", "98466572ebb322301eec59937bebc6677023c909471344ff493d036dbb9f8ad0",
		"state", "unreachable", "addr", "34.32.224.253:443", "why", "timed out dialing 34.32.224.253:443")
	slog.Error("guard not reachable", "guard", "5f0909b6648e", "host", "relay.example.org", "err", "i/o timeout")
	slog.Info("Peer authenticated over TCP", "peer_id", "5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d")

	r, err := api.ConnectionReport("v0.1.0")
	require.NoError(t, err)
	t.Log("\n" + r.Text)

	assert.Equal(t, "v0.1.0", r.Version)
	for _, want := range []string{"app: v0.1.0", "guard mode: auto", "proxy: none", "vpn: off", "exit is not serving us", "state=unreachable", "guard not reachable"} {
		assert.Contains(t, r.Text, want)
	}
	for _, leak := range []string{"34.32.224.253", "98466572", "5f0909b6648e", "relay.example.org", "Peer authenticated"} {
		assert.NotContains(t, r.Text, leak, "an info line or an identifying value got in")
	}
	assert.True(t, strings.Contains(r.Text, "why=timed out dialing [ip]:443"), "the reason stays, the address goes")
}
