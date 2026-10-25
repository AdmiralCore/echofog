package call

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSigner signs call signals with a real ed25519 identity, like the swarm does.
type testSigner struct {
	priv ed25519.PrivateKey
	pub  types.PeerPublicKey
}

func newTestSigner() *testSigner {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := &testSigner{priv: priv}
	copy(s.pub[:], pub)
	return s
}

func (s *testSigner) CreateSignature(data []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, data), nil
}
func (s *testSigner) GetMyPubKey() types.PeerPublicKey { return s.pub }
func (s *testSigner) id() types.PeerID                 { return types.PeerPubKeyToID(s.pub) }

// mesh is an in-memory network: every frame is delivered asynchronously, nodes can be cut off.
type mesh struct {
	mu    sync.Mutex
	nodes map[types.PeerID]*CallManager
	dead  map[types.PeerID]bool
	sent  atomic.Uint64
	drop  func(t net_types.MessageType, payload []byte) bool // simulated packet loss
}

func newMesh() *mesh {
	return &mesh{nodes: make(map[types.PeerID]*CallManager), dead: make(map[types.PeerID]bool)}
}

type meshPort struct {
	m    *mesh
	self types.PeerID
}

func (p meshPort) SendRawFrame(to types.PeerID, t net_types.MessageType, payload []byte) error {
	return p.m.deliver(p.self, to, t, payload)
}

func (p meshPort) SendUnreliableFrame(to types.PeerID, t net_types.MessageType, payload []byte) error {
	return p.m.deliver(p.self, to, t, payload)
}

func (m *mesh) deliver(from, to types.PeerID, t net_types.MessageType, payload []byte) error {
	m.mu.Lock()
	target := m.nodes[to]
	cut := m.dead[from] || m.dead[to]
	drop := m.drop
	m.mu.Unlock()
	if target == nil {
		return fmt.Errorf("peer %s not connected", to)
	}
	if cut || (drop != nil && drop(t, payload)) {
		return nil // a crashed peer or a lossy link silently swallows traffic
	}
	m.sent.Add(1)
	data := append([]byte(nil), payload...)
	go func() {
		switch t {
		case net_types.TypeCallSignaling:
			_ = target.handleIncomingSignaling(data, from)
		case net_types.TypeCallMediaAudio:
			_ = target.handleIncomingAudio(data, from)
		case net_types.TypeCallMediaVideo:
			_ = target.handleIncomingVideo(data, from)
		}
	}()
	return nil
}

func (m *mesh) kill(id types.PeerID) {
	m.mu.Lock()
	m.dead[id] = true
	m.mu.Unlock()
}

// mediaLog records delivered units per (kind, broadcaster, stream).
type mediaLog struct {
	mu    sync.Mutex
	units map[string]map[uint32]bool
}

func (l *mediaLog) tap(p *media.MediaPacket) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := fmt.Sprintf("%d/%s/%s", p.Kind, p.Broadcaster, p.StreamID)
	if l.units[k] == nil {
		l.units[k] = make(map[uint32]bool)
	}
	// payload carries the unit index so content corruption is caught, not only loss
	if len(p.Payload) >= 4 {
		l.units[k][binary.BigEndian.Uint32(p.Payload)] = true
	}
}

func (l *mediaLog) count(kind media.FrameKind, b types.PeerID, stream string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.units[fmt.Sprintf("%d/%s/%s", kind, b, stream)])
}

type testNode struct {
	cm  *CallManager
	id  types.PeerID
	log *mediaLog
}

func newRoom(t *testing.T, m *mesh, n int) ([]*testNode, []byte) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	nodes := make([]*testNode, n)
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
	coord := nodes[0].id
	for i, nd := range nodes {
		prof := tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}
		if i%3 == 2 {
			prof = tree.PeerNodeProfile{UploadKbps: 3000, IsMobile: true}
		}
		require.NoError(t, nd.cm.JoinRoom("room-1", coord, key, prof))
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			_ = nd.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 5*time.Second, 20*time.Millisecond, "room did not form")
	return nodes, key
}

