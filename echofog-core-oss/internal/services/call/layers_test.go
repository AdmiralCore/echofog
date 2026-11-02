package call

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A child whose reports are heavy while ours are not loses a layer at a time, down to the base,
// waiting between steps; clean for a while, it gets a layer back, and one lost again soon after
// makes the next try wait twice as long.
func TestChildLayers_StepsDownAndBackUp(t *testing.T) {
	c := newChildLayers()
	now := time.Unix(1000, 0)
	tick := func(level media.Congestion, own media.Congestion) bool {
		now = now.Add(media.FeedbackInterval)
		c.note(level, true, now)
		return c.step(own, videolayer.All, now)
	}
	assert.False(t, tick(media.PathHeavy, media.PathClean), "one heavy report is not enough")
	assert.True(t, tick(media.PathHeavy, media.PathClean))
	assert.Equal(t, uint8(1), c.cap)
	assert.False(t, tick(media.PathHeavy, media.PathClean))
	assert.False(t, tick(media.PathHeavy, media.PathClean), "a change waits for the last one to show")
	tick(media.PathHeavy, media.PathClean)
	tick(media.PathHeavy, media.PathClean)
	assert.Equal(t, uint8(0), c.cap)
	assert.False(t, tick(media.PathFlood, media.PathClean), "the base always goes")

	// our own path is as bad: the trouble is not the child's hop
	c = newChildLayers()
	for i := 0; i < 8; i++ {
		assert.False(t, tick(media.PathHeavy, media.PathHeavy))
	}
	assert.Equal(t, videolayer.All, c.cap)

	// back up after layerUpFirst clean, down again at once: the next try waits twice as long
	c = newChildLayers()
	tick(media.PathHeavy, media.PathClean)
	tick(media.PathHeavy, media.PathClean)
	require.Equal(t, uint8(1), c.cap)
	raised := false
	for i := 0; i < int(layerUpFirst/media.FeedbackInterval)+2 && !raised; i++ {
		raised = tick(media.PathClean, media.PathClean)
	}
	require.True(t, raised)
	assert.Equal(t, videolayer.All, c.cap)
	for i := 0; i < 6 && c.cap == videolayer.All; i++ {
		tick(media.PathHeavy, media.PathClean)
	}
	assert.Equal(t, uint8(1), c.cap)
	assert.Equal(t, 2*layerUpFirst, c.upWait)
}

func TestChildLayers_TwoLayerStreamSkipsTheMiddle(t *testing.T) {
	assert.Equal(t, uint8(0), down(videolayer.All, 1))
	assert.Equal(t, videolayer.All, up(0, 1))
	assert.Equal(t, uint8(1), down(videolayer.All, videolayer.All))
	assert.Equal(t, uint8(1), up(0, videolayer.All))
}

// layerPump stands for the desktop's encoder with three temporal layers: 60 frames a second in
// the L1T3 pattern at the bitrate it is told, base frames half again as large as the others, in
// marked RTP packets of 1000 bytes.
type layerPump struct {
	pump
}

func (p *layerPump) run(cm *CallManager) {
	p.stop = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(time.Second / 60)
		defer t.Stop()
		seq := uint16(1)
		for f := 0; ; f++ {
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
			tid := [4]uint8{0, 2, 1, 2}[f%4]
			key := p.key.Swap(false) || f == 0
			if key {
				tid = 0
			}
			size := int(p.bps.Load()/8/60) * 4 / 9 // 1.5 + 1 + 1 + 1 per four frames
			if tid == 0 {
				size = size * 3 / 2
			}
			if key {
				size *= 4
			}
			n := max(1, (size+999)/1000)
			for i := 0; i < n; i++ {
				pl := make([]byte, 1000-12-videolayer.ExtensionLen)
				pl[0] = 0x41
				if key {
					pl[0] = 0x65
				}
				binary.BigEndian.PutUint32(pl[1:5], uint32(f))
				pk := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: seq, Timestamp: uint32(f * 1500), SSRC: 9, Marker: i == n-1}, Payload: pl}
				pk.Header.Extension, pk.Header.ExtensionProfile = true, 0xBEDE
				m := videolayer.Mark{TID: tid, Start: i == 0, End: i == n-1, Key: key, Discardable: tid == 2, Index: uint16(i), Count: uint16(n)}
				_ = pk.Header.SetExtension(videolayer.ExtensionID, m.Data())
				b, _ := pk.Marshal()
				seq++
				cm.PublishLocalRTP(bridge.RoleScreen, b)
			}
		}
	}()
}

