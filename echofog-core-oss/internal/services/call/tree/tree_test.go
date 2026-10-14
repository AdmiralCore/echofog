package tree

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makePeerID(val byte) types.PeerID {
	var id types.PeerID
	id[0] = val
	return id
}

func TestVoiceTree_TopologyConstructionAndFailover(t *testing.T) {
	coordID := makePeerID(1)

	// Create 15 peers: Coordinator, 5 strong PC relays, 5 normal PCs, 4 mobile phones
	var profiles []PeerNodeProfile
	for i := 1; i <= 15; i++ {
		id := makePeerID(byte(i))
		isMobile := i >= 12
		maxChildren := 3
		upload := 10000 // 10 Mbps
		if isMobile {
			maxChildren = 0
			upload = 1000
		} else if i <= 5 {
			maxChildren = 4
			upload = 50000 // 50 Mbps
		}

		profiles = append(profiles, PeerNodeProfile{
			PeerID:            id,
			UploadKbps:        upload,
			MaxRelayChildren:  maxChildren,
			RTT:               time.Duration(i*5) * time.Millisecond,
			PacketLossPercent: 0.1,
			IsMobile:          isMobile,
			LastHeartbeat:     time.Now(),
		})
	}

	manifest := BuildVoiceTopology("call-room-alpha", 1, coordID, profiles)
	require.NotNil(t, manifest)
	assert.Equal(t, "call-room-alpha", manifest.CallID)
	assert.Equal(t, uint64(1), manifest.Epoch)
	assert.Equal(t, coordID, manifest.Coordinator)

	// Verify all 14 non-coordinator peers have a primary and standby parent
	for i := 2; i <= 15; i++ {
		peerID := makePeerID(byte(i))
		parentAss, ok := manifest.ParentMap[peerID]
		require.True(t, ok, "peer %d should have a parent assignment", i)
		assert.NotEqual(t, types.PeerID{}, parentAss.PrimaryParent)
		assert.NotEqual(t, peerID, parentAss.PrimaryParent, "peer cannot be its own parent")
	}

	// Verify mobile peers (12..15) are strictly leaves (no children)
	for i := 12; i <= 15; i++ {
		peerID := makePeerID(byte(i))
		children := manifest.ChildrenMap[peerID]
		assert.Empty(t, children, "mobile peer %d must have no children", i)
	}

	// Verify Standby Coordinator election priority
	require.NotEmpty(t, manifest.StandbyCoordinators)
	assert.NotContains(t, manifest.StandbyCoordinators, coordID)

	// Test Failover: Coordinator 1 dies
	alivePeers := make(map[types.PeerID]bool)
	for i := 2; i <= 15; i++ {
		alivePeers[makePeerID(byte(i))] = true
	}

	newLeader, ok := ElectNewCoordinator(manifest.StandbyCoordinators, alivePeers)
	require.True(t, ok)
	assert.NotEqual(t, coordID, newLeader)
	assert.True(t, alivePeers[newLeader])

	// Rebuild topology with new coordinator
	manifestV2 := BuildVoiceTopology("call-room-alpha", 2, newLeader, profiles)
	assert.Equal(t, uint64(2), manifestV2.Epoch)
	assert.Equal(t, newLeader, manifestV2.Coordinator)
	assert.Equal(t, newLeader, manifestV2.ParentMap[coordID].PrimaryParent)
}

func TestStreamTreeEngine_MultiStreamingAndSubscriptions(t *testing.T) {
	engine := NewStreamTreeEngine()

	aliceID := makePeerID(10) // Alice: Stream 1 (1080p Screen)
	bobID := makePeerID(20)   // Bob: Stream 2 (720p Webcam)

	aliceProf := PeerNodeProfile{PeerID: aliceID, UploadKbps: 15000, MaxRelayChildren: 4}
	bobProf := PeerNodeProfile{PeerID: bobID, UploadKbps: 8000, MaxRelayChildren: 3}

	// Register Alice's stream
	mAlice, err := engine.RegisterStream("alice_screen", aliceID, "screen", 3500, aliceProf)
	require.NoError(t, err)
	assert.Equal(t, "alice_screen", mAlice.StreamID)

	// Register Bob's stream (Multi-stream test)
	mBob, err := engine.RegisterStream("bob_webcam", bobID, "camera", 1500, bobProf)
	require.NoError(t, err)
	assert.Equal(t, "bob_webcam", mBob.StreamID)

	assert.ElementsMatch(t, []string{"alice_screen", "bob_webcam"}, engine.ActiveStreams())

	// Test On-Demand Subscriptions:
	// Charlie and Dave want to watch Alice's screen.
	// Eve wants to watch Bob's webcam.
	// Frank is in the voice call but watches NOTHING.
	charlieID := makePeerID(30)
	daveID := makePeerID(40)
	eveID := makePeerID(50)
	frankID := makePeerID(60)

	charlieProf := PeerNodeProfile{PeerID: charlieID, UploadKbps: 10000, MaxRelayChildren: 3}
	daveProf := PeerNodeProfile{PeerID: daveID, UploadKbps: 500, IsMobile: true}
	eveProf := PeerNodeProfile{PeerID: eveID, UploadKbps: 20000, MaxRelayChildren: 4}
	_ = frankID

	// Charlie & Dave subscribe to Alice
	_, err = engine.Subscribe("alice_screen", charlieID, charlieProf)
	require.NoError(t, err)
	_, err = engine.Subscribe("alice_screen", daveID, daveProf)
	require.NoError(t, err)

	// Eve subscribes to Bob
	_, err = engine.Subscribe("bob_webcam", eveID, eveProf)
	require.NoError(t, err)

	// Verify Alice's stream distribution tree
	aliceSubscribers := engine.streams["alice_screen"].Subscribers
	assert.ElementsMatch(t, []types.PeerID{charlieID, daveID}, aliceSubscribers)
	// Frank and Eve should NOT be in Alice's stream
	assert.NotContains(t, aliceSubscribers, frankID)
	assert.NotContains(t, aliceSubscribers, eveID)

	// Downstream routing check for Alice
	aliceDownstream := engine.GetDownstream("alice_screen", aliceID)
	assert.NotEmpty(t, aliceDownstream)

	// Verify Bob's stream distribution tree
	bobSubscribers := engine.streams["bob_webcam"].Subscribers
	assert.ElementsMatch(t, []types.PeerID{eveID}, bobSubscribers)
	assert.NotContains(t, bobSubscribers, charlieID)
	assert.NotContains(t, bobSubscribers, daveID)

	// Test Unsubscribe and tree healing
	_, err = engine.Unsubscribe("alice_screen", charlieID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []types.PeerID{daveID}, engine.streams["alice_screen"].Subscribers)

	// Test Unregister Stream
	engine.UnregisterStream("bob_webcam")
	assert.Equal(t, []string{"alice_screen"}, engine.ActiveStreams())
}