// roomConverged: every live node joined and agrees on coordinator, epoch and membership.
func roomConverged(nodes []*testNode, dead map[types.PeerID]bool) bool {
	var ref *RoomSnapshot
	live := 0
	for _, nd := range nodes {
		if dead[nd.id] {
			continue
		}
		live++
		s, ok := nd.cm.Snapshot()
		if !ok || !s.Joined {
			return false
		}
		if ref == nil {
			ref = &s
			continue
		}
		if s.Coordinator != ref.Coordinator || s.Epoch != ref.Epoch || len(s.Members) != len(ref.Members) {
			return false
		}
	}
	return ref != nil && len(ref.Members) == live
}

// dumpRoom is evaluated lazily by fmt when a convergence assertion fails.
type roomDump struct {
	nodes []*testNode
	dead  map[types.PeerID]bool
}

func dumpRoom(nodes []*testNode, dead map[types.PeerID]bool) roomDump { return roomDump{nodes, dead} }

func (d roomDump) String() string {
	out := "\n"
	for i, nd := range d.nodes {
		s, ok := nd.cm.Snapshot()
		out += fmt.Sprintf("node %d %s dead=%v ok=%v coord=%s isCoord=%v epoch=%d joined=%v members=%d\n",
			i, nd.id.String()[:8], d.dead[nd.id], ok, s.Coordinator.String()[:8], s.IsCoordinator, s.Epoch, s.Joined, len(s.Members))
	}
	return out
}

func unit(i uint32, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, i)
	return b
}

func TestRoom_FormsAndAgreesOnTopology(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 7)

	s0, _ := nodes[0].cm.Snapshot()
	assert.True(t, s0.IsCoordinator)
	assert.Len(t, s0.Members, 7)
	for _, nd := range nodes[1:] {
		s, _ := nd.cm.Snapshot()
		assert.False(t, s.IsCoordinator)
		assert.Equal(t, nodes[0].id, s.Coordinator)
	}
}

func TestRoom_TwoStreamsReachEveryViewer(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 8)
	a, b := nodes[1], nodes[4] // two broadcasters, neither is the coordinator

	q1080, _ := tree.QualityByID("1080p60")
	q720, _ := tree.QualityByID("720p30")
	require.NoError(t, a.cm.StartStream("a-screen", "screen", q1080))
	require.NoError(t, b.cm.StartStream("b-cam", "camera", q720))

	for _, nd := range nodes {
		if nd != a {
			require.NoError(t, nd.cm.WatchStream("a-screen"))
		}
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("b-cam"))
		}
	}
	// wait until every replica of both trees covers all 7 viewers
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			for _, s := range []string{"a-screen", "b-cam"} {
				sm, ok := nd.cm.StreamTree(s)
				if !ok || len(sm.ParentMap) != 7 {
					return false
				}
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)

	// All replicas must be identical: that is what makes relaying consistent.
	ref, _ := nodes[0].cm.StreamTree("a-screen")
	for _, nd := range nodes[1:] {
		sm, _ := nd.cm.StreamTree("a-screen")
		assert.Equal(t, ref.ParentMap, sm.ParentMap)
		assert.Equal(t, ref.Epoch, sm.Epoch)
	}

	// paced like real media (a frame every few milliseconds), not all at once: an unpaced burst
	// overflows relay queues, and a unit lost at the very end of a stream has no later unit to
	// reveal the gap, so nothing could ever recover it
	const units = 300
	for i := uint32(0); i < units; i++ {
		require.NoError(t, a.cm.PublishVideo("a-screen", unit(i, 2400))) // 2 fragments each
		require.NoError(t, b.cm.PublishVideo("b-cam", unit(i, 900)))
		if i%10 == 0 {
			require.NoError(t, nodes[2].cm.PublishAudio(unit(i/10, 160)))
			time.Sleep(5 * time.Millisecond)
		}
	}

	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			if nd != a && nd.log.count(media.FrameVideo, a.id, "a-screen") < units {
				return false
			}
			if nd != b && nd.log.count(media.FrameVideo, b.id, "b-cam") < units {
				return false
			}
			if nd != nodes[2] && nd.log.count(media.FrameAudio, nodes[2].id, "voice") < units/10 {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond)

	for _, nd := range nodes {
		st := nd.cm.MediaStats()
		assert.Zero(t, st.DecryptFailures)
		assert.Zero(t, st.Malformed)
	}
	assert.Equal(t, a.id, nodes[3].cm.activeRoom.announces["a-screen"].Broadcaster)
}

