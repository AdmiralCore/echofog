package circuitswitch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A call between two peers behind relays measures its round trip through the relay with the
// circuit's own ping, and moves to another relay when asked: the far end follows by itself, and
// a relay that cannot reach the far end changes nothing.
func TestCircuitRoundTripAndSwitch(t *testing.T) {
	relay := func(c *config.AppConfig) { c.Relay.Server.Enabled = true; c.Network.NoAutoDial = true }
	noAutoDial := func(c *config.AppConfig) { c.Network.NoAutoDial = true }
	r1N, _ := startNodeWith(t, "sw-relay1", relay)
	r2N, _ := startNodeWith(t, "sw-relay2", relay)
	r3N, _ := startNodeWith(t, "sw-relay3", relay)
	aN, a := startNodeWith(t, "sw-alice", noAutoDial)
	bN, b := startNodeWith(t, "sw-bob", noAutoDial)
	for _, r := range []string{
		network.JoinPeerAddr(r1N.Transport.Addr().String(), r1N.ID),
		network.JoinPeerAddr(r2N.Transport.Addr().String(), r2N.ID),
	} {
		require.NoError(t, a.ConnectToPeer(r))
		require.NoError(t, b.ConnectToPeer(r))
	}
	// only alice knows the third relay: it cannot carry a circuit to bob
	require.NoError(t, a.ConnectToPeer(network.JoinPeerAddr(r3N.Transport.Addr().String(), r3N.ID)))
	require.Eventually(t, func() bool {
		return aN.Swarm.GetPeer(r1N.ID) != nil && aN.Swarm.GetPeer(r2N.ID) != nil && aN.Swarm.GetPeer(r3N.ID) != nil &&
			bN.Swarm.GetPeer(r1N.ID) != nil && bN.Swarm.GetPeer(r2N.ID) != nil
	}, 5*time.Second, 50*time.Millisecond)
	require.Nil(t, aN.Swarm.GetPeer(bN.ID), "alice and bob must only meet through relays")

	var viaR1, viaR2 atomic.Int64
	bN.RelayClient.AddDataListener(func(cid uint32, ty net_types.MessageType, _ []byte, _ types.PeerID) {
		if ty != net_types.TypeCallSignaling {
			return
		}
		if r, ok := bN.RelayClient.RouteTo(aN.ID); ok && r.Relay == r2N.ID {
			viaR2.Add(1)
		} else {
			viaR1.Add(1)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := aN.RelayClient.OpenCircuit(ctx, r1N.ID, bN.ID)
	require.NoError(t, err)
	route, ok := aN.RelayClient.RouteTo(bN.ID)
	require.True(t, ok)
	assert.Equal(t, r1N.ID, route.Relay)
	assert.Zero(t, route.RTT, "nothing measured yet")

	// both sides talk, as a call does; bob answers over the circuit alice opened
	send := func(from, to types.PeerID, rc *routing.RelayClient) {
		if cid, ok := rc.CircuitTo(to); ok {
			_ = rc.SendCircuitData(cid, net_types.TypeCallSignaling, []byte("hb"))
		}
	}
	talk := func(d time.Duration) {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			send(aN.ID, bN.ID, aN.RelayClient)
			send(bN.ID, aN.ID, bN.RelayClient)
		}
	}
	talk(200 * time.Millisecond)

	require.True(t, aN.RelayClient.PingCircuit(bN.ID))
	require.Eventually(t, func() bool {
		r, _ := aN.RelayClient.RouteTo(bN.ID)
		return r.RTT > 0
	}, 3*time.Second, 20*time.Millisecond, "the far end answers the ping")
	assert.False(t, aN.RelayClient.PingCircuit(bN.ID), "at most one ping every half second")

	// a relay bob is not connected to cannot take the call: the old circuit stays
	_, err = aN.RelayClient.SwitchCircuit(ctx, bN.ID, r3N.ID)
	assert.ErrorIs(t, err, routing.ErrNoOtherRelay)
	route, _ = aN.RelayClient.RouteTo(bN.ID)
	assert.Equal(t, r1N.ID, route.Relay)

	// with no relay named, the best other one that reaches bob
	moved, err := aN.RelayClient.SwitchCircuit(ctx, bN.ID, types.PeerID{})
	require.NoError(t, err)
	assert.Equal(t, r2N.ID, moved)
	route, _ = aN.RelayClient.RouteTo(bN.ID)
	assert.Equal(t, r2N.ID, route.Relay)

	// bob moves over once the old circuit has gone quiet, and keeps talking
	talk(3 * time.Second)
	back, ok := bN.RelayClient.RouteTo(aN.ID)
	require.True(t, ok)
	assert.Equal(t, r2N.ID, back.Relay, "the far end follows the circuit that carries the call")
	before := viaR2.Load()
	talk(300 * time.Millisecond)
	assert.Greater(t, viaR2.Load(), before, "frames keep arriving over the new relay")

	again, err := aN.RelayClient.SwitchCircuit(ctx, bN.ID, r2N.ID)
	require.NoError(t, err)
	assert.Equal(t, r2N.ID, again, "already there: nothing to do")
}
