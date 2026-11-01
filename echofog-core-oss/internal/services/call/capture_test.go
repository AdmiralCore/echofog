package call

import (
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEncoder stands for the desktop's own screen encoder.
type fakeEncoder struct {
	mu        sync.Mutex
	bitrates  []uint64
	keyframes int
}

func (f *fakeEncoder) SetBitrate(bps uint64) {
	f.mu.Lock()
	f.bitrates = append(f.bitrates, bps)
	f.mu.Unlock()
}

func (f *fakeEncoder) RequestKeyframe() {
	f.mu.Lock()
	f.keyframes++
	f.mu.Unlock()
}

func (f *fakeEncoder) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bitrates), f.keyframes
}

// A screen encoded outside the webview gets the stream's rate and the viewers' keyframe requests,
// and what it publishes reaches every viewer like the webview's capture does.
func TestRoom_LocalEncoderGetsRateAndKeyframes(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 4)
	b := nodes[1]
	enc := &fakeEncoder{}
	b.cm.SetLocalEncoder(bridge.RoleScreen, enc)

	q, _ := tree.QualityByID("1440p120")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, nd := range nodes {
		if nd != b {
			require.NoError(t, nd.cm.WatchStream("s"))
		}
	}
	// joining viewers ask for a keyframe; the rate loop sets the preset
	require.Eventually(t, func() bool { r, k := enc.counts(); return r > 0 && k > 0 }, 5*time.Second, 20*time.Millisecond)
	enc.mu.Lock()
	assert.Equal(t, uint64(q.BitrateKbps*1000), enc.bitrates[0], "starts at the preset")
	enc.mu.Unlock()

	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && len(sm.ParentMap) == 3
	}, 5*time.Second, 20*time.Millisecond)
	for i := uint32(0); i < 3; i++ {
		b.cm.PublishLocalRTP(bridge.RoleScreen, unit(i, 1000))
	}
	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			if nd != b && nd.log.count(media.FrameVideo, b.id, "s") < 3 {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond)

	// the broadcaster sees its own screen on the preview track
	preview := func() bool {
		for _, s := range b.cm.MediaSlots() {
			if s.Kind == "preview" && s.StreamID == "s" {
				return true
			}
		}
		return false
	}
	assert.True(t, preview(), "our screen plays on the preview track")

	// handed back to the webview: the encoder hears nothing more
	b.cm.SetLocalEncoder(bridge.RoleScreen, nil)
	time.Sleep(rateInterval + 2*tickInterval)
	r, k := enc.counts()
	time.Sleep(2 * rateInterval)
	r2, k2 := enc.counts()
	assert.Equal(t, r, r2)
	assert.Equal(t, k, k2)

	require.NoError(t, b.cm.StopScreenShare("s"))
	assert.False(t, preview(), "a stopped stream frees the preview track")
}
