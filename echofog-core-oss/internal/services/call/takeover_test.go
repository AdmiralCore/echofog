package call

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/transport"
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

	// a's line goes dark: it hears nothing, elects itself, and the coordinator times it out
	var dark atomic.Bool
	dark.Store(true)
	for _, nd := range []*testNode{coord, b, c} {
		m.blockLink(a.id, nd.id, nil, dark.Load)
		m.blockLink(nd.id, a.id, nil, dark.Load)
	}
	a.cm.mu.Lock()
	aroom := a.cm.activeRoom
	a.cm.becomeCoordinatorLocked(aroom, a.cm.now())
	a.cm.unlockAndFlush()
	coord.cm.mu.Lock()
	room := coord.cm.activeRoom
	room.rekeyAt = time.Time{} // isolate what the removal itself asks for
	epoch := room.keyEpoch
	coord.cm.removeMemberLocked(room, a.id, "timeout")
	scheduled := !room.rekeyAt.IsZero()
	coord.cm.unlockAndFlush()
	assert.False(t, scheduled, "a network timeout scheduled a new media key")
	time.Sleep(rekeyDelay + media.MediaKeySwitch + 500*time.Millisecond)
	assert.Equal(t, map[string]int{before: 3}, sendKeyIDs([]*testNode{coord, b, c}, nil),
		"a network timeout rotated the media key: the member that comes back cannot open what it missed")

	// and it is taken back in without a rotation, through the real join: the member, its line
	// back, yields to the coordinator that kept the call and sends it a join
	dark.Store(false)
	a.cm.mu.Lock()
	a.cm.yieldLocked(aroom, coord.id, epoch, a.cm.now())
	a.cm.unlockAndFlush()
	require.Eventually(t, func() bool {
		coord.cm.mu.Lock()
		defer coord.cm.mu.Unlock()
		_, member := coord.cm.activeRoom.ConnectedPeers[a.id]
		return member
	}, 5*time.Second, 20*time.Millisecond, "the returning member was not taken back in")
	time.Sleep(rekeyDelay + media.MediaKeySwitch + 300*time.Millisecond)
	coord.cm.mu.Lock()
	keyNow := coord.cm.activeRoom.keyEpoch
	coord.cm.unlockAndFlush()
	assert.Equal(t, epoch, keyNow, "the real join rotated the key the member already has")
	assert.Equal(t, map[string]int{before: 4}, sendKeyIDs(nodes, nil),
		"taking a member lost to a timeout back in rotated the key it already has")

	// a member that leaves, though, takes its key with it
	dark.Store(true)
	for _, nd := range []*testNode{coord, c} {
		m.blockLink(b.id, nd.id, nil, dark.Load)
		m.blockLink(nd.id, b.id, nil, dark.Load)
	}
	coord.cm.mu.Lock()
	room = coord.cm.activeRoom
	room.rekeyAt = time.Time{}
	coord.cm.removeMemberLocked(room, b.id, "left")
	leftScheduled := !room.rekeyAt.IsZero()
	coord.cm.unlockAndFlush()
	assert.True(t, leftScheduled, "a member that left did not schedule a new media key")
}

