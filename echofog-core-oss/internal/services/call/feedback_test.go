package call

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
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

// A relay passes up the worst report of its subtree: the broadcaster slows down for a viewer it
// does not feed itself.
func TestRoom_RelayPassesUpItsChildrensReports(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 2)
	b, relay := nodes[1], nodes[0]
	now := time.Now()
	b.cm.mu.Lock()
	room := b.cm.activeRoom
	b.cm.mu.Unlock()
	require.NotNil(t, room)
	q, _ := tree.QualityByID("1080p60")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, relay.cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := relay.cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 1
	}, 5*time.Second, 20*time.Millisecond)

	// a viewer under the relay reports a heavy path
	var far types.PeerID
	far[0] = 9
	relay.cm.mu.Lock()
	rr := relay.cm.activeRoom
	rr.feedback["s"] = map[types.PeerID]viewerReport{far: {fb: media.Feedback{Level: media.PathHeavy, Kbps: 3000, Loss: 0.12}, viewer: far, at: now}}
	relay.cm.mu.Unlock()
	refresh := func() {
		relay.cm.mu.Lock()
		rr.feedback["s"][far] = viewerReport{fb: media.Feedback{Level: media.PathHeavy, Kbps: 3000, Loss: 0.12}, viewer: far, at: time.Now()}
		relay.cm.mu.Unlock()
	}
	require.Eventually(t, func() bool {
		refresh()
		b.cm.mu.Lock()
		defer b.cm.mu.Unlock()
		rep, ok := b.cm.worstFeedbackLocked(room, "s", time.Now())
		return ok && rep.viewer == far && rep.fb.Level == media.PathHeavy && rep.fb.Kbps == 3000
	}, 5*time.Second, 50*time.Millisecond)
}
