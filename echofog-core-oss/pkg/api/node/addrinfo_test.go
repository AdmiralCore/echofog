package node

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPeerID(b byte) types.PeerID {
	var id types.PeerID
	id[0], id[31] = b, b
	return id
}

// Saving a bootstrap list and reading it back must not lose what makes the first dial REALITY: the
// camouflage SNI and the server role of each node.
func TestNormalizeBootstrapKeepsSNIAndRole(t *testing.T) {
	id := testPeerID(1)
	in := []string{
		"203.0.113.15:4242/" + id.String() + "?sni=cover.lab&srv=1",
		"echofog://relay/203.0.113.16:4242/" + testPeerID(2).String() + "?sni=cover.lab",
		"203.0.113.17:4242/" + testPeerID(3).String(),
		"203.0.113.15:4242/" + id.String() + "?sni=cover.lab&srv=1", // a duplicate
	}
	out, err := normalizeBootstrap(in)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"203.0.113.15:4242/" + id.String() + "?sni=cover.lab&srv=1",
		"203.0.113.16:4242/" + testPeerID(2).String() + "?sni=cover.lab",
		"203.0.113.17:4242/" + testPeerID(3).String(),
	}, out)
	// the saved entry is read again as the same node, with its facts
	a, got, err := ParseRelayAddr(out[0])
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.15:4242", a)
	assert.Equal(t, id, got)
	info, err := ParseRelayAddrInfo(out[0])
	require.NoError(t, err)
	assert.Equal(t, "cover.lab", info.SNI)
	assert.True(t, info.Server)
}

func TestNormalizeRelaySettingsKeepsSNIAndRole(t *testing.T) {
	id := testPeerID(4)
	r, err := normalizeRelaySettings(RelaySettingsDTO{
		Relays: []string{"203.0.113.15:4242/" + id.String() + "?sni=cover.lab&srv=1"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.15:4242/" + id.String() + "?sni=cover.lab&srv=1"}, r.Relays)
}

func TestParseRelayAddrInfoRejectsWhatIsNotAnAddress(t *testing.T) {
	for _, bad := range []string{"", "203.0.113.15:4242?sni=cover.lab", "203.0.113.15/" + testPeerID(1).String(), "ws:///" + testPeerID(1).String()} {
		_, err := ParseRelayAddrInfo(bad)
		assert.Error(t, err, bad)
	}
}
