package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVPNStartValidation(t *testing.T) {
	n := &NodeApi{node: &node.Node{}}
	if n.GetVPNStatus().Running {
		t.Fatal("should not be running")
	}
	if err := n.startVPN("abc", "0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback must be rejected")
	}
	if err := n.startVPN("abc", "127.0.0.1:0"); err == nil {
		t.Fatal("must fail while the node is not running")
	}
	if err := n.SetExitGatewayEnabled(true); err == nil {
		t.Fatal("exit toggle needs a running node")
	}
	if err := n.StopVPN(); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyNetwork(t *testing.T) {
	n := &NodeApi{node: &node.Node{}}
	if n.GetNetworkStats().Online {
		t.Fatal("offline expected")
	}
	if m := n.GetRelayMarket(); m == nil || len(m) != 0 {
		t.Fatal("empty non-nil market expected")
	}
}

// A preset above the declared upload is not refused: the person chose it.
func TestChoosingAHigherQualityRaisesTheDeclaredUpload(t *testing.T) {
	n := &NodeApi{}
	q, err := tree.QualityByID("2160p60")
	require.NoError(t, err)
	assert.Less(t, n.callUploadKbps(), q.BitrateKbps, "the default does not carry 4K60")
	n.allowQuality(q)
	assert.GreaterOrEqual(t, n.callUploadKbps(), q.BitrateKbps)
	low, _ := tree.QualityByID("360p30")
	before := n.callUploadKbps()
	n.allowQuality(low)
	assert.Equal(t, before, n.callUploadKbps(), "a lighter preset changes nothing")
}
