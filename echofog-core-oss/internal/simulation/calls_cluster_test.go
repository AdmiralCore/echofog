package simulation

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rxLog records what one node received: unit index and one-way latency per source stream.
type rxLog struct {
	mu   sync.Mutex
	got  map[string]map[uint32]time.Duration
	errs int
}

func newRxLog() *rxLog { return &rxLog{got: make(map[string]map[uint32]time.Duration)} }

func srcKey(kind media.FrameKind, b types.PeerID, stream string) string {
	return fmt.Sprintf("%d/%s/%s", kind, b.String()[:8], stream)
}

// payload layout: [idx:4][sentUnixNano:8][filler = idx byte]
func mkUnit(idx uint32, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, idx)
	binary.BigEndian.PutUint64(b[4:], uint64(time.Now().UnixNano()))
	for i := 12; i < size; i++ {
		b[i] = byte(idx)
	}
	return b
}

func (l *rxLog) tap(p *media.MediaPacket) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(p.Payload) < 12 {
		l.errs++
		return
	}
	idx := binary.BigEndian.Uint32(p.Payload)
	for _, c := range p.Payload[12:] {
		if c != byte(idx) {
			l.errs++ // corrupted content
			return
		}
	}
	k := srcKey(p.Kind, p.Broadcaster, p.StreamID)
	if l.got[k] == nil {
		l.got[k] = make(map[uint32]time.Duration)
	}
	l.got[k][idx] = now.Sub(time.Unix(0, int64(binary.BigEndian.Uint64(p.Payload[4:]))))
}

// received counts units with index in [from, to) and returns their latencies.
func (l *rxLog) received(key string, from, to uint32) (int, []time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	var lat []time.Duration
	for idx, d := range l.got[key] {
		if idx >= from && idx < to {
			n++
			lat = append(lat, d)
		}
	}
	return n, lat
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[min(len(d)-1, int(float64(len(d))*p))]
}

type liveStream struct {
	id    string
	owner *node.Node
	q     tree.StreamQuality
	size  int // bytes per unit, ~one RTP packet
}

// publishFor pushes units for every stream at its real bitrate, plus voice from two speakers.
// It returns the end index (exclusive) per stream.
func publishFor(t *testing.T, streams []liveStream, speakers []*node.Node, start []uint32, d time.Duration) []uint32 {
	const tick = 10 * time.Millisecond
	perTick := make([]float64, len(streams))
	for i, s := range streams {
		perTick[i] = float64(s.q.BitrateKbps*1000/8) / float64(s.size) * tick.Seconds()
	}
	acc := make([]float64, len(streams))
	next := append([]uint32(nil), start...)
	voiceIdx := start[0]
	deadline := time.Now().Add(d)
	ticks := 0
	for time.Now().Before(deadline) {
		for i, s := range streams {
			acc[i] += perTick[i]
			for acc[i] >= 1 {
				acc[i]--
				if err := s.owner.CallManager.PublishVideo(s.id, mkUnit(next[i], s.size)); err != nil {
					t.Errorf("publish %s: %v", s.id, err)
					return next
				}
				next[i]++
			}
		}
		if ticks%2 == 0 { // 50 voice frames/s per speaker
			for _, sp := range speakers {
				_ = sp.CallManager.PublishAudio(mkUnit(voiceIdx, 160))
			}
			voiceIdx++
		}
		ticks++
		time.Sleep(tick)
	}
	return next
}

