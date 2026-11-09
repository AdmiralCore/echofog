package call

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedRoom builds a converged room and keeps the signers, so a test can send a valid signed
// envelope in a member's or a stranger's name.
func signedRoom(t *testing.T, n int) ([]*testNode, []*testSigner, *mesh) {
	t.Helper()
	m := newMesh()
	key := make([]byte, 32)
	nodes := make([]*testNode, n)
	signers := make([]*testSigner, n)
	for i := range nodes {
		s := newTestSigner()
		signers[i] = s
		cm := newCallManager(meshPort{m, s.id()}, s, s.id())
		nodes[i] = &testNode{cm: cm, id: s.id(), log: &mediaLog{units: map[string]map[uint32]bool{}}}
		m.nodes[s.id()] = cm
	}
	for _, nd := range nodes {
		require.NoError(t, nd.cm.JoinRoom("room-1", nodes[0].id, key, tree.PeerNodeProfile{UploadKbps: 40000}))
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			_ = nd.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 5*time.Second, 20*time.Millisecond, "room did not form")
	return nodes, signers, m
}

func sendSigned(t *testing.T, cm *CallManager, s *testSigner, from types.PeerID, a signaling.SignalAction, epoch uint64, payload any) {
	t.Helper()
	env, err := signaling.EncodeSignedSignal(s, a, "room-1", from, epoch, payload)
	require.NoError(t, err)
	require.NoError(t, cm.handleIncomingSignaling(env, from))
}

// A peer with a session and the call ID but no place in the call must not restructure its tree:
// parent_lost and stream_subscribe are checked against the room's membership.
func TestNonMemberCannotChangeTheTree(t *testing.T) {
	nodes, signers, _ := signedRoom(t, 3)
	coord := nodes[0].cm
	strangerSigner := newTestSigner()
	stranger := strangerSigner.id()

	// A live member's parent is "reported lost" by a stranger.
	parent := nodes[1].id
	sendSigned(t, coord, strangerSigner, stranger, signaling.ActionParentLost, 1, parent)
	coord.mu.RLock()
	_, reported := coord.activeRoom.reportedLost[parent]
	coord.mu.RUnlock()
	assert.False(t, reported, "a non-member made the coordinator report a live parent lost")

	// A stream gets a subscriber who is not in the call.
	coord.mu.Lock()
	_, err := coord.activeRoom.StreamEngine.RegisterStream("s1", nodes[1].id, "screen", 1000, tree.PeerNodeProfile{UploadKbps: 40000})
	coord.mu.Unlock()
	require.NoError(t, err)
	sendSigned(t, coord, strangerSigner, stranger, signaling.ActionStreamSubscribe, 2,
		signaling.StreamSubscribePayload{StreamID: "s1", Subscriber: stranger, Profile: tree.PeerNodeProfile{UploadKbps: 40000}})
	coord.mu.RLock()
	sm, ok := coord.activeRoom.StreamEngine.GetStreamManifest("s1")
	_, attached := sm.ParentMap[stranger]
	coord.mu.RUnlock()
	assert.False(t, ok && attached, "a non-member was attached to a stream tree")

	// A member still does both.
	coord.mu.Lock()
	coord.activeRoom.memberSeen[nodes[2].id] = time.Now()
	coord.mu.Unlock()
	sendSigned(t, coord, signers[1], nodes[1].id, signaling.ActionParentLost, 3, nodes[2].id)
	coord.mu.RLock()
	_, reported = coord.activeRoom.reportedLost[nodes[2].id]
	coord.mu.RUnlock()
	assert.True(t, reported, "a member's parent_lost was ignored")

	sendSigned(t, coord, signers[2], nodes[2].id, signaling.ActionStreamSubscribe, 4,
		signaling.StreamSubscribePayload{StreamID: "s1", Subscriber: nodes[2].id, Profile: tree.PeerNodeProfile{UploadKbps: 40000}})
	coord.mu.RLock()
	sm, ok = coord.activeRoom.StreamEngine.GetStreamManifest("s1")
	_, attached = sm.ParentMap[nodes[2].id]
	coord.mu.RUnlock()
	assert.True(t, ok && attached, "a member could not subscribe")
}
