package tree

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertValidStreamTree checks that every subscriber is reachable from the broadcaster exactly
// once and that ParentMap and DistributionTree agree.
func assertValidStreamTree(t *testing.T, m *StreamManifest) {
	t.Helper()
	require.Len(t, m.ParentMap, len(m.Subscribers))
	edges := 0
	for parent, children := range m.DistributionTree {
		for _, c := range children {
			assert.Equal(t, parent, m.ParentMap[c])
			edges++
		}
	}
	assert.Equal(t, len(m.Subscribers), edges)
	for _, s := range m.Subscribers {
		node, hops := s, 0
		for node != m.Broadcaster {
			node = m.ParentMap[node]
			hops++
			require.LessOrEqual(t, hops, len(m.Subscribers), "cycle in stream tree")
		}
	}
}

func TestRelayCapacity_BoundedByUpload(t *testing.T) {
	// 30 Mbps upstream cannot relay four 25 Mbps 4K60 copies, whatever MaxRelayChildren says.
	p := PeerNodeProfile{UploadKbps: 30000, MaxRelayChildren: 4}
	assert.Equal(t, 1, RelayCapacity(p, 25000, false))
	assert.Equal(t, 3, RelayCapacity(p, 8000, false))
	assert.Equal(t, 4, RelayCapacity(p, 2500, false))
	assert.Equal(t, 0, RelayCapacity(PeerNodeProfile{UploadKbps: 20000, MaxRelayChildren: 4}, 25000, false))
	assert.Equal(t, 0, RelayCapacity(PeerNodeProfile{MaxRelayChildren: 4}, 8000, false), "unknown upload")
	assert.Equal(t, 0, RelayCapacity(PeerNodeProfile{UploadKbps: 90000, MaxRelayChildren: 4, IsMobile: true}, 8000, false))
	assert.Equal(t, 2, RelayCapacity(PeerNodeProfile{UploadKbps: 50000}, 25000, true))
	assert.Equal(t, DefaultBroadcasterFanout, RelayCapacity(PeerNodeProfile{}, 25000, true))
	assert.Equal(t, MaxStreamFanout, RelayCapacity(PeerNodeProfile{UploadKbps: 1_000_000}, 800, true))
}

func TestStreamTree_4KCapacityNeverExceeded(t *testing.T) {
	e := NewStreamTreeEngine()
	b := makePeerID(1)
	_, err := e.RegisterStream("s", b, "screen", 25000, PeerNodeProfile{UploadKbps: 50000})
	require.NoError(t, err)

	var subs []PeerNodeProfile
	for i := 0; i < 12; i++ {
		subs = append(subs, PeerNodeProfile{PeerID: makePeerID(byte(10 + i)), UploadKbps: 60000, MaxRelayChildren: 4})
	}
	var m *StreamManifest
	for _, s := range subs {
		m, err = e.Subscribe("s", s.PeerID, s)
		require.NoError(t, err)
	}
	assertValidStreamTree(t, m)
	assert.Empty(t, m.Overflow)
	assert.LessOrEqual(t, len(m.DistributionTree[b]), 2)
	for _, s := range subs {
		assert.LessOrEqual(t, len(m.DistributionTree[s.PeerID]), 2, "60 Mbps relays two 25 Mbps copies at most")
	}
}

func TestStreamTree_IncrementalJoinKeepsExistingParents(t *testing.T) {
	e := NewStreamTreeEngine()
	b := makePeerID(1)
	_, _ = e.RegisterStream("s", b, "screen", 5000, PeerNodeProfile{UploadKbps: 10000})
	relay := PeerNodeProfile{PeerID: makePeerID(2), UploadKbps: 50000, MaxRelayChildren: 4}
	_, _ = e.Subscribe("s", relay.PeerID, relay)
	m, _ := e.Subscribe("s", makePeerID(3), PeerNodeProfile{IsMobile: true})
	before := m.ParentMap

	for i := 4; i < 9; i++ {
		m, _ = e.Subscribe("s", makePeerID(byte(i)), PeerNodeProfile{IsMobile: true})
	}
	for peer, parent := range before {
		assert.Equal(t, parent, m.ParentMap[peer], "joins must not move existing viewers")
	}
	assertValidStreamTree(t, m)
}