// TestCluster_Calls_TenNodesTwoStreamers runs a real 10-node call on loopback (QUIC, or TCP/TLS
// with SIM_TRANSPORT=tcp):
// two peers stream (1080p60 and 720p60), everyone else watches both and two peers talk.
// Then a relaying viewer crashes mid-stream and the rest must keep receiving.
func TestCluster_Calls_TenNodesTwoStreamers(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test")
	}
	nodeCount := getClusterNodeCount(10)
	c, err := NewCluster(t, nodeCount)
	require.NoError(t, err)
	defer c.Close()
	if err := c.WaitForConvergence(20*time.Second, 2); err != nil {
		t.Logf("convergence warning: %v", err)
	}

	for _, n := range c.Nodes {
		require.Equal(t, n.ID, types.PeerPubKeyToID(n.Swarm.GetMyPubKey()), "call signatures are checked against the swarm identity")
	}

	invite := call.NewInviteSecret()
	roomID, key := call.DeriveRoom(invite)
	coord := c.Nodes[0]

	logs := make(map[types.PeerID]*rxLog, nodeCount)
	for i, n := range c.Nodes {
		l := newRxLog()
		logs[n.ID] = l
		n.CallManager.OnMedia = l.tap
		prof := tree.PeerNodeProfile{UploadKbps: 60000, MaxRelayChildren: 3, RTT: 5 * time.Millisecond}
		if i%4 == 3 {
			prof = tree.PeerNodeProfile{UploadKbps: 4000, IsMobile: true} // phones never relay
		}
		require.NoError(t, n.CallManager.JoinRoom(roomID, coord.ID, key, prof))
	}

	allJoined := func(alive map[types.PeerID]bool) bool {
		want := len(alive)
		for _, n := range c.Nodes {
			if !alive[n.ID] {
				continue
			}
			s, ok := n.CallManager.Snapshot()
			if !ok || !s.Joined || len(s.Members) != want {
				return false
			}
		}
		return true
	}
	alive := make(map[types.PeerID]bool)
	for _, n := range c.Nodes {
		alive[n.ID] = true
	}
	defer func() { o, i := network.SmuxDatagramDrops(); t.Logf("[calls] tcp datagram drops: out=%d in=%d", o, i) }()
	joinStart := time.Now()
	require.Eventually(t, func() bool { return allJoined(alive) }, 30*time.Second, 100*time.Millisecond, "room did not form")
	t.Logf("[calls] %d nodes joined in %v", nodeCount, time.Since(joinStart).Round(time.Millisecond))

	// SIM_CALLS_QUALITY="2160p60,1440p60" stresses the trees with 4K; default is 1080p60 + 720p60.
	qa, qb := "1080p60", "720p60"
	if v := os.Getenv("SIM_CALLS_QUALITY"); v != "" {
		if parts := strings.SplitN(v, ",", 2); len(parts) == 2 {
			qa, qb = parts[0], parts[1]
		}
	}
	q1080, err := tree.QualityByID(qa)
	require.NoError(t, err)
	q720, err := tree.QualityByID(qb)
	require.NoError(t, err)
	streamerA, streamerB := c.Nodes[2], c.Nodes[5]
	streams := []liveStream{
		{id: "a-screen", owner: streamerA, q: q1080, size: 1100},
		{id: "b-screen", owner: streamerB, q: q720, size: 1100},
	}
	// Streamers' uplink fits only two copies, so the trees must use viewers as relays.
	streamerA.CallManager.SetUploadCapacity(q1080.BitrateKbps * 5 / 2)
	streamerB.CallManager.SetUploadCapacity(q720.BitrateKbps * 5 / 2)
	for _, s := range streams {
		require.NoError(t, s.owner.CallManager.StartStream(s.id, "screen", s.q))
	}
	for _, n := range c.Nodes {
		for _, s := range streams {
			if n != s.owner {
				require.NoError(t, n.CallManager.WatchStream(s.id))
			}
		}
	}

	viewers := nodeCount - 1
	treesReady := func(alive map[types.PeerID]bool, want int) bool {
		for _, n := range c.Nodes {
			if !alive[n.ID] {
				continue
			}
			for _, s := range streams {
				sm, ok := n.CallManager.StreamTree(s.id)
				if !ok || len(sm.ParentMap) != want {
					return false
				}
			}
		}
		return true
	}
	require.Eventually(t, func() bool { return treesReady(alive, viewers) }, 20*time.Second, 100*time.Millisecond, "stream trees did not converge")
	for _, s := range streams {
		sm, _ := coord.CallManager.StreamTree(s.id)
		depth := 0
		for _, sub := range sm.Subscribers {
			d, p := 0, sub
			for p != sm.Broadcaster {
				p = sm.ParentMap[p]
				d++
			}
			depth = max(depth, d)
		}
		t.Logf("[calls] %s tree: root fan-out %d, depth %d, overflow %d", s.id, len(sm.DistributionTree[sm.Broadcaster]), depth, len(sm.Overflow))
		assert.Empty(t, sm.Overflow)
	}

	speakers := []*node.Node{c.Nodes[1], c.Nodes[7]}

	// Phase 1: steady state.
	phase1 := 4 * time.Second
	end1 := publishFor(t, streams, speakers, make([]uint32, len(streams)), phase1)
	time.Sleep(500 * time.Millisecond)

	report := func(phase string, from, to []uint32, minRatio float64) {
		var allLat []time.Duration
		for _, n := range c.Nodes {
			if !alive[n.ID] {
				continue
			}
			l := logs[n.ID]
			for i, s := range streams {
				if n == s.owner {
					continue
				}
				got, lat := l.received(srcKey(media.FrameVideo, s.owner.ID, s.id), from[i], to[i])
				ratio := float64(got) / float64(to[i]-from[i])
				allLat = append(allLat, lat...)
				assert.GreaterOrEqualf(t, ratio, minRatio, "%s: node %s got %d/%d units of %s", phase, n.ID.String()[:8], got, to[i]-from[i], s.id)
			}
			for _, sp := range speakers {
				if n == sp || !alive[sp.ID] {
					continue
				}
				got, _ := l.received(srcKey(media.FrameAudio, sp.ID, "voice"), 0, 1<<31)
				assert.Positivef(t, got, "%s: node %s heard nothing from %s", phase, n.ID.String()[:8], sp.ID.String()[:8])
			}
			l.mu.Lock()
			assert.Zerof(t, l.errs, "%s: corrupted units at %s", phase, n.ID.String()[:8])
			l.mu.Unlock()
			st := n.CallManager.MediaStats()
			assert.Zero(t, st.DecryptFailures)
		}
		t.Logf("[calls] %s: latency p50=%v p95=%v p99=%v over %d deliveries", phase,
			percentile(allLat, 0.50), percentile(allLat, 0.95), percentile(allLat, 0.99), len(allLat))
	}
	steadyMin, crashMin := 0.97, 0.97
	if raceBuild {
		// the stream still flows, and after the crash it comes back at all; rebuilding the tree
		// at a tenth of real speed does not fit the window. Corruption and decrypt checks stay strict.
		steadyMin, crashMin = 0.5, 0.05
	}
	report("steady", make([]uint32, len(streams)), end1, steadyMin)
	for i, s := range streams {
		t.Logf("[calls] %s published %d units (%d kbps)", s.id, end1[i], s.q.BitrateKbps)
	}

	var relayed, sent uint64
	for _, n := range c.Nodes {
		st := n.CallManager.MediaStats()
		relayed += st.FragmentsRelayed
		sent += st.Outbox.Sent[media.ClassVideo]
		t.Logf("[calls] node %s relayed=%d videoSent=%d dropped=%d failed=%d dups=%d",
			n.ID.String()[:8], st.FragmentsRelayed, st.Outbox.Sent[media.ClassVideo], st.Outbox.Dropped[media.ClassVideo], st.Outbox.Failed[media.ClassVideo], st.Duplicates)
	}
	assert.Positive(t, relayed, "viewers must relay, not only the streamers")

	// Phase 2: a relaying viewer crashes without saying goodbye.
	sm, _ := coord.CallManager.StreamTree("a-screen")
	var victim *node.Node
	for _, n := range c.Nodes {
		if n != coord && n != streamerA && n != streamerB && len(sm.DistributionTree[n.ID]) > 0 {
			victim = n
			break
		}
	}
	require.NotNil(t, victim, "1080p60 tree should contain a relaying viewer")
	t.Logf("[calls] crashing relay %s (feeds %d viewers)", victim.ID.String()[:8], len(sm.DistributionTree[victim.ID]))
	crashAt := time.Now()
	_ = victim.Net.Close() // QUIC and the TCP fallback: a crashed node answers neither
	delete(alive, victim.ID)

	// Publish through the failure while measuring, in parallel, how long the room takes to heal.
	start2 := make([]uint32, len(streams))
	for i := range start2 {
		start2[i] = end1[i] + 10000 // separate index space for phase 2
	}
	healed := make(chan time.Duration, 1)
	go func() {
		for time.Since(crashAt) < 15*time.Second {
			if allJoined(alive) && treesReady(alive, viewers-1) {
				healed <- time.Since(crashAt)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		healed <- -1
	}()
	end2 := publishFor(t, streams, speakers, start2, 8*time.Second)
	time.Sleep(500 * time.Millisecond)
	healTime := <-healed
	require.Positive(t, healTime, "room did not heal")
	t.Logf("[calls] room and both trees healed %v after the crash", healTime.Round(10*time.Millisecond))

	// Everything published once the room healed (plus a 500 ms margin) must reach all survivors.
	from2 := make([]uint32, len(streams))
	for i, s := range streams {
		perSec := float64(s.q.BitrateKbps*1000/8) / float64(s.size)
		from2[i] = start2[i] + uint32(perSec*(healTime+500*time.Millisecond).Seconds())
		require.Less(t, from2[i], end2[i], "publishing window too short to judge recovery")
	}
	report("after-crash", from2, end2, crashMin)

	if os.Getenv("SIM_CALLS_VERBOSE") != "" {
		for _, n := range c.Nodes {
			s, _ := n.CallManager.Snapshot()
			t.Logf("node %s coord=%v members=%d", n.ID.String()[:8], s.IsCoordinator, len(s.Members))
		}
	}
}