func TestRoom_RelayCrashRehomesViewers(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 8)
	b := nodes[1]
	b.cm.SetUploadCapacity(5000) // can feed only two 720p viewers itself: the rest must be relayed
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool {
		sm, ok := nodes[0].cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 7
	}, 5*time.Second, 20*time.Millisecond)
	sm0, _ := nodes[0].cm.StreamTree("s")
	assert.LessOrEqual(t, len(sm0.DistributionTree[b.id]), 2)

	// Crash a viewer that relays to others.
	sm, _ := nodes[0].cm.StreamTree("s")
	var relay *testNode
	for _, nd := range nodes {
		if nd != b && len(sm.DistributionTree[nd.id]) > 0 && nd != nodes[0] {
			relay = nd
		}
	}
	require.NotNil(t, relay, "tree should use a viewer as relay")
	m.kill(relay.id)
	dead := map[types.PeerID]bool{relay.id: true}

	if !assert.Eventually(t, func() bool { return roomConverged(nodes, dead) }, 6*time.Second, 50*time.Millisecond) {
		t.Fatal(dumpRoom(nodes, dead).String())
	}
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			if dead[nd.id] {
				continue
			}
			sm, ok := nd.cm.StreamTree("s")
			if !ok || len(sm.ParentMap) != 6 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)

	for i := uint32(0); i < 100; i++ {
		require.NoError(t, b.cm.PublishVideo("s", unit(i, 1200)))
	}
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			if nd != b && !dead[nd.id] && nd.log.count(media.FrameVideo, b.id, "s") < 100 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)
}

func TestRoom_CoordinatorCrashElectsAndStreamsSurvive(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 7)
	var changed atomic.Int32
	for _, nd := range nodes[1:] {
		nd.cm.OnCoordinatorChanged = func(types.PeerID, uint64) { changed.Add(1) }
	}

	b := nodes[2]
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes[1:] {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool {
		sm, ok := nodes[1].cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 5
	}, 5*time.Second, 20*time.Millisecond)

	old := nodes[0].id
	m.kill(old)
	dead := map[types.PeerID]bool{old: true}
	require.Eventually(t, func() bool { return roomConverged(nodes, dead) }, 10*time.Second, 50*time.Millisecond, "no new coordinator")

	s, _ := nodes[1].cm.Snapshot()
	assert.NotEqual(t, old, s.Coordinator)
	assert.Len(t, s.Members, 6)
	coordinators := 0
	for _, nd := range nodes[1:] {
		if sn, _ := nd.cm.Snapshot(); sn.IsCoordinator {
			coordinators++
		}
	}
	assert.Equal(t, 1, coordinators, "exactly one coordinator after failover")
	assert.Positive(t, changed.Load())

	require.Eventually(t, func() bool {
		for _, nd := range nodes[1:] {
			sm, ok := nd.cm.StreamTree("s")
			if !ok || len(sm.ParentMap) != 5 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "stream lost in failover")

	for i := uint32(0); i < 50; i++ {
		require.NoError(t, b.cm.PublishVideo("s", unit(i, 700)))
	}
	require.Eventually(t, func() bool {
		for _, nd := range nodes[1:] {
			if nd != b && nd.log.count(media.FrameVideo, b.id, "s") < 50 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)
}

func TestRoom_KeyframeRequestReachesBroadcaster(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 6)
	b := nodes[3]
	var served atomic.Int32
	b.cm.OnKeyframeRequest = func(string) { served.Add(1) }

	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	// joining viewers ask for a keyframe as soon as they are attached
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool { return served.Load() > 0 }, 5*time.Second, 20*time.Millisecond)

	// a gap in the stream triggers another request after the rate limit window
	time.Sleep(keyframeServeInterval + 50*time.Millisecond)
	before := served.Load()
	m.mu.Lock()
	m.drop = func(t net_types.MessageType, payload []byte) bool {
		p, err := media.Decode(payload)
		return t == net_types.TypeCallMediaVideo && err == nil && p.Seq == 2
	}
	m.mu.Unlock()
	require.NoError(t, b.cm.PublishVideo("s", unit(0, 100))) // seq 1
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			if nd != b && nd.log.count(media.FrameVideo, b.id, "s") < 1 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, b.cm.PublishVideo("s", unit(1, 100))) // seq 2, lost on every link
	require.NoError(t, b.cm.PublishVideo("s", unit(2, 100))) // seq 3 reveals the gap
	require.Eventually(t, func() bool { return served.Load() > before }, 5*time.Second, 20*time.Millisecond)
	assert.Positive(t, b.cm.KeyframesServed())
}

