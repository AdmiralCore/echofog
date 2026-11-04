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
	"github.com/echofog/echofog-core/internal/testutil"
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
	frames atomic.Int64
	bases  atomic.Int64 // frames published on the base layer
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
			if tid == 0 {
				p.bases.Add(1)
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
			p.frames.Add(1)
		}
	}()
}

// layerTap records what a viewer plays: frames per layer, and the base frames it never got.
// Upper layers are left out on purpose for a weak viewer, so their RTP numbers are not holes;
// the base is what the webview must receive whole.
type layerTap struct {
	mu       sync.Mutex
	frames   map[int]uint8 // frame -> layer
	backs    int           // base frames whose first packet arrived after a later frame
	seenBase map[int]bool
	maxBase  int
	baseSeen bool
	// parent names the feeder the viewer currently hangs on, when the test can tell it: a jump
	// in the RTP numbers across a change of feeder is neither a loss nor a reordering.
	parent      func() types.PeerID
	parentSeen  types.PeerID
	parentKnown bool
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
	if l.parent != nil {
		// A new feeder numbers its packets from elsewhere: its first frame is a fresh anchor.
		p := l.parent()
		if l.parentKnown && p != l.parentSeen {
			l.baseSeen, l.seenBase, l.maxBase = false, nil, 0
		}
		l.parentSeen, l.parentKnown = p, true
	}
	frame := int(binary.BigEndian.Uint32(pk.Payload[1:5]))
	l.frames[frame] = m.TID
	if m.TID != 0 {
		return // an upper layer may be left out by design
	}
	if !l.baseSeen {
		l.baseSeen, l.maxBase = true, frame
	} else if !l.seenBase[frame] {
		if frame < l.maxBase {
			l.backs++
		}
		if frame > l.maxBase {
			l.maxBase = frame
		}
	}
	if l.seenBase == nil {
		l.seenBase = map[int]bool{}
	}
	l.seenBase[frame] = true
}

// total returns how many frames the viewer has played since the last take, without resetting.
func (l *layerTap) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.frames)
}

// take returns the frames played per layer since the last call, and how many base frames first
// arrived after a later one.
func (l *layerTap) take() (perLayer map[uint8]int, backs int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	perLayer = map[uint8]int{}
	for _, tid := range l.frames {
		perLayer[tid]++
	}
	backs = l.backs
	l.frames, l.seenBase, l.backs, l.baseSeen, l.maxBase = map[int]uint8{}, nil, 0, false, 0
	return perLayer, backs
}

// oneBase is a base frame of the layer pump, in the packet a viewer's tap sees.
func oneBase(seq uint16, frame uint32) *media.MediaPacket {
	pl := make([]byte, 100)
	pl[0] = 0x41
	binary.BigEndian.PutUint32(pl[1:5], frame)
	pk := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: seq, Timestamp: frame * 1500, SSRC: 9, Marker: true},
		Payload: pl,
	}
	pk.Header.Extension, pk.Header.ExtensionProfile = true, 0xBEDE
	m := videolayer.Mark{TID: 0, Start: true, End: true, Index: 0, Count: 1}
	_ = pk.Header.SetExtension(videolayer.ExtensionID, m.Data())
	b, _ := pk.Marshal()
	return &media.MediaPacket{Kind: media.FrameVideo, Payload: b}
}

