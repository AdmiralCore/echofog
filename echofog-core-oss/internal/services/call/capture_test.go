package call

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/videolayer"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEncoder stands for the desktop's own screen encoder.
type fakeEncoder struct {
	mu        sync.Mutex
	bitrates  []uint64
	keyframes int
	screens   []int // the viewers' refresh rates it was last given
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

func (f *fakeEncoder) SetViewerRates(hz []int) {
	f.mu.Lock()
	f.screens = append([]int(nil), hz...)
	f.mu.Unlock()
}

func (f *fakeEncoder) viewerRates() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]int(nil), f.screens...)
	slices.Sort(out)
	return out
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

// The broadcaster's encoder learns the refresh rates of its viewers' screens: with the subscription,
// and again when a viewer's screen changes while it watches. A viewer that does not say counts as 0.
func TestRoom_LocalEncoderLearnsTheViewersScreens(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 4)
	b := nodes[1]
	enc := &fakeEncoder{}
	b.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("1080p144")
	require.NoError(t, b.cm.StartStream("s", "screen", q))

	rates := map[int]int{0: 60, 2: 144} // node 3 is an older build
	for i, nd := range nodes {
		if nd == b {
			continue
		}
		nd.cm.SetDisplayRate(rates[i])
		require.NoError(t, nd.cm.WatchStream("s"))
	}
	require.Eventually(t, func() bool { return slices.Equal(enc.viewerRates(), []int{0, 60, 144}) },
		5*time.Second, 20*time.Millisecond, "got %v", enc.viewerRates())

	// the first viewer moves its window to a 120 Hz screen
	nodes[0].cm.SetDisplayRate(120)
	require.Eventually(t, func() bool { return slices.Equal(enc.viewerRates(), []int{0, 120, 144}) },
		5*time.Second, 20*time.Millisecond, "got %v", enc.viewerRates())
}

// A layer mark rides only on the app's own capture. The webview's RTP keeps its other header
// extensions, but an element that looks like the mark (ID 10, 9 bytes) is blanked before it goes:
// no relay or viewer takes the browser's packets for layers.
func TestRoom_LayerMarksOnlyFromTheAppsCapture(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 2)
	b, v := nodes[0], nodes[1]
	q, _ := tree.QualityByID("720p60")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, v.cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && sm.ParentMap[v.id] == b.id
	}, 5*time.Second, 20*time.Millisecond)

	var mu sync.Mutex
	marks := map[byte]bool{} // first payload byte -> the delivered packet had a mark
	v.cm.OnMedia = func(p *media.MediaPacket) {
		if p.Kind != media.FrameVideo {
			return
		}
		var pk rtp.Packet
		if pk.Unmarshal(p.Payload) != nil || len(pk.Payload) == 0 {
			return
		}
		_, _, ok := videolayer.Find(p.Payload)
		mu.Lock()
		marks[pk.Payload[0]] = ok
		mu.Unlock()
	}
	packet := func(tag byte, seq uint16) []byte {
		pk := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: seq, Timestamp: uint32(seq) * 1500, SSRC: 5, Marker: true},
			Payload: []byte{tag, 0x41, 1, 2, 3}}
		pk.Header.Extension, pk.Header.ExtensionProfile = true, 0xBEDE
		mk := videolayer.Mark{TID: 2, Start: true, End: true, Index: 0, Count: 1}
		require.NoError(t, pk.Header.SetExtension(videolayer.ExtensionID, mk.Data()))
		raw, err := pk.Marshal()
		require.NoError(t, err)
		return raw
	}
	b.cm.onWebviewRTP(bridge.RoleScreen, packet(1, 1))
	b.cm.PublishLocalRTP(bridge.RoleScreen, packet(2, 2))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(marks) == 2
	}, 5*time.Second, 20*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.False(t, marks[1], "the webview's packet reached the viewer with a layer mark")
	assert.True(t, marks[2], "the app's own capture keeps its mark")
}