// One fragment lost on one tree link is resent by the parent after a NACK: every viewer gets
// every unit and nobody asks the broadcaster for a keyframe.
func TestRoom_LostFragmentRecoveredByNack(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 6)
	b := nodes[3]
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	allGot := func(n int) func() bool {
		return func() bool {
			for _, nd := range nodes {
				if nd != b && nd.log.count(media.FrameVideo, b.id, "s") < n {
					return false
				}
			}
			return true
		}
	}
	// every node holds the full distribution tree before anything is sent
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			sm, ok := nd.cm.StreamTree("s")
			if !ok || len(sm.ParentMap) != len(nodes)-1 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, b.cm.PublishVideo("s", unit(0, 1500))) // seq 1
	require.Eventually(t, allGot(1), 5*time.Second, 20*time.Millisecond)
	time.Sleep(keyframeMinInterval) // let the join-time keyframe requests settle

	var dropped atomic.Bool
	var keyframeRequests, nackSignals atomic.Int32
	m.mu.Lock()
	m.drop = func(t net_types.MessageType, payload []byte) bool {
		switch t {
		case net_types.TypeCallSignaling:
			if env, err := signaling.DecodeSignal(payload); err == nil {
				switch env.Action {
				case signaling.ActionKeyframeRequest:
					keyframeRequests.Add(1)
				case signaling.ActionNack:
					nackSignals.Add(1)
				}
			}
		case net_types.TypeCallMediaVideo:
			p, err := media.Decode(payload)
			// the first copy of fragment 1 of seq 2 is lost, on whichever link carries it first
			return err == nil && p.Seq == 2 && p.FragIdx == 1 && dropped.CompareAndSwap(false, true)
		}
		return false
	}
	m.mu.Unlock()

	require.NoError(t, b.cm.PublishVideo("s", unit(1, 1500))) // seq 2, two fragments
	require.NoError(t, b.cm.PublishVideo("s", unit(2, 1500))) // seq 3 reveals the gap
	require.Eventually(t, allGot(3), 5*time.Second, 20*time.Millisecond)
	require.True(t, dropped.Load())

	time.Sleep(300 * time.Millisecond) // past the 200 ms reorder window that would ask for a keyframe
	assert.Positive(t, nackSignals.Load())
	assert.Zero(t, keyframeRequests.Load(), "the NACK recovered the unit, no keyframe needed")
	var retransmitted uint64
	for _, nd := range nodes {
		retransmitted += nd.cm.MediaStats().Retransmitted
	}
	assert.Positive(t, retransmitted)
}

