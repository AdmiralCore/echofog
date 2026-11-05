package call

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForOneKey waits until every live node sends with the same key, and returns it.
func waitForOneKey(t *testing.T, nodes []*testNode, dead map[types.PeerID]bool) string {
	t.Helper()
	var id string
	require.Eventually(t, func() bool {
		ids := sendKeyIDs(nodes, dead)
		live := 0
		for _, nd := range nodes {
			if !dead[nd.id] {
				live++
			}
		}
		for k, n := range ids {
			id = k
			return n == live
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the room never settled on one media key")
	return id
}

// A member lost to a network timeout has not left the call: its media key stays, so when its line
// comes back it speaks the key the room still speaks and no unit is lost to a rotation it missed.
// A member that leaves gets everybody a key it never had.
func TestRoom_ATimeoutDoesNotRotateTheMediaKey(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 4)
	coord, a, b, c := nodes[0], nodes[1], nodes[2], nodes[3]
	waitForOneKey(t, nodes, nil)
	// heartbeats carry each member's key exchange key; the first ones after the joins may ask for
	// one more key. Let the room settle before taking the key it speaks.
	time.Sleep(2*rekeyDelay + media.MediaKeySwitch)
	before := waitForOneKey(t, nodes, nil)

	// a's line goes dark: no frames in or out, and the coordinator times it out
	var dark atomic.Bool
	dark.Store(true)
	for _, nd := range []*testNode{coord, b, c} {
		m.blockLink(a.id, nd.id, nil, dark.Load)
		m.blockLink(nd.id, a.id, nil, dark.Load)
	}
	coord.cm.mu.Lock()
	coord.cm.removeMemberLocked(coord.cm.activeRoom, a.id, "timeout")
	coord.cm.unlockAndFlush()
	time.Sleep(rekeyDelay + media.MediaKeySwitch + 500*time.Millisecond)
	assert.Equal(t, map[string]int{before: 3}, sendKeyIDs([]*testNode{coord, b, c}, nil),
		"a network timeout rotated the media key: the member that comes back cannot open what it missed")

	// and it is taken back in without a rotation: it still has the key the room speaks
	dark.Store(false)
	coord.cm.mu.Lock()
	prof := coord.cm.activeRoom.ConnectedPeers[coord.id]
	prof.PeerID = a.id
	coord.cm.admitLocked(coord.cm.activeRoom, a.id, prof, coord.cm.now())
	coord.cm.unlockAndFlush()
	time.Sleep(rekeyDelay + media.MediaKeySwitch + 300*time.Millisecond)
	assert.Equal(t, map[string]int{before: 4}, sendKeyIDs(nodes, nil),
		"taking a member lost to a timeout back in rotated the key it already has")

	// a member that leaves, though, takes its key with it
	dark.Store(true)
	for _, nd := range []*testNode{coord, c} {
		m.blockLink(b.id, nd.id, nil, dark.Load)
		m.blockLink(nd.id, b.id, nil, dark.Load)
	}
	coord.cm.mu.Lock()
	coord.cm.removeMemberLocked(coord.cm.activeRoom, b.id, "left")
	coord.cm.unlockAndFlush()
	require.Eventually(t, func() bool {
		for k, n := range sendKeyIDs([]*testNode{coord, c}, nil) {
			return n == 2 && k != before
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "a member that left kept the key the others went on speaking")
}

// An election ran on a node cut off from the call: it led a half of one member and then heard
// again. Its claim must not take a room that kept more members: the smaller half is the one that
// yields, or the two halves keep swapping the lead and the call never becomes one room.
func TestRoom_ASmallerHalfDoesNotTakeTheCallOver(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	coord, b, c := nodes[0], nodes[1], nodes[2]

	// b was cut off, heard nothing and led a call of one; c's heartbeat never reached it
	var cut atomic.Bool
	cut.Store(true)
	for _, nd := range []*testNode{coord, c} {
		m.blockLink(b.id, nd.id, nil, cut.Load)
		m.blockLink(nd.id, b.id, nil, cut.Load)
	}
	b.cm.mu.Lock()
	broom := b.cm.activeRoom
	b.cm.becomeCoordinatorLocked(broom, b.cm.now())
	b.cm.removeMemberLocked(broom, c.id, "timeout")
	claim := signaling.CoordinatorHeartbeatPayload{Coordinator: b.id, Epoch: broom.Epoch, Members: len(broom.ConnectedPeers)}
	data, err := b.cm.encodeLocked(broom, signaling.ActionCoordinatorHeartbeat, claim)
	b.cm.unlockAndFlush()
	require.NoError(t, err)
	require.Equal(t, 1, claim.Members, "the test needs a lone splinter")

	require.NoError(t, coord.cm.handleIncomingSignaling(data, b.id))
	s, ok := coord.cm.Snapshot()
	require.True(t, ok)
	assert.Equal(t, coord.id, s.Coordinator, "the smaller half took a room that kept the others")
}
