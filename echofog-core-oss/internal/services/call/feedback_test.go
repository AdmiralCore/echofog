package call

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pump stands for the desktop's encoder: 30 frames a second at the bitrate it is told, in units
// of 1100 bytes, a keyframe five frames large when asked.
type pump struct {
	bps  atomic.Uint64
	key  atomic.Bool
	stop chan struct{}
	wg   sync.WaitGroup
}

func (p *pump) SetBitrate(bps uint64) { p.bps.Store(bps) }
func (p *pump) RequestKeyframe()      { p.key.Store(true) }

func (p *pump) run(cm *CallManager) {
	p.stop = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(time.Second / 30)
		defer t.Stop()
		var i uint32
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
			size := int(p.bps.Load() / 8 / 30)
			if p.key.Swap(false) {
				size *= 5
			}
			for ; size > 0; size -= 1100 {
				i++
				cm.PublishLocalRTP(bridge.RoleScreen, unit(i, 1100))
			}
		}
	}()
}

func (p *pump) halt() {
	close(p.stop)
	p.wg.Wait()
}

// A viewer behind a 4 Mbit/s link watches a stream started at 1080p60 (12 Mbit/s). The
// broadcaster's own queue never fills, as the link is the viewer's: only the viewer's reports
// can bring the rate down to what the link carries, and once they do, the stream comes through
// whole. When the link widens again, the rate climbs back.
func TestRoom_ViewerReportsBringTheRateDownToTheirLink(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 2)
	v, b := nodes[0], nodes[1]
	enc := &pump{}
	b.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("1080p60")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, v.cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return enc.bps.Load() > 0 }, 5*time.Second, 20*time.Millisecond)

	link := m.shape(b.id, v.id, 4000, 250*time.Millisecond)
	enc.run(b.cm)
	defer enc.halt()

	var settled time.Duration
	start := time.Now()
	for time.Since(start) < 15*time.Second {
		time.Sleep(250 * time.Millisecond)
		if settled == 0 && enc.bps.Load() <= 4000_000 {
			settled = time.Since(start)
		}
	}
	require.NotZero(t, settled, "the rate stayed at %d kbps", enc.bps.Load()/1000)
	t.Logf("the rate came down to the link in %v", settled.Round(100*time.Millisecond))

	// settled: the link drops next to nothing
	d0, p0 := link.dropped.Load(), link.passed.Load()
	time.Sleep(4 * time.Second)
	dropped, passed := link.dropped.Load()-d0, link.passed.Load()-p0
	t.Logf("settled at %d kbps: %d of %d fragments dropped", enc.bps.Load()/1000, dropped, dropped+passed)
	assert.Less(t, float64(dropped), 0.02*float64(dropped+passed), "the stream fits its link")
	assert.Greater(t, enc.bps.Load(), uint64(2000_000), "and still uses most of it")

	// the link widens: the rate climbs back
	m.shape(b.id, v.id, 0, 0)
	low, wide := enc.bps.Load(), time.Now()
	require.Eventually(t, func() bool { return enc.bps.Load() > 2*low }, 15*time.Second, 100*time.Millisecond,
		"the rate stayed at %d kbps", enc.bps.Load()/1000)
	t.Logf("the link widened: the rate doubled in %v", time.Since(wide).Round(100*time.Millisecond))
}