func TestRoom_RejectsForgedAndUnauthorizedSignals(t *testing.T) {
	m := newMesh()
	nodes, key := newRoom(t, m, 4)
	coord, victim, attacker := nodes[0], nodes[1], nodes[2]

	// 1. A member forging a tree manifest in the coordinator's name: signature check fails.
	forged := tree.BuildVoiceTopology("room-1", 999, coord.id, nil)
	env := signaling.CallSignalEnvelope{
		Action: signaling.ActionTreeManifest, CallID: "room-1", Sender: coord.id,
		SenderPubKey: newTestSigner().pub, Epoch: 999,
	}
	env.Payload, _ = json.Marshal(forged)
	raw, _ := json.Marshal(env)
	assert.Error(t, victim.cm.handleIncomingSignaling(raw, attacker.id))
	s, _ := victim.cm.Snapshot()
	assert.NotEqual(t, uint64(999), s.Epoch)

	// 2. A correctly signed unicast relayed by someone else is a spoof.
	signer := newTestSigner()
	raw, _ = signaling.EncodeSignedSignal(signer, signaling.ActionStreamSubscribe, "room-1", signer.id(), 1, nil)
	assert.ErrorIs(t, coord.cm.handleIncomingSignaling(raw, attacker.id), ErrSpoofedSender)

	// 3. An outsider with a valid identity but no room key cannot join.
	outsider := newTestSigner()
	ocm := newCallManager(meshPort{m, outsider.id()}, outsider, outsider.id())
	m.mu.Lock()
	m.nodes[outsider.id()] = ocm
	m.mu.Unlock()
	wrongKey := make([]byte, 32)
	require.NoError(t, ocm.JoinRoom("room-1", coord.id, wrongKey, tree.PeerNodeProfile{UploadKbps: 1000}))
	defer ocm.LeaveRoom()
	time.Sleep(1500 * time.Millisecond)
	s, _ = coord.cm.Snapshot()
	assert.Len(t, s.Members, 4, "outsider must not be admitted")

	// 4. A member cannot announce someone else's election.
	raw, _ = signaling.EncodeSignedSignal(attacker.cm.signer, signaling.ActionCoordinatorElection, "room-1", attacker.id, 1000, victim.id)
	require.NoError(t, victim.cm.handleIncomingSignaling(raw, attacker.id))
	s, _ = victim.cm.Snapshot()
	assert.Equal(t, coord.id, s.Coordinator)
	_ = key
}

func TestCallManager_EdgeCases(t *testing.T) {
	var id types.PeerID
	id[0] = 0x99
	cm := newCallManager(nil, nil, id)

	assert.ErrorIs(t, cm.StartScreenShare("stream1", "screen", 1000), ErrNotInCall)
	assert.ErrorIs(t, cm.StopScreenShare("stream1"), ErrNotInCall)
	assert.ErrorIs(t, cm.WatchStream("stream1"), ErrNotInCall)
	assert.ErrorIs(t, cm.UnwatchStream("stream1"), ErrNotInCall)
	assert.ErrorIs(t, cm.PublishAudio([]byte{1}), ErrNotInCall)

	assert.ErrorIs(t, cm.JoinRoom("r1", id, nil, tree.PeerNodeProfile{}), ErrInvalidRoomKey)
	key := make([]byte, 32)
	require.NoError(t, cm.JoinRoom("r1", id, key, tree.PeerNodeProfile{UploadKbps: 6000}))
	assert.ErrorIs(t, cm.JoinRoom("r1", id, key, tree.PeerNodeProfile{}), ErrCallAlreadyActive)

	q4k, _ := tree.QualityByID("2160p60")
	assert.ErrorIs(t, cm.StartStream("s", "screen", q4k), tree.ErrInsufficientUpload)
	assert.Error(t, cm.StartStream("voice", "screen", tree.CustomQuality(100)), "reserved id")
	assert.ErrorIs(t, cm.PublishVideo("not-mine", []byte{1}), ErrNoLocalStream)
	assert.ErrorIs(t, cm.UpdateStreamQuality("not-mine", q4k), ErrNoLocalStream)
	assert.Equal(t, "1080p30", tree.BestQualityFor(6000).ID)

	assert.Equal(t, "CallManager", cm.Name())
	require.NoError(t, cm.Stop())
	_, ok := cm.Snapshot()
	assert.False(t, ok)
}