func TestVoiceTree_EdgeCases(t *testing.T) {
	coordID := makePeerID(1)

	// Single coordinator call
	manifestSingle := BuildVoiceTopology("solo-call", 1, coordID, nil)
	assert.Equal(t, coordID, manifestSingle.Coordinator)
	assert.Empty(t, manifestSingle.ParentMap)

	// Failover with empty alive peers
	_, ok := ElectNewCoordinator([]types.PeerID{makePeerID(2)}, map[types.PeerID]bool{})
	assert.False(t, ok)

	// Failover with unlisted alive peer
	unlistedID := makePeerID(99)
	elected, ok := ElectNewCoordinator(nil, map[types.PeerID]bool{unlistedID: true})
	assert.True(t, ok)
	assert.Equal(t, unlistedID, elected)
}

func TestStreamTreeEngine_CoverageEdgeCases(t *testing.T) {
	engine := NewStreamTreeEngine()
	aliceID := makePeerID(1)
	bobID := makePeerID(2)
	charlieID := makePeerID(3)

	// Broadcaster with low capacity (only 1 child) to force daisy-chain relaying
	aliceProf := PeerNodeProfile{PeerID: aliceID, UploadKbps: 2000, MaxRelayChildren: 1}
	_, err := engine.RegisterStream("s1", aliceID, "screen", 2000, aliceProf)
	require.NoError(t, err)

	// Duplicate register error
	_, err = engine.RegisterStream("s1", aliceID, "screen", 2000, aliceProf)
	assert.ErrorIs(t, err, ErrStreamAlreadyExist)

	// Subscribe to non-existent stream
	_, err = engine.Subscribe("unknown", bobID, PeerNodeProfile{})
	assert.ErrorIs(t, err, ErrStreamNotFound)

	// Unsubscribe from non-existent stream
	_, err = engine.Unsubscribe("unknown", bobID)
	assert.ErrorIs(t, err, ErrStreamNotFound)

	// Unsubscribe non-existent peer from existent stream (no error, unchanged)
	_, err = engine.Unsubscribe("s1", bobID)
	require.NoError(t, err)

	// UpdatePeerProfile
	engine.UpdatePeerProfile(PeerNodeProfile{PeerID: bobID, UploadKbps: 15000})

	// Subscribe Bob (strong relay) and Charlie (leaf)
	bobProf := PeerNodeProfile{PeerID: bobID, UploadKbps: 10000, MaxRelayChildren: 2}
	charlieProf := PeerNodeProfile{PeerID: charlieID, UploadKbps: 500, IsMobile: true}

	_, err = engine.Subscribe("s1", bobID, bobProf)
	require.NoError(t, err)

	// Re-subscribe same peer returns existing without error
	_, err = engine.Subscribe("s1", bobID, bobProf)
	require.NoError(t, err)

	_, err = engine.Subscribe("s1", charlieID, charlieProf)
	require.NoError(t, err)

	// GetUpstream
	parent, ok := engine.GetUpstream("s1", charlieID)
	assert.True(t, ok)
	assert.NotEmpty(t, parent)

	_, ok = engine.GetUpstream("unknown", charlieID)
	assert.False(t, ok)

	// GetStreamManifest
	sm, ok := engine.GetStreamManifest("s1")
	assert.True(t, ok)
	assert.Equal(t, "s1", sm.StreamID)

	_, ok = engine.GetStreamManifest("unknown")
	assert.False(t, ok)

	// GetDownstream with non-existent stream and peer with no children
	assert.Nil(t, engine.GetDownstream("unknown", aliceID))
	assert.Nil(t, engine.GetDownstream("s1", charlieID)) // Charlie is a leaf
}

func BenchmarkVoiceTree_100Peers(b *testing.B) {
	coordID := makePeerID(1)
	var profiles []PeerNodeProfile
	for i := 1; i <= 100; i++ {
		id := makePeerID(byte(i))
		profiles = append(profiles, PeerNodeProfile{
			PeerID:           id,
			UploadKbps:       10000,
			MaxRelayChildren: 4,
			IsMobile:         i%3 == 0,
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildVoiceTopology("bench-room", uint64(i), coordID, profiles)
	}
}
