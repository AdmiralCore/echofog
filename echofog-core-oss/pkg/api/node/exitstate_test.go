package node

import (
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
)

func TestClassifyExit(t *testing.T) {
	cases := []struct {
		name  string
		info  *exit_gateway.AccountInfo
		err   error
		state string
		says  string
	}{
		{"no answer", nil, errors.New("stream reset"), ExitUnreachable, "Exit unreachable"},
		{"exit off", &exit_gateway.AccountInfo{Enabled: false}, nil, ExitDisabled, "does not share its connection"},
		{"free exit", &exit_gateway.AccountInfo{Enabled: true}, nil, ExitOK, ""},
		{"priced, free quota left", &exit_gateway.AccountInfo{Enabled: true, PricePerMB: 49, FreeLeft: 1000}, nil, ExitOK, ""},
		{"priced, quota used", &exit_gateway.AccountInfo{Enabled: true, PricePerMB: 49}, nil, ExitQuota, "Free traffic used up"},
		{"priced, paid", &exit_gateway.AccountInfo{Enabled: true, PricePerMB: 49, Credit: 1 << 20}, nil, ExitOK, ""},
		{"priced, vault credit", &exit_gateway.AccountInfo{Enabled: true, PricePerMB: 49, Vault: &exit_gateway.VaultTerms{Credit: 5}}, nil, ExitOK, ""},
	}
	for _, c := range cases {
		state, notice := classifyExit(c.info, c.err)
		assert.Equal(t, c.state, state, c.name)
		if c.says == "" {
			assert.Empty(t, notice, c.name)
		} else {
			assert.Contains(t, notice, c.says, c.name)
		}
	}
}

// The status reports what the exit said, not just that the peer link is up.
func TestVPNStatusCarriesExitState(t *testing.T) {
	n := &NodeApi{node: &node.Node{}}
	relay := "aa"
	n.vpn.server = exit_gateway.NewSOCKS5Server("127.0.0.1:0", nil)
	n.vpn.relayPeer = relay
	n.vpn.exitState = ExitChecking

	n.noteExitState(relay, &exit_gateway.AccountInfo{Enabled: false}, nil)
	st := n.GetVPNStatus()
	assert.True(t, st.Running)
	assert.Equal(t, ExitDisabled, st.ExitState)
	assert.False(t, st.TunnelReady)

	n.noteExitState("bb", &exit_gateway.AccountInfo{Enabled: true}, nil)
	assert.Equal(t, ExitDisabled, n.GetVPNStatus().ExitState, "an answer from a previous exit is ignored")

	n.noteExitState(relay, &exit_gateway.AccountInfo{Enabled: true, PricePerMB: 10}, nil)
	assert.Equal(t, ExitQuota, n.GetVPNStatus().ExitState)

	assert.NoError(t, n.StopVPN())
	assert.Empty(t, n.GetVPNStatus().ExitState)
	n.noteExitState(relay, nil, errors.New("late"))
	assert.Empty(t, n.GetVPNStatus().ExitState, "nothing is recorded once disconnected")
}