func TestDeriveRoom(t *testing.T) {
	inv := NewInviteSecret()
	assert.Len(t, inv, len("call_")+64)
	id1, k1 := DeriveRoom(inv)
	id2, k2 := DeriveRoom(inv)
	assert.Equal(t, id1, id2)
	assert.Equal(t, k1, k2)
	assert.Len(t, k1, 32)
	assert.NotContains(t, id1, inv[5:15], "room id must not leak the secret")
	_, k3 := DeriveRoom(NewInviteSecret())
	assert.False(t, bytes.Equal(k1, k3))
}

func TestRoom_RelayCrashHealsWithinASecond(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 8)
	b := nodes[1]
	b.cm.SetUploadCapacity(5000)
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool {
		sm, ok := nodes[0].cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 7
	}, 5*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond) // links exchange pulses, nobody is in the grace period

	sm, _ := nodes[0].cm.StreamTree("s")
	var relay *testNode
	var orphans []types.PeerID
	for _, nd := range nodes {
		if nd != b && nd != nodes[0] && len(sm.DistributionTree[nd.id]) > 0 {
			relay, orphans = nd, sm.DistributionTree[nd.id]
			break
		}
	}
	require.NotNil(t, relay)
	m.kill(relay.id)
	crash := time.Now()

	// Orphans must be re-homed long before the 2.5 s member timeout would remove the relay.
	require.Eventually(t, func() bool {
		cur, ok := nodes[0].cm.StreamTree("s")
		if !ok {
			return false
		}
		for _, o := range orphans {
			if p, attached := cur.ParentMap[o]; !attached || p == relay.id {
				return false
			}
		}
		return true
	}, 2*time.Second, 10*time.Millisecond)
	healed := time.Since(crash)
	t.Logf("orphans re-homed %v after the relay crashed", healed)
	assert.Less(t, healed, 1500*time.Millisecond)
}

func TestRoom_UserCanSwitchRelay(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 7)
	b := nodes[1]
	b.cm.SetUploadCapacity(2500) // one direct viewer: everybody else is relayed
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool {
		sm, ok := nodes[0].cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 6
	}, 5*time.Second, 20*time.Millisecond)

	sm, _ := nodes[0].cm.StreamTree("s")
	var viewer *testNode
	for _, nd := range nodes {
		if p, ok := sm.ParentMap[nd.id]; ok && p != b.id {
			viewer = nd
			break
		}
	}
	require.NotNil(t, viewer)
	old := sm.ParentMap[viewer.id]
	require.NoError(t, viewer.cm.RequestRelayChange("s"))
	require.Eventually(t, func() bool {
		cur, _ := viewer.cm.StreamTree("s")
		return cur != nil && cur.ParentMap[viewer.id] != old && containsID(cur.Avoid[viewer.id], old)
	}, 5*time.Second, 20*time.Millisecond)
	assert.ErrorIs(t, b.cm.RequestRelayChange("s"), ErrNotWatching, "the broadcaster has no relay")
}

func containsID(list []types.PeerID, p types.PeerID) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