// layerTap records what a viewer plays: frames per layer, and holes in the RTP numbers its webview
// would see.
type layerTap struct {
	mu      sync.Mutex
	frames  map[int]uint8 // frame -> layer
	holes   int
	backs   int
	lastSeq uint16
	started bool
}

func (l *layerTap) tap(p *media.MediaPacket) {
	if p.Kind != media.FrameVideo {
		return
	}
	var pk rtp.Packet
	if pk.Unmarshal(p.Payload) != nil || len(pk.Payload) < 5 {
		return
	}
	m, _, ok := videolayer.Find(p.Payload)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames[int(binary.BigEndian.Uint32(pk.Payload[1:5]))] = m.TID
	if l.started {
		switch d := int16(pk.SequenceNumber - l.lastSeq); {
		case d > 1:
			l.holes++
		case d < 0:
			l.backs++
		}
		if int16(pk.SequenceNumber-l.lastSeq) <= 0 {
			return
		}
	}
	l.started, l.lastSeq = true, pk.SequenceNumber
}

// take returns the frames played per layer since the last call, and the holes and steps back.
func (l *layerTap) take() (perLayer map[uint8]int, holes, backs int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	perLayer = map[uint8]int{}
	for _, tid := range l.frames {
		perLayer[tid]++
	}
	holes, backs = l.holes, l.backs
	l.frames, l.holes, l.backs = map[int]uint8{}, 0, 0
	return perLayer, holes, backs
}

// Two viewers watch a 720p60 stream with three temporal layers straight from its broadcaster; the
// link to one of them narrows to less than the stream but more than its base layer. Its parent
// (the broadcaster, or the other viewer once it is found weak) leaves the upper layers out for that
// viewer alone: it plays the base, whole, at a
// quarter of the frame rate, with nothing its webview would take for a loss, while the other
// keeps every layer and the stream's rate is not held back for the weak one.
func TestRoom_AWeakChildGetsTheBaseLayer(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	strong, b, weak := nodes[0], nodes[1], nodes[2]
	enc := &layerPump{}
	b.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("720p60")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, strong.cm.WatchStream("s"))
	require.NoError(t, weak.cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && sm.ParentMap[weak.id] == b.id && sm.ParentMap[strong.id] == b.id
	}, 5*time.Second, 20*time.Millisecond, "both viewers hang on the broadcaster")
	require.Eventually(t, func() bool { return enc.bps.Load() > 0 }, 5*time.Second, 20*time.Millisecond)

	taps := map[types.PeerID]*layerTap{}
	for _, nd := range []*testNode{strong, weak} {
		tp := &layerTap{frames: map[int]uint8{}}
		taps[nd.id] = tp
		nd.cm.OnMedia = tp.tap
	}
	enc.run(b.cm)
	defer enc.halt()
	time.Sleep(2 * time.Second)
	// the weak viewer's own downlink: the base is ~2 Mbit/s of 6, base and middle ~3.3. Found weak,
	// it may be hung below the other viewer: the link is the same.
	link := m.shape(b.id, weak.id, 2800, 200*time.Millisecond)
	m.mu.Lock()
	m.links[[2]types.PeerID{strong.id, weak.id}] = link
	m.mu.Unlock()

	capOf := func() uint8 {
		sm, ok := b.cm.StreamTree("s")
		if !ok {
			return videolayer.All
		}
		parent := b
		if sm.ParentMap[weak.id] == strong.id {
			parent = strong
		}
		parent.cm.mu.RLock()
		defer parent.cm.mu.RUnlock()
		return parent.cm.activeRoom.Dispatcher.ChildLayers("s", weak.id)
	}
	require.Eventually(t, func() bool { return capOf() == 0 }, 10*time.Second, 50*time.Millisecond,
		"the weak viewer's layers come down to the base")
	time.Sleep(1500 * time.Millisecond) // the change goes through
	taps[weak.id].take()
	taps[strong.id].take()
	d0, p0 := link.dropped.Load(), link.passed.Load()

	time.Sleep(3 * time.Second)
	wl, wHoles, wBacks := taps[weak.id].take()
	sl, sHoles, _ := taps[strong.id].take()
	dropped, passed := link.dropped.Load()-d0, link.passed.Load()-p0
	t.Logf("weak viewer: frames per layer %v, %d holes, %d steps back; link dropped %d of %d; cap now %s",
		wl, wHoles, wBacks, dropped, dropped+passed, capName(capOf()))
	t.Logf("strong viewer: frames per layer %v, %d holes; stream at %d kbps", sl, sHoles, enc.bps.Load()/1000)

	weakFrames := wl[0] + wl[1] + wl[2]
	assert.GreaterOrEqual(t, wl[0], 3*15*8/10, "the weak viewer plays the base, about 15 frames a second")
	assert.LessOrEqual(t, wl[1]+wl[2], weakFrames/4, "and little above it")
	assert.LessOrEqual(t, wHoles, 3, "its webview sees next to no holes")
	assert.Zero(t, wBacks)
	assert.GreaterOrEqual(t, sl[0]+sl[1]+sl[2], 3*60*8/10, "the strong viewer keeps all 60 frames a second")
	assert.Greater(t, sl[2], 3*30*7/10)
	assert.Greater(t, enc.bps.Load(), uint64(4500_000), "the stream is not held back for the weak viewer")
	assert.Less(t, float64(dropped), 0.05*float64(dropped+passed), "the base fits the narrow link")
}