// A media key handed out once can be lost on a link that broke while it was in flight. The member
// then cannot open anything, and nothing asks again: it is without the key to the end of the call.
// The coordinator must keep sending the key until the member confirms it, and keep sending media
// with the key the member has meanwhile, so no unit is lost to the gap.
func TestRoom_AKeyLostInFlightIsSentAgain(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 4)
	coord, b, c, d := nodes[0], nodes[1], nodes[2], nodes[3]
	waitForOneKey(t, nodes, nil)
	time.Sleep(2*rekeyDelay + media.MediaKeySwitch)
	before := waitForOneKey(t, nodes, nil)

	// the next key's frame to b is lost on the way (heartbeats and media still get through)
	m.mu.Lock()
	m.dropTo = func(from, to types.PeerID, tp transport.Kind, payload []byte) bool {
		if from != coord.id || to != b.id {
			return false
		}
		if tp != transport.Signal && tp != transport.SignalLossy {
			return false
		}
		env, err := signaling.DecodeSignal(payload)
		return err == nil && env.Action == signaling.ActionMediaKey
	}
	m.mu.Unlock()
	t.Cleanup(func() {
		m.mu.Lock()
		m.dropTo = nil
		m.mu.Unlock()
	})
	// d leaves, which rotates the key; d is cut off so it cannot rejoin meanwhile
	for _, nd := range []*testNode{coord, b, c} {
		m.blockLink(d.id, nd.id, nil, func() bool { return true })
		m.blockLink(nd.id, d.id, nil, func() bool { return true })
	}
	coord.cm.mu.Lock()
	coord.cm.removeMemberLocked(coord.cm.activeRoom, d.id, "left")
	coord.cm.unlockAndFlush()

	// the coordinator waits for b's confirmation: it keeps sending with the key b has
	require.Eventually(t, func() bool {
		coord.cm.mu.Lock()
		defer coord.cm.mu.Unlock()
		return len(coord.cm.activeRoom.keyWait) > 0
	}, 5*time.Second, 20*time.Millisecond, "the coordinator never asked for a confirmation")
	time.Sleep(media.MediaKeySwitch + 500*time.Millisecond)
	coord.cm.mu.Lock()
	sendNow := coord.cm.activeRoom.Dispatcher.SendKeyID()
	coord.cm.unlockAndFlush()
	assert.Equal(t, before, sendNow, "the coordinator switched to a key b never got")

	// what it sends meanwhile is still openable by b
	beforeUnits := b.log.count(media.FrameAudio, coord.id, "voice")
	unit := make([]byte, 8)
	unit[7] = 42
	coord.cm.mu.RLock()
	cd := coord.cm.activeRoom.Dispatcher
	coord.cm.mu.RUnlock()
	require.NoError(t, cd.BroadcastLocalAudio(unit))
	require.Eventually(t, func() bool {
		return b.log.count(media.FrameAudio, coord.id, "voice") > beforeUnits
	}, 3*time.Second, 20*time.Millisecond, "the member could not open what was sent while it lacked the new key")

	// the path works again: the key goes through and both speak it
	m.mu.Lock()
	m.dropTo = nil
	m.mu.Unlock()
	require.Eventually(t, func() bool {
		coord.cm.mu.Lock()
		coordID := coord.cm.activeRoom.Dispatcher.SendKeyID()
		coord.cm.unlockAndFlush()
		b.cm.mu.Lock()
		bID := b.cm.activeRoom.Dispatcher.SendKeyID()
		b.cm.unlockAndFlush()
		return coordID == bID && coordID != before
	}, 15*time.Second, 50*time.Millisecond, "the lost key was never sent again")
	assert.Zero(t, b.cm.dispatcher().Stats().DecryptFailures, "the member lost units to the key it did not have")
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

// A call split by a blackout into halves of three and two, both with a coordinator and more than
// one member: the half that kept fewer members yields to the one that kept more, whatever its
// epoch, or the two halves keep rejecting each other's heartbeats and stay apart.
func TestRoom_ASmallerHalfYieldsToTheLarger(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 5)
	large, small := nodes[:3], nodes[3:]

	var cut atomic.Bool
	cut.Store(true)
	for _, l := range large {
		for _, s := range small {
			m.blockLink(l.id, s.id, nil, cut.Load)
			m.blockLink(s.id, l.id, nil, cut.Load)
		}
	}
	// both halves lose the others to timeouts and settle at their own members
	require.Eventually(t, func() bool {
		for _, nd := range large {
			s, ok := nd.cm.Snapshot()
			if !ok || !s.Joined || len(s.Members) != len(large) {
				return false
			}
		}
		return true
	}, 30*time.Second, 100*time.Millisecond, "the larger half never settled at its members")
	require.Eventually(t, func() bool {
		leaders := 0
		for _, nd := range small {
			s, ok := nd.cm.Snapshot()
			if ok && s.Joined && len(s.Members) == len(small) && s.IsCoordinator {
				leaders++
			}
		}
		return leaders == 1
	}, 30*time.Second, 100*time.Millisecond, "the smaller half never settled on one coordinator")

	// the halves meet again: the larger one is the call. The lone coordinator of the smaller half
	// yields when a heartbeat of the larger one reaches it, which on a loaded runner takes longer
	// than the election windows that produced the split.
	cut.Store(false)
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 45*time.Second, 100*time.Millisecond,
		"the halves stayed apart: %v", dumpRoom(nodes, nil))
	for _, nd := range nodes {
		s, ok := nd.cm.Snapshot()
		require.True(t, ok)
		assert.Equal(t, nodes[0].id, s.Coordinator, "a half with fewer members kept the call")
	}
}