func TestRoom_SpamReportsKickAndBan(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 6)
	spammer := nodes[4]
	var kicked atomic.Value
	spammer.cm.OnKicked = func(reason string) { kicked.Store(reason) }

	assert.ErrorIs(t, nodes[1].cm.ReportMember(nodes[1].id, "x"), ErrSelfReport)

	// one report is not enough
	require.NoError(t, nodes[1].cm.ReportMember(spammer.id, "garbage video"))
	time.Sleep(300 * time.Millisecond)
	s, _ := nodes[0].cm.Snapshot()
	assert.Len(t, s.Members, 6)

	// reports from two distinct members (a third of five others, at least two) remove it
	require.NoError(t, nodes[2].cm.ReportMember(spammer.id, "garbage video"))
	require.Eventually(t, func() bool {
		s, _ := nodes[0].cm.Snapshot()
		return len(s.Members) == 5 && kicked.Load() != nil
	}, 5*time.Second, 20*time.Millisecond)
	_, inCall := spammer.cm.Snapshot()
	require.Eventually(t, func() bool { _, in := spammer.cm.Snapshot(); return !in }, 2*time.Second, 20*time.Millisecond)
	_ = inCall

	// the ban holds: rejoining with the right key is refused
	key := nodes[0].cm.activeRoom.RoomKey
	require.NoError(t, spammer.cm.JoinRoom("room-1", nodes[0].id, key, tree.PeerNodeProfile{UploadKbps: 1000}))
	time.Sleep(1500 * time.Millisecond)
	s, _ = nodes[0].cm.Snapshot()
	assert.Len(t, s.Members, 5)
}

// The sound of a shared screen reaches the people watching it, relayed down the stream's tree,
// and nobody else.
func TestRoom_ScreenSoundReachesOnlyViewers(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 6)
	a := nodes[1]
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, a.cm.StartStream("a-screen", "screen", q))
	viewers := nodes[2:5] // nodes[0] and nodes[5] do not watch
	for _, nd := range viewers {
		require.NoError(t, nd.cm.WatchStream("a-screen"))
	}
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			sm, ok := nd.cm.StreamTree("a-screen")
			if !ok || len(sm.ParentMap) != len(viewers) {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)

	const units = 50
	for i := uint32(0); i < units; i++ {
		require.NoError(t, a.cm.PublishStreamAudio("a-screen", unit(i, 300)))
		time.Sleep(2 * time.Millisecond)
	}
	assert.ErrorIs(t, nodes[2].cm.PublishStreamAudio("a-screen", unit(0, 10)), ErrNoLocalStream, "only the broadcaster sends a stream's sound")

	require.Eventually(t, func() bool {
		for _, nd := range viewers {
			if nd.log.count(media.FrameStreamAudio, a.id, "a-screen") < units {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)
	for _, nd := range []*testNode{nodes[0], nodes[5]} {
		assert.Zero(t, nd.log.count(media.FrameStreamAudio, a.id, "a-screen"), "not watching: no sound")
	}
	for _, nd := range nodes {
		assert.Zero(t, nd.cm.MediaStats().Malformed)
	}
}

// parentLostCounter counts ParentLost signals sent by one node, keyed by the reported parent.
func parentLostCounter(m *mesh, from types.PeerID) func() map[types.PeerID]int {
	var mu sync.Mutex
	got := make(map[types.PeerID]int)
	m.mu.Lock()
	m.drop = func(t net_types.MessageType, payload []byte) bool {
		var env signaling.CallSignalEnvelope
		if t != net_types.TypeCallSignaling || json.Unmarshal(payload, &env) != nil ||
			env.Action != signaling.ActionParentLost || env.Sender != from {
			return false
		}
		var parent types.PeerID
		_ = json.Unmarshal(env.Payload, &parent)
		mu.Lock()
		got[parent]++
		mu.Unlock()
		return false
	}
	m.mu.Unlock()
	return func() map[types.PeerID]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[types.PeerID]int, len(got))
		for k, v := range got {
			out[k] = v
		}
		return out
	}
}