// The same, with the weak viewer an older node that does not say it reads layer caps: it gets the
// whole stream as before, and never a fragment it would take as malformed.
func TestRoom_AnOlderWeakViewerGetsTheWholeStream(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 3)
	strong, b, weak := nodes[0], nodes[1], nodes[2]
	weak.cm.beforeLayers = true
	var capped atomic.Int32
	m.tap = func(from, to types.PeerID, mt net_types.MessageType, payload []byte) {
		if to == weak.id && mt == net_types.TypeCallMediaVideo {
			if p, err := media.Decode(payload); err == nil && p.HopCap != 0 {
				capped.Add(1)
			}
		}
	}
	enc := &layerPump{}
	b.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("720p60")
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	require.NoError(t, strong.cm.WatchStream("s"))
	require.NoError(t, weak.cm.WatchStream("s"))
	require.Eventually(t, func() bool {
		sm, ok := b.cm.StreamTree("s")
		return ok && sm.ParentMap[weak.id] == b.id && sm.ParentMap[strong.id] == b.id
	}, 5*time.Second, 20*time.Millisecond)
	tp := &layerTap{frames: map[int]uint8{}}
	weak.cm.OnMedia = tp.tap
	enc.run(b.cm)
	defer enc.halt()
	time.Sleep(2 * time.Second)
	link := m.shape(b.id, weak.id, 2800, 200*time.Millisecond)
	m.mu.Lock()
	m.links[[2]types.PeerID{strong.id, weak.id}] = link
	m.mu.Unlock()
	time.Sleep(6 * time.Second)
	tp.take()
	time.Sleep(2 * time.Second)
	perLayer, _, _ := tp.take()
	t.Logf("older weak viewer: frames per layer %v", perLayer)
	assert.NotZero(t, perLayer[2], "it still gets the top layer, as much as its link lets through")
	assert.Zero(t, capped.Load(), "no fragment with a cap went to it")
	for _, nd := range []*testNode{b, strong} {
		nd.cm.mu.RLock()
		got := nd.cm.activeRoom.Dispatcher.ChildLayers("s", weak.id)
		nd.cm.mu.RUnlock()
		assert.Equal(t, videolayer.All, got)
	}
}