// A viewer may be moved to another feeder: the new feeder's RTP numbers are elsewhere, and that
// jump is neither a lost base frame nor one that came out of order.
func TestLayerTap_AParentSwitchIsNotALoss(t *testing.T) {
	parent := types.PeerID{1}
	tap := &layerTap{frames: map[int]uint8{}, parent: func() types.PeerID { return parent }}
	tap.tap(oneBase(1, 0))
	tap.tap(oneBase(5, 1))
	tap.tap(oneBase(9, 2))

	parent = types.PeerID{2}
	tap.tap(oneBase(401, 10))
	tap.tap(oneBase(405, 11))

	perLayer, backs := tap.take()
	assert.Equal(t, 5, perLayer[0], "every base frame is counted")
	assert.Zero(t, backs, "a new feeder's numbering is not a reordering")
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
	}, testutil.Slow(5*time.Second), 20*time.Millisecond, "both viewers hang on the broadcaster")
	require.Eventually(t, func() bool { return enc.bps.Load() > 0 }, testutil.Slow(5*time.Second), 20*time.Millisecond)

	parentOf := func() types.PeerID {
		sm, ok := b.cm.StreamTree("s")
		if !ok {
			return b.id
		}
		return sm.ParentMap[weak.id]
	}
	taps := map[types.PeerID]*layerTap{}
	for _, nd := range []*testNode{strong, weak} {
		tp := &layerTap{frames: map[int]uint8{}}
		if nd == weak {
			tp.parent = parentOf
		}
		taps[nd.id] = tp
		nd.cm.OnMedia = tp.tap
	}
	enc.run(b.cm)
	defer enc.halt()
	require.Eventually(t, func() bool {
		return taps[strong.id].total() > 0 && taps[weak.id].total() > 0
	}, testutil.Slow(10*time.Second), 20*time.Millisecond, "both viewers receive the stream")
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
	require.Eventually(t, func() bool { return enc.bps.Load() > 4_500_000 }, testutil.Slow(10*time.Second), 20*time.Millisecond,
		"the stream comes back up once the weak viewer is capped")
	require.Eventually(t, func() bool { return capOf() == 0 }, testutil.Slow(12*time.Second), 50*time.Millisecond,
		"the weak viewer's layers come down to the base")
	require.Eventually(t, func() bool { return capOf() == 0 }, testutil.Slow(12*time.Second), 50*time.Millisecond,
		"the weak viewer is still on the base")

	// A weak viewer may be moved under another one; sampling during that move counts the old
	// parent's late packets as steps back. Let the move drain, then measure only a window in
	// which the feeder did not change: a move from one relay to another puts the RTP numbers
	// elsewhere, and a window that spans it would count that jump as loss on a link that
	// dropped nothing.
	parent, stableSince := parentOf(), time.Now()
	require.Eventually(t, func() bool {
		if p := parentOf(); p != parent {
			parent, stableSince = p, time.Now()
		}
		return time.Since(stableSince) >= 700*time.Millisecond
	}, testutil.Slow(15*time.Second), 20*time.Millisecond, "the weak viewer's parent settles")
	const sampleWindow = 1500 * time.Millisecond
	var (
		wl, sl                    map[uint8]int
		wBacks                    int
		published, publishedBases int
		dropped, passed           uint64
	)
	measured := false
	for attempt := 0; attempt < 10 && !measured; attempt++ {
		parent := parentOf()
		taps[weak.id].take()
		taps[strong.id].take()
		d0, p0 := link.dropped.Load(), link.passed.Load()
		published0, bases0 := enc.frames.Load(), enc.bases.Load()

		for deadline := time.Now().Add(sampleWindow); time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			if parentOf() != parent {
				break
			}
		}
		if parentOf() != parent {
			continue
		}
		wl, wBacks = taps[weak.id].take()
		sl, _ = taps[strong.id].take()
		published = int(enc.frames.Load() - published0)
		publishedBases = int(enc.bases.Load() - bases0)
		dropped, passed = link.dropped.Load()-d0, link.passed.Load()-p0
		measured = true
	}
	require.True(t, measured, "the weak viewer's feeder did not hold still for a measurement window")
	// A base frame lost is a base frame the encoder published whose packets never reached the
	// viewer: the RTP numbering cannot tell that on a capped stream, where upper layers are left
	// out upstream. One frame may straddle the edge of the window.
	wHoles := max(0, publishedBases-wl[0])
	t.Logf("weak viewer: frames per layer %v, %d base frames lost, %d reordered; link dropped %d of %d; cap now %s",
		wl, wHoles, wBacks, dropped, dropped+passed, capName(capOf()))
	t.Logf("strong viewer: frames per layer %v; %d frames (%d base) published, stream at %d kbps",
		sl, published, publishedBases, enc.bps.Load()/1000)

	require.Greater(t, published, 30, "too few frames to judge the layers")
	weakFrames := wl[0] + wl[1] + wl[2]
	// The encoder's pattern is TID 0, 2, 1, 2, so the base is about a quarter of its frames and
	// the top half. Count against what it actually published, not against a frame rate the test
	// machine may not keep up with under the race detector.
	assert.GreaterOrEqual(t, wl[0], published/4*8/10, "the weak viewer plays the base, about a quarter of the frames")
	assert.LessOrEqual(t, wl[1]+wl[2], weakFrames/4, "and little above it")
	assert.LessOrEqual(t, wHoles, 1, "no base frame is lost beyond one that straddles the window edge")
	// Out-of-order base frames are not a loss: the webview's jitter buffer reorders them. The
	// in-memory mesh schedules every frame on its own timer, so under the race detector a frame
	// can swap with the next; that is not the link dropping anything, but it must stay rare.
	assert.LessOrEqual(t, wBacks, 2, "base frames do not arrive out of order beyond the test mesh's own jitter")
	assert.GreaterOrEqual(t, sl[0]+sl[1]+sl[2], published*8/10, "the strong viewer keeps the frames the streamer published")
	assert.Greater(t, sl[2], published/2*7/10, "and the top layer, about half of them")
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
	perLayer, _ := tp.take()
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