// silentParentTicks runs 15 member ticks (1.4 s) of a node whose voice parent went quiet 900 ms
// ago, with the given standby. The room lock is held throughout, so real pulses cannot interfere.
func silentParentTicks(nd *testNode, standby types.PeerID) (primary types.PeerID) {
	nd.cm.mu.Lock()
	defer nd.cm.unlockAndFlush()
	room := nd.cm.activeRoom
	manifest := *room.VoiceManifest
	manifest.ParentMap = make(map[types.PeerID]tree.ParentAssignment, len(room.VoiceManifest.ParentMap))
	for k, v := range room.VoiceManifest.ParentMap {
		manifest.ParentMap[k] = v
	}
	ass := manifest.ParentMap[nd.id]
	ass.StandbyParent = standby
	manifest.ParentMap[nd.id] = ass
	room.VoiceManifest = &manifest

	t0 := time.Now()
	room.LastTreePulse = t0.Add(-900 * time.Millisecond)
	for k := 0; k < 15; k++ {
		nd.cm.memberTickLocked(room, t0.Add(time.Duration(k)*tickInterval))
	}
	return ass.PrimaryParent
}

func TestRoom_SilentParentWithoutStandbyIsReportedOncePerSecond(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 4)
	victim := nodes[1]
	sent := parentLostCounter(m, victim.id)

	primary := silentParentTicks(victim, types.PeerID{})
	time.Sleep(300 * time.Millisecond) // the outbox delivers asynchronously

	// t0 and t0+1s: two reports, not one per 100 ms tick
	assert.Equal(t, map[types.PeerID]int{primary: 2}, sent())
}

func TestRoom_SilentStandbyParentIsReportedToo(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 4)
	victim := nodes[1]
	snap, _ := victim.cm.Snapshot()
	var standby types.PeerID
	victim.cm.mu.RLock()
	primaryNow := victim.cm.activeRoom.VoiceManifest.ParentMap[victim.id].PrimaryParent
	victim.cm.mu.RUnlock()
	for _, p := range snap.Members {
		if p != victim.id && p != primaryNow {
			standby = p
			break
		}
	}
	require.NotEqual(t, types.PeerID{}, standby)
	sent := parentLostCounter(m, victim.id)

	primary := silentParentTicks(victim, standby)
	time.Sleep(300 * time.Millisecond)

	// t0: fail over and report the primary; t0+1s: the standby is silent too, report both
	assert.Equal(t, map[types.PeerID]int{primary: 2, standby: 1}, sent())
	victim.cm.mu.RLock()
	assert.True(t, victim.cm.activeRoom.usingStandby)
	victim.cm.mu.RUnlock()
}

// A "feeder silent" report naming the coordinator (stale, from a member that was cut off) must
// not make the coordinator remove itself from its own room.
func TestRoom_StaleReportAboutCoordinatorIsIgnored(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	coord := nodes[0]

	coord.cm.mu.Lock()
	room := coord.cm.activeRoom
	coord.cm.reparentLocked(room, nodes[1].id, signaling.ReparentPayload{StreamID: "s", Avoid: coord.id, Reason: "silent"}, time.Now())
	_, reported := room.reportedLost[coord.id]
	for k := 1; k <= 30; k++ {
		coord.cm.coordinatorTickLocked(room, time.Now().Add(time.Duration(k)*tickInterval))
	}
	_, still := room.ConnectedPeers[coord.id]
	coord.cm.mu.Unlock()

	assert.False(t, reported, "a report about the coordinator itself is not recorded")
	assert.True(t, still, "the coordinator stays in its own room")
}

// A member cut off for longer than the timeouts takes over; once its line is back the two halves
// become one room again.
func TestRoom_CutOffMemberRejoinsAfterItsLineComesBack(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	cut := nodes[1]

	m.kill(cut.id)
	require.Eventually(t, func() bool {
		s, ok := cut.cm.Snapshot()
		return ok && s.IsCoordinator
	}, 6*time.Second, 50*time.Millisecond, "the cut-off member takes over alone")
	require.Eventually(t, func() bool {
		s, _ := nodes[0].cm.Snapshot()
		return len(s.Members) == 2
	}, 6*time.Second, 50*time.Millisecond, "the coordinator drops it")

	m.mu.Lock()
	delete(m.dead, cut.id)
	m.mu.Unlock()
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 6*time.Second, 50*time.Millisecond,
		"one room again: %v", dumpRoom(nodes, nil))
}
