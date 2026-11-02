package call

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A media key sealed to one member opens for that member only, and not once tampered with.
func TestMediaKeySealOpensForItsMemberOnly(t *testing.T) {
	alice, _ := ecdh.X25519().GenerateKey(rand.Reader)
	bob, _ := ecdh.X25519().GenerateKey(rand.Reader)
	var aliceID, bobID types.PeerID
	aliceID[0], bobID[0] = 1, 2
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	p, err := sealMediaKey(key, alice.PublicKey().Bytes(), "room-1", aliceID, 3)
	require.NoError(t, err)
	got, err := openMediaKey(p, alice, "room-1", aliceID)
	require.NoError(t, err)
	assert.Equal(t, key, got)

	_, err = openMediaKey(p, bob, "room-1", bobID)
	assert.Error(t, err, "another member's key does not open it")
	_, err = openMediaKey(p, alice, "room-2", aliceID)
	assert.Error(t, err, "bound to the call")
	other := p
	other.KeyEpoch = 4
	_, err = openMediaKey(other, alice, "room-1", aliceID)
	assert.Error(t, err, "bound to the key epoch")
	other = p
	other.Sealed = append([]byte(nil), p.Sealed...)
	other.Sealed[0] ^= 1
	_, err = openMediaKey(other, alice, "room-1", aliceID)
	assert.Error(t, err, "tampered")
}

func sendKeyIDs(nodes []*testNode, skip map[types.PeerID]bool) map[string]int {
	out := map[string]int{}
	for _, nd := range nodes {
		if skip[nd.id] {
			continue
		}
		if d := nd.cm.dispatcher(); d != nil {
			out[d.SendKeyID()]++
		}
	}
	return out
}

// When a member leaves, everybody still in the call moves to a key the leaver never got, and the
// voice still gets through on it.
func TestRoom_MediaKeyRotatesWhenAMemberLeaves(t *testing.T) {
	m := newMesh()
	nodes, roomKey := newRoom(t, m, 4)
	d := media.NewMediaDispatcher(types.PeerID{}, nil, nil, nil)
	require.NoError(t, d.SetRoomKey(roomKey))
	roomID := d.SendKeyID()

	// the joins already brought a key of their own, not the invite's
	var first string
	require.Eventually(t, func() bool {
		ids := sendKeyIDs(nodes, nil)
		for id, n := range ids {
			first = id
			return n == 4 && id != roomID
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the members never moved to a shared new key")

	leaver := nodes[3]
	require.NoError(t, leaver.cm.LeaveRoom())
	gone := map[types.PeerID]bool{leaver.id: true}
	var second string
	require.Eventually(t, func() bool {
		ids := sendKeyIDs(nodes, gone)
		for id, n := range ids {
			second = id
			return n == 3 && id != first && id != roomID
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the members did not move to a key the leaver lacks")
	assert.NotEqual(t, first, second)

	before := nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice")
	require.NoError(t, nodes[1].cm.dispatcher().BroadcastLocalAudio([]byte("hello after the leave")))
	require.Eventually(t, func() bool {
		return nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice") > before
	}, 5*time.Second, 20*time.Millisecond, "voice did not get through on the new key")
	for _, nd := range nodes[:3] {
		d := nd.cm.dispatcher()
		assert.True(t, d.SignsUnits(), "every member signs: units under the new key carry their sender's signature")
		assert.Zero(t, d.Stats().Forged, "and every one of them checks out")
	}
}

// A member of a build with key rotation but without unit signatures sends no signing key: the
// members still move to a new key, but its units go unsigned, and so do everybody's.
func TestRoom_MemberWithoutSigningKeyGetsAnUnsignedKey(t *testing.T) {
	m := newMesh()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	d := media.NewMediaDispatcher(types.PeerID{}, nil, nil, nil)
	require.NoError(t, d.SetRoomKey(key))
	roomID := d.SendKeyID()

	nodes := make([]*testNode, 3)
	for i := range nodes {
		s := newTestSigner()
		cm := newCallManager(meshPort{m, s.id()}, s, s.id())
		log := &mediaLog{units: make(map[string]map[uint32]bool)}
		cm.OnMedia = log.tap
		nodes[i] = &testNode{cm: cm, id: s.id(), log: log}
		m.mu.Lock()
		m.nodes[s.id()] = cm
		m.mu.Unlock()
	}
	nodes[2].cm.unsignedNode = true
	for _, nd := range nodes {
		require.NoError(t, nd.cm.JoinRoom("room-1", nodes[0].id, key, tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}))
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			_ = nd.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool {
		ids := sendKeyIDs(nodes, nil)
		for id, n := range ids {
			return n == 3 && id != roomID
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the members never moved to a shared new key")
	for _, nd := range nodes {
		assert.False(t, nd.cm.dispatcher().SignsUnits(), "a member without a signing key could not check signatures")
	}
	before := nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice")
	require.NoError(t, nodes[1].cm.dispatcher().BroadcastLocalAudio([]byte("hello, unsigned")))
	require.Eventually(t, func() bool {
		return nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice") > before
	}, 5*time.Second, 20*time.Millisecond, "the member without a signing key hears nothing")
}

// With a node of an older version in the call, which cannot take a new key, everybody stays on
// the room key it speaks, and the call works as it did before rotation.
func TestRoom_OlderNodeKeepsEverybodyOnTheRoomKey(t *testing.T) {
	m := newMesh()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	d := media.NewMediaDispatcher(types.PeerID{}, nil, nil, nil)
	require.NoError(t, d.SetRoomKey(key))
	roomID := d.SendKeyID()

	nodes := make([]*testNode, 3)
	for i := range nodes {
		s := newTestSigner()
		cm := newCallManager(meshPort{m, s.id()}, s, s.id())
		log := &mediaLog{units: make(map[string]map[uint32]bool)}
		cm.OnMedia = log.tap
		nodes[i] = &testNode{cm: cm, id: s.id(), log: log}
		m.mu.Lock()
		m.nodes[s.id()] = cm
		m.mu.Unlock()
	}
	nodes[2].cm.olderNode = true
	for _, nd := range nodes {
		require.NoError(t, nd.cm.JoinRoom("room-1", nodes[0].id, key, tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}))
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			_ = nd.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(rekeyDelay + media.MediaKeySwitch + 500*time.Millisecond)
	assert.Equal(t, map[string]int{roomID: 3}, sendKeyIDs(nodes, nil))
	for _, nd := range nodes {
		assert.False(t, nd.cm.dispatcher().SignsUnits(), "the room key is never signed under")
	}

	before := nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice")
	require.NoError(t, nodes[1].cm.dispatcher().BroadcastLocalAudio([]byte("hello, older node")))
	require.Eventually(t, func() bool {
		return nodes[2].log.count(media.FrameAudio, nodes[1].id, "voice") > before
	}, 5*time.Second, 20*time.Millisecond, "the older node hears nothing")
}
