package node

import (
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
)

func TestExitFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	settled := failoverGrace + time.Second
	failing := exit_gateway.StreamHealth{FailStreak: failStreakLimit, FailHosts: 2}
	cases := []struct {
		name string
		c    exitCheck
		want string
	}{
		{"working", exitCheck{State: ExitOK, Using: settled}, ""},
		{"exit turned off", exitCheck{State: ExitDisabled}, SwitchDisabled},
		{"just switched: give it time", exitCheck{Lost: 5, State: ExitUnreachable, Using: time.Second}, ""},
		{"link blinked once", exitCheck{Lost: 1, State: ExitOK, Using: settled}, ""},
		{"link gone", exitCheck{Lost: lostChecks, State: ExitOK, Using: settled}, SwitchLost},
		{"does not answer", exitCheck{State: ExitUnreachable, Using: settled}, SwitchNoAnswer},
		{"nothing opens", exitCheck{State: ExitOK, Health: failing, Using: settled}, SwitchFailing},
		{"one dead site is not the exit's fault", exitCheck{State: ExitOK, Health: exit_gateway.StreamHealth{FailStreak: 9, FailHosts: 1}, Using: settled}, ""},
		{"failures but something worked a moment ago", exitCheck{State: ExitOK, Using: settled,
			Health: exit_gateway.StreamHealth{FailStreak: 5, FailHosts: 3, LastOK: now.Add(-5 * time.Second)}}, ""},
		{"quota is for the payment card, not a switch", exitCheck{State: ExitQuota, Using: settled}, ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, exitFailure(c.c, now), c.name)
	}
}

func TestSwitchExitMovesTheRunningProxy(t *testing.T) {
	from, to := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	n := &NodeApi{node: &node.Node{}}
	n.vpn.server = exit_gateway.NewSOCKS5Server("127.0.0.1:0", nil)
	n.vpn.relayPeer, n.vpn.exitState = from, ExitUnreachable

	assert.False(t, n.switchExit(to, from, SwitchLost), "a stale switch from an exit we no longer use does nothing")
	assert.True(t, n.switchExit(from, to, SwitchNoAnswer))

	st := n.GetVPNStatus()
	assert.Equal(t, to, st.RelayPeerID)
	assert.Equal(t, ExitChecking, st.ExitState)
	assert.Equal(t, 1, st.Switches)
	assert.Equal(t, VPNSwitch{From: from, To: to, Reason: SwitchNoAnswer, At: st.LastSwitch.At}, st.LastSwitch)
	assert.False(t, st.Searching)
	assert.NoError(t, n.StopVPN())
}

func TestAvoidedExitsExpire(t *testing.T) {
	n := &NodeApi{}
	now := time.Unix(1_800_000_000, 0)
	n.vpn.avoid = map[string]time.Time{"old": now.Add(-time.Second), "bad": now.Add(time.Minute)}
	n.vpn.mu.Lock()
	got := n.avoidedLocked(now)
	n.vpn.mu.Unlock()
	assert.Equal(t, map[string]bool{"bad": true}, got)
	assert.NotContains(t, n.vpn.avoid, "old")
}