// A viewer behind a narrow link hangs below a relay. Its reports reach the broadcaster through
// the relay; as the relay's own path is clean meanwhile, the broadcaster finds it weak, the
// coordinator keeps it at the bottom of the tree, and the stream stays at what the relay gets
// instead of following the weak viewer down to its link.
func TestRoom_AWeakViewerDoesNotHoldTheOthersBack(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	relay, b, weak := nodes[0], nodes[1], nodes[2]
	b.cm.SetUploadCapacity(8000) // the broadcaster feeds one viewer, the other hangs below it
	enc := &pump{}
	b.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("720p60") // 6 Mbit/s
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, relay.cm.WatchStream("s"))
	require.NoError(t, weak.cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && sm.ParentMap[weak.id] == relay.id && sm.ParentMap[relay.id] == b.id
	}, 5*time.Second, 20*time.Millisecond, "the weak viewer hangs below the relay")
	require.Eventually(t, func() bool { return enc.bps.Load() > 0 }, 5*time.Second, 20*time.Millisecond)

	enc.run(b.cm)
	defer enc.halt()
	time.Sleep(3 * time.Second) // both watch for a while, then the weak one's link narrows
	m.shape(relay.id, weak.id, 1500, 200*time.Millisecond)

	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && len(sm.Weak) == 1 && sm.Weak[0] == weak.id
	}, 15*time.Second, 100*time.Millisecond, "the broadcaster finds the weak viewer")
	b.cm.mu.Lock()
	reports := len(b.cm.activeRoom.feedback["s"])
	b.cm.mu.Unlock()
	assert.Equal(t, 2, reports, "both viewers' reports reach the broadcaster, one through the relay")

	time.Sleep(5 * time.Second)
	t.Logf("with the weak viewer's link at 1.5 Mbit/s the stream goes at %d kbps", enc.bps.Load()/1000)
	assert.Greater(t, enc.bps.Load(), uint64(4000_000), "the relay's clean path sets the rate")
}

// Sound goes twice on every hop: with every tenth datagram of sound lost on every link, nearly all
// of a speaker's words and a stream's sound arrive, through relays too, where a tenth of them was
// lost on each hop before.
func TestRoom_SoundSurvivesScatteredLoss(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 5)
	b := nodes[1]
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 4
	}, 5*time.Second, 20*time.Millisecond)

	var n atomic.Uint64
	m.mu.Lock()
	m.drop = func(tp net_types.MessageType, payload []byte) bool {
		p, err := media.Decode(payload)
		return err == nil && (p.Kind == media.FrameAudio || p.Kind == media.FrameStreamAudio) && n.Add(1)%10 == 0
	}
	m.mu.Unlock()
	const units = 200
	for i := uint32(0); i < units; i++ {
		require.NoError(t, b.cm.PublishAudio(unit(i, 160)))
		require.NoError(t, b.cm.PublishStreamAudio("s", unit(i, 300)))
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	for _, nd := range nodes {
		if nd == b {
			continue
		}
		voice, sound := nd.log.count(media.FrameAudio, b.id, "voice"), nd.log.count(media.FrameStreamAudio, b.id, "s")
		t.Logf("of %d units: %d of the voice, %d of the stream's sound", units, voice, sound)
		assert.GreaterOrEqual(t, voice, units*97/100, "voice")
		assert.GreaterOrEqual(t, sound, units*97/100, "stream sound")
	}
	assert.NotZero(t, n.Load())
}

// A weak viewer's keyframe requests go on once in weakKeyframeInterval: every keyframe it got
// would be every viewer's.
func TestRoom_AWeakViewersKeyframeRequestsAreHeldBack(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	b := nodes[1]
	var served atomic.Int32
	b.cm.OnKeyframeRequest = func(string) { served.Add(1) }
	q, _ := tree.QualityByID("720p30")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, nodes[0].cm.WatchStream("s"))
	require.NoError(t, nodes[2].cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 2
	}, 5*time.Second, 20*time.Millisecond)
	coord := nodes[0] // as the broadcaster's stream_weak reaches it
	coord.cm.mu.Lock()
	coord.cm.weakLocked(coord.cm.activeRoom, b.id, signaling.StreamWeakPayload{StreamID: "s", Weak: []types.PeerID{nodes[2].id}})
	coord.cm.mu.Unlock()
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && len(sm.Weak) == 1
	}, 5*time.Second, 20*time.Millisecond)

	time.Sleep(keyframeServeInterval + 100*time.Millisecond)
	before := served.Load()
	ask := func(nd *testNode) {
		b.cm.mu.Lock()
		b.cm.handleKeyframeRequestLocked(b.cm.activeRoom, nd.id, "s", time.Now())
		b.cm.mu.Unlock()
	}
	for i := 0; i < 3; i++ {
		ask(nodes[2])
		time.Sleep(keyframeServeInterval + 100*time.Millisecond)
	}
	require.Eventually(t, func() bool { return served.Load() == before+1 }, time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before+1, served.Load(), "one of three in %v", 3*keyframeServeInterval)

	ask(nodes[0]) // a strong viewer's goes on
	require.Eventually(t, func() bool { return served.Load() == before+2 }, time.Second, 20*time.Millisecond)
}