func TestStreamTree_RelayDepartureRehomesOrphans(t *testing.T) {
	e := NewStreamTreeEngine()
	b := makePeerID(1)
	_, _ = e.RegisterStream("s", b, "screen", 5000, PeerNodeProfile{UploadKbps: 5000})
	relayA := PeerNodeProfile{PeerID: makePeerID(2), UploadKbps: 20000, MaxRelayChildren: 4}
	relayB := PeerNodeProfile{PeerID: makePeerID(3), UploadKbps: 20000, MaxRelayChildren: 4}
	_, _ = e.Subscribe("s", relayA.PeerID, relayA)
	_, _ = e.Subscribe("s", relayB.PeerID, relayB)
	for i := 4; i < 9; i++ {
		_, _ = e.Subscribe("s", makePeerID(byte(i)), PeerNodeProfile{IsMobile: true})
	}
	m, _ := e.GetStreamManifest("s")
	feeder := m.ParentMap[makePeerID(4)]
	require.NotEqual(t, b, feeder)

	changed, closed := e.RemovePeer(feeder)
	require.Len(t, changed, 1)
	assert.Empty(t, closed)
	m = changed[0]
	assertValidStreamTree(t, m)
	_, stillThere := m.ParentMap[feeder]
	assert.False(t, stillThere)
	assert.Greater(t, m.Epoch, uint64(1))

	_, closed = e.RemovePeer(b)
	assert.Equal(t, []string{"s"}, closed)
	assert.Empty(t, e.ActiveStreams())
}

func TestStreamTree_OverflowIsReported(t *testing.T) {
	e := NewStreamTreeEngine()
	b := makePeerID(1)
	_, _ = e.RegisterStream("s", b, "screen", 25000, PeerNodeProfile{UploadKbps: 25000})
	var m *StreamManifest
	for i := 2; i < 5; i++ {
		m, _ = e.Subscribe("s", makePeerID(byte(i)), PeerNodeProfile{IsMobile: true})
	}
	assertValidStreamTree(t, m)
	assert.Len(t, m.Overflow, 2)

	m, _ = e.Unsubscribe("s", m.Overflow[0])
	assert.Len(t, m.Overflow, 1)
}

func TestStreamTree_ApplyManifestAndQualityRebuild(t *testing.T) {
	coord := NewStreamTreeEngine()
	b := makePeerID(1)
	_, _ = coord.RegisterStream("s", b, "screen", 2500, PeerNodeProfile{UploadKbps: 30000})
	relay := PeerNodeProfile{PeerID: makePeerID(2), UploadKbps: 30000, MaxRelayChildren: 8}
	_, _ = coord.Subscribe("s", relay.PeerID, relay)
	var m *StreamManifest
	for i := 3; i < 20; i++ {
		m, _ = coord.Subscribe("s", makePeerID(byte(i)), PeerNodeProfile{IsMobile: true})
	}

	replica := NewStreamTreeEngine()
	require.True(t, replica.ApplyManifest(m))
	assert.False(t, replica.ApplyManifest(m), "same epoch is ignored")
	stale := *m
	stale.Epoch--
	assert.False(t, replica.ApplyManifest(&stale))
	assert.ElementsMatch(t, coord.GetDownstream("s", relay.PeerID), replica.GetDownstream("s", relay.PeerID))

	// Mutating a returned manifest must not corrupt engine state.
	m.DistributionTree[b] = nil
	assert.NotEmpty(t, coord.GetDownstream("s", b))

	// Raising quality shrinks capacities and forces a consistent rebuild.
	q4k, _ := QualityByID("2160p60")
	require.NoError(t, coord.SetStreamQuality("s", q4k))
	m, _ = coord.GetStreamManifest("s")
	assertValidStreamTree(t, m)
	assert.LessOrEqual(t, len(m.DistributionTree[relay.PeerID]), 1)
	assert.True(t, replica.ApplyManifest(m))
}

func TestVoiceTree_StandbyIndependentOfPrimary(t *testing.T) {
	coord := makePeerID(1)
	var profiles []PeerNodeProfile
	profiles = append(profiles, PeerNodeProfile{PeerID: coord, MaxRelayChildren: 2})
	for i := 2; i < 30; i++ {
		profiles = append(profiles, PeerNodeProfile{PeerID: makePeerID(byte(i)), UploadKbps: 5000 + i, MaxRelayChildren: 2, IsMobile: i%4 == 0})
	}
	m := BuildVoiceTopology("c", 1, coord, profiles)
	require.Len(t, m.ParentMap, 28)
	assert.Len(t, m.Profiles, 29)

	sameAsPrimary := 0
	for child, ass := range m.ParentMap {
		assert.NotEqual(t, child, ass.StandbyParent)
		assert.False(t, isDescendant(ass.StandbyParent, child, m.ParentMap), "standby must not sit below the child")
		if ass.PrimaryParent == coord {
			if ass.StandbyParent == coord {
				sameAsPrimary++ // only the very first child has no alternative yet
			}
			continue
		}
		assert.NotEqual(t, ass.PrimaryParent, ass.StandbyParent, "standby must differ from primary for %s", child)
		assert.False(t, isDescendant(ass.StandbyParent, ass.PrimaryParent, m.ParentMap), "standby must survive the primary's death")
	}
	assert.LessOrEqual(t, sameAsPrimary, 1)
	// Shallow: with fanout 2 and 28 peers the depth stays logarithmic.
	maxDepth := 0
	for child := range m.ParentMap {
		d, node := 0, child
		for node != coord {
			node = m.ParentMap[node].PrimaryParent
			d++
		}
		maxDepth = max(maxDepth, d)
	}
	assert.LessOrEqual(t, maxDepth, 5)
	assert.Equal(t, m.Members()[0], coord)
	assert.Len(t, m.Members(), 29)
}

func TestVoiceTree_Deterministic(t *testing.T) {
	coord := makePeerID(1)
	var profiles []PeerNodeProfile
	for i := 2; i < 12; i++ {
		profiles = append(profiles, PeerNodeProfile{PeerID: makePeerID(byte(i)), UploadKbps: 1000, MaxRelayChildren: 2})
	}
	a := BuildVoiceTopology("c", 1, coord, profiles)
	rev := make([]PeerNodeProfile, len(profiles))
	for i, p := range profiles {
		rev[len(profiles)-1-i] = p
	}
	b := BuildVoiceTopology("c", 1, coord, rev)
	assert.Equal(t, a.ParentMap, b.ParentMap)
	assert.Equal(t, a.StandbyCoordinators, b.StandbyCoordinators)
	_ = types.PeerID{}
}

func TestStreamTree_LateRelaySplicesAboveEarlyLeaves(t *testing.T) {
	e := NewStreamTreeEngine()
	b := makePeerID(1)
	_, _ = e.RegisterStream("s", b, "screen", 2500, PeerNodeProfile{UploadKbps: 5000}) // two slots
	_, _ = e.Subscribe("s", makePeerID(2), PeerNodeProfile{IsMobile: true})
	_, _ = e.Subscribe("s", makePeerID(3), PeerNodeProfile{IsMobile: true})

	var m *StreamManifest
	for i := 4; i < 9; i++ {
		m, _ = e.Subscribe("s", makePeerID(byte(i)), PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2})
	}
	assertValidStreamTree(t, m)
	assert.Empty(t, m.Overflow, "relays joining late must still fit")
	assert.LessOrEqual(t, len(m.DistributionTree[b]), 2)
	for _, p := range m.Subscribers {
		assert.LessOrEqual(t, len(m.DistributionTree[p]), RelayCapacity(m.Profiles[p], 2500, false))
	}
}

func TestStreamTree_ReparentAvoidsRelayAndKeepsSubtree(t *testing.T) {
	e := NewStreamTreeEngine()
	b := makePeerID(1)
	_, _ = e.RegisterStream("s", b, "screen", 2500, PeerNodeProfile{UploadKbps: 5000}) // 2 slots
	relayA := PeerNodeProfile{PeerID: makePeerID(2), UploadKbps: 20000, MaxRelayChildren: 3}
	relayB := PeerNodeProfile{PeerID: makePeerID(3), UploadKbps: 20000, MaxRelayChildren: 3}
	_, _ = e.Subscribe("s", relayA.PeerID, relayA)
	_, _ = e.Subscribe("s", relayB.PeerID, relayB)
	mid := PeerNodeProfile{PeerID: makePeerID(4), UploadKbps: 20000, MaxRelayChildren: 1}
	_, _ = e.Subscribe("s", mid.PeerID, mid)
	leaf := makePeerID(5)
	m, _ := e.Subscribe("s", leaf, PeerNodeProfile{IsMobile: true})
	feeder := m.ParentMap[mid.PeerID]
	require.NotEqual(t, b, feeder)

	m, err := e.Reparent("s", mid.PeerID, feeder)
	require.NoError(t, err)
	assertValidStreamTree(t, m)
	assert.NotEqual(t, feeder, m.ParentMap[mid.PeerID])
	assert.Contains(t, m.Avoid[mid.PeerID], feeder)

	// the avoid list survives a full rebuild
	q, _ := QualityByID("720p60")
	require.NoError(t, e.SetStreamQuality("s", q))
	m, _ = e.GetStreamManifest("s")
	assertValidStreamTree(t, m)
	assert.NotEqual(t, feeder, m.ParentMap[mid.PeerID])

	// a node can never be re-homed below its own subtree
	if kids := m.DistributionTree[mid.PeerID]; len(kids) > 0 {
		m, err = e.Reparent("s", mid.PeerID, m.ParentMap[mid.PeerID])
		require.NoError(t, err)
		assertValidStreamTree(t, m)
	}

	_, err = e.Reparent("s", mid.PeerID, b)
	assert.ErrorIs(t, err, ErrCannotAvoidSource)
	_, err = e.Reparent("s", makePeerID(99), feeder)
	assert.ErrorIs(t, err, ErrNotSubscribed)

	changed, _ := e.RemovePeer(feeder)
	for _, c := range changed {
		assert.NotContains(t, c.Avoid[mid.PeerID], feeder, "departed peers leave avoid lists")
	}
}
