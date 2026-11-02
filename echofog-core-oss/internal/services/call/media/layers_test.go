package media

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dyadic is the layer of each frame of an L1T3 stream, by its place in a group of four.
var dyadic = [4]uint8{0, 2, 1, 2}

// layeredFrames makes the RTP packets of an L1T3 stream as the desktop's packetizer does: a
// keyframe first, base frames three packets large, the others two. Each payload starts with the
// NAL header and then the frame's number.
func layeredFrames(t testing.TB, frames int) [][]byte {
	t.Helper()
	var out [][]byte
	seq := uint16(1000)
	for f := 0; f < frames; f++ {
		tid, key := dyadic[f%4], f == 0
		n := 2
		if tid == 0 {
			n = 3
		}
		for i := 0; i < n; i++ {
			nal := byte(0x41) // a reference slice
			if key {
				nal = 0x65
			} else if tid == 2 {
				nal = 0x01
			}
			payload := make([]byte, 600)
			payload[0] = nal
			binary.BigEndian.PutUint32(payload[1:5], uint32(f))
			p := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: seq, Timestamp: uint32(f) * 1500, SSRC: 7, Marker: i == n-1}, Payload: payload}
			m := videolayer.Mark{TID: tid, Start: i == 0, End: i == n-1, Key: key, Discardable: tid == 2, Index: uint16(i), Count: uint16(n)}
			p.Header.Extension, p.Header.ExtensionProfile = true, 0xBEDE
			require.NoError(t, p.Header.SetExtension(videolayer.ExtensionID, m.Data()))
			b, err := p.Marshal()
			require.NoError(t, err)
			out = append(out, b)
			seq++
		}
	}
	return out
}

// frameOf reads the frame number and layer of a delivered or relayed RTP packet.
func frameOf(t testing.TB, b []byte) (frame int, m videolayer.Mark, rtpSeq uint16) {
	t.Helper()
	var p rtp.Packet
	require.NoError(t, p.Unmarshal(b))
	m, _, ok := videolayer.Find(b)
	require.True(t, ok, "a delivered packet lost its mark")
	return int(binary.BigEndian.Uint32(p.Payload[1:5])), m, p.SequenceNumber
}

// layerTree is a broadcaster b feeding a relay r, which feeds kids.
func layerTree(t *testing.T, kids ...types.PeerID) (b, r *MediaDispatcher, bs, rs *mockSender, rc *mockConsumer, key []byte) {
	bcast, relay := pid(1), pid(2)
	key = newKey(t)
	m := &tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 2, Subscribers: append([]types.PeerID{relay}, kids...),
		ParentMap:        map[types.PeerID]types.PeerID{relay: bcast},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {relay}, relay: kids},
	}
	for _, k := range kids {
		m.ParentMap[k] = relay
	}
	be, re := tree.NewStreamTreeEngine(), tree.NewStreamTreeEngine()
	be.ApplyManifest(m)
	re.ApplyManifest(m)
	bs, rs, rc = &mockSender{}, &mockSender{}, &mockConsumer{}
	b = NewMediaDispatcher(bcast, bs, nil, be)
	r = NewMediaDispatcher(relay, rs, rc, re)
	require.NoError(t, b.SetRoomKey(key))
	require.NoError(t, r.SetRoomKey(key))
	t.Cleanup(func() { b.Close(); r.Close() })
	return b, r, bs, rs, rc, key
}

// broadcast sends packets from b and hands r what b sent it, in order.
func broadcast(t *testing.T, b, r *MediaDispatcher, bs *mockSender, pkts [][]byte) {
	t.Helper()
	for _, p := range pkts {
		require.NoError(t, b.BroadcastLocalVideo("s1", p))
	}
	b.flushParities(time.Now().Add(time.Second))
	time.Sleep(50 * time.Millisecond)
	for _, f := range bs.to(pid(2)) {
		require.NoError(t, r.ProcessIncomingVideo(f.data, pid(1)))
	}
}

// opened decrypts what a node sent one peer: its video units' plaintext in order, their hop caps,
// and how many parities of each class went.
func opened(t *testing.T, s *mockSender, to types.PeerID, key []byte) (units [][]byte, hops []uint8, parities map[uint8]int) {
	t.Helper()
	parities = map[uint8]int{}
	for _, f := range s.to(to) {
		p, err := Decode(f.data)
		require.NoError(t, err)
		if p.Kind == FrameVideoFEC {
			parities[parityClass(p)]++
			continue
		}
		if p.Kind != FrameVideo {
			continue
		}
		plain, err := OpenUnit(p, p.Payload, key)
		require.NoError(t, err, "a relayed fragment no longer opens")
		units = append(units, plain)
		hops = append(hops, p.HopCap)
	}
	return units, hops, parities
}

// The broadcaster counts the packets of each layer into their marks as it numbers the units.
func TestLayers_BroadcasterCountsEachLayer(t *testing.T) {
	b, _, bs, _, _, key := layerTree(t)
	for _, p := range layeredFrames(t, 8) {
		require.NoError(t, b.BroadcastLocalVideo("s1", p))
	}
	require.Eventually(t, func() bool { return len(bs.to(pid(2))) >= 18 }, time.Second, 5*time.Millisecond)
	units, hops, _ := opened(t, bs, pid(2), key)
	require.Equal(t, 2*3+6*2, len(units))
	var l0, l1 uint16
	for i, u := range units {
		_, m, _ := frameOf(t, u)
		if m.TID == 0 {
			l0++
		}
		if m.TID <= 1 {
			l1++
		}
		assert.Equal(t, l0, m.L0, "unit %d", i)
		assert.Equal(t, l1, m.L1, "unit %d", i)
		assert.Zero(t, hops[i], "a child that did not say it reads caps gets none")
	}
}

// A relay sends each child the layers its cap lets through, whole frames only: a child capped at
// the base gets base frames and their parities, one with every layer gets everything, and a child
// that never said it reads caps (an older node) gets the whole stream as before, without the cap.
func TestLayers_RelayCutsLayersPerChild(t *testing.T) {
	weak, strong, old := pid(3), pid(4), pid(5)
	b, r, bs, rs, _, key := layerTree(t, weak, strong, old)
	r.SetChildLayers("s1", weak, true, capBase)
	r.SetChildLayers("s1", strong, true, capAll)
	broadcast(t, b, r, bs, layeredFrames(t, 16))
	time.Sleep(50 * time.Millisecond)

	byFrame := func(units [][]byte) map[int]int {
		out := map[int]int{}
		for _, u := range units {
			f, _, _ := frameOf(t, u)
			out[f]++
		}
		return out
	}
	units, hops, par := opened(t, rs, weak, key)
	frames := byFrame(units)
	assert.Len(t, frames, 4, "the base frames of 16")
	for f, n := range frames {
		assert.Zero(t, f%4, "frame %d is above the base", f)
		assert.Equal(t, 3, n, "base frame %d went whole", f)
	}
	for _, h := range hops {
		assert.Equal(t, capBase+1, h)
	}
	assert.NotZero(t, par[capBase])
	assert.Zero(t, par[capMid]+par[capAll], "no parity of a layer the child does not get")

	units, hops, par = opened(t, rs, strong, key)
	assert.Len(t, byFrame(units), 16)
	assert.Len(t, units, 4*3+12*2)
	for _, h := range hops {
		assert.Equal(t, capAll+1, h)
	}
	assert.NotZero(t, par[capAll])

	units, hops, _ = opened(t, rs, old, key)
	assert.Len(t, units, 4*3+12*2)
	for _, h := range hops {
		assert.Zero(t, h)
	}
	for _, f := range rs.to(old) {
		assert.True(t, f.data[fragIdxOffset(f.data)]&hopCapBit == 0, "an older node would drop a fragment with a cap")
	}
}

// A viewer capped at the base gets frames at a quarter of the rate: nothing it was not sent counts
// as lost, nothing is asked for again, and its webview sees packets numbered without holes. One
// base packet lost on the way is asked for alone.
func TestLayers_CappedViewerTellsLeftOutFromLost(t *testing.T) {
	viewer := pid(3)
	b, r, bs, rs, _, key := layerTree(t, viewer)
	r.SetChildLayers("s1", viewer, true, capBase)
	broadcast(t, b, r, bs, layeredFrames(t, 20))
	time.Sleep(50 * time.Millisecond)

	ve := tree.NewStreamTreeEngine()
	ve.ApplyManifest(&tree.StreamManifest{StreamID: "s1", Broadcaster: pid(1), Epoch: 2, Subscribers: []types.PeerID{pid(2), viewer},
		ParentMap: map[types.PeerID]types.PeerID{pid(2): pid(1), viewer: pid(2)}, DistributionTree: map[types.PeerID][]types.PeerID{pid(1): {pid(2)}, pid(2): {viewer}}})
	vc := &mockConsumer{}
	v := NewMediaDispatcher(viewer, &mockSender{}, vc, ve)
	defer v.Close()
	require.NoError(t, v.SetRoomKey(key))
	v.SetStreamWatching("s1", true)
	var nackMu sync.Mutex
	var nacked []uint32
	var losses atomic.Int32
	v.OnVideoNack = func(_ string, lost []NackEntry) {
		nackMu.Lock()
		defer nackMu.Unlock()
		for _, e := range lost {
			nacked = append(nacked, e.Seq)
		}
	}
	v.OnVideoLoss = func(string, types.PeerID) { losses.Add(1) }

	var dropped uint32
	videoSeen := 0
	for _, f := range rs.to(viewer) {
		p, err := Decode(f.data)
		require.NoError(t, err)
		if p.Kind == FrameVideoFEC {
			continue // the parities would rebuild it: this is about what is asked for
		}
		if videoSeen++; videoSeen == 8 { // the second packet of the third base frame
			dropped = p.Seq
			continue
		}
		require.NoError(t, v.processVideo(f.data, pid(2), false))
	}
	time.Sleep(nackDelay + 40*time.Millisecond)
	nackMu.Lock()
	assert.Equal(t, []uint32{dropped}, dedupe(nacked), "only the lost base packet is asked for")
	nackMu.Unlock()

	vc.mu.Lock()
	var seqs []uint16
	for _, p := range vc.videoPackets[:min(len(vc.videoPackets), 14)] {
		f, m, s := frameOf(t, p.Payload)
		assert.Zero(t, f%4, "frame %d is above the base", f)
		assert.Zero(t, m.TID)
		seqs = append(seqs, s)
	}
	vc.mu.Unlock()
	require.Len(t, seqs, 5*3-1)
	for i := 1; i < len(seqs); i++ {
		want := uint16(1)
		if i == 7 {
			want = 2 // the lost one
		}
		assert.Equal(t, want, seqs[i]-seqs[i-1], "packet %d: the webview sees a hole where nothing was lost", i)
	}

	// the resend comes: the gap is whole, nothing more is lost
	frags, _, _ := r.ring.lookup("s1", dropped, 0, time.Now())
	require.Len(t, frags, 1)
	_ = v.processVideo(withHopCap(frags[0], capBase), pid(2), false)
	time.Sleep(maxLossWait)
	assert.Zero(t, losses.Load(), "no keyframe was needed")
	vc.mu.Lock()
	last := vc.videoPackets[len(vc.videoPackets)-1]
	vc.mu.Unlock()
	_, _, s := frameOf(t, last.Payload)
	assert.Equal(t, seqs[6]+1, s, "the resent packet takes the place its hole kept")
}

func dedupe(s []uint32) []uint32 {
	var out []uint32
	seen := map[uint32]bool{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// A cap that changes waits for the next base frame, from which the layers above it decode, and the
// numbers the webview sees go on without a hole or a step back across the change.
func TestLayers_CapChangeWaitsForABaseFrame(t *testing.T) {
	viewer := pid(3)
	b, r, bs, rs, _, key := layerTree(t, viewer)
	r.SetChildLayers("s1", viewer, true, capBase)
	pkts := layeredFrames(t, 24)
	half := 0
	for i, p := range pkts { // frames 0-9, then the cap goes up in the middle of a group of four
		if f, _, _ := frameOf(t, p); f == 10 {
			half = i
			break
		}
	}
	broadcast(t, b, r, bs, pkts[:half])
	r.SetChildLayers("s1", viewer, true, capAll)
	before := len(bs.to(pid(2)))
	for _, p := range pkts[half:] {
		require.NoError(t, b.BroadcastLocalVideo("s1", p))
	}
	time.Sleep(50 * time.Millisecond)
	for _, f := range bs.to(pid(2))[before:] {
		require.NoError(t, r.ProcessIncomingVideo(f.data, pid(1)))
	}
	time.Sleep(50 * time.Millisecond)

	units, _, _ := opened(t, rs, viewer, key)
	var frames []int
	for _, u := range units {
		f, _, _ := frameOf(t, u)
		if len(frames) == 0 || frames[len(frames)-1] != f {
			frames = append(frames, f)
		}
	}
	assert.Equal(t, []int{0, 4, 8, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}, frames,
		"frames 10 and 11 wait for the base frame 12")

	ve := tree.NewStreamTreeEngine()
	ve.ApplyManifest(&tree.StreamManifest{StreamID: "s1", Broadcaster: pid(1), Epoch: 2, Subscribers: []types.PeerID{pid(2), viewer},
		ParentMap: map[types.PeerID]types.PeerID{pid(2): pid(1), viewer: pid(2)}, DistributionTree: map[types.PeerID][]types.PeerID{pid(1): {pid(2)}, pid(2): {viewer}}})
	vc := &mockConsumer{}
	v := NewMediaDispatcher(viewer, &mockSender{}, vc, ve)
	defer v.Close()
	require.NoError(t, v.SetRoomKey(key))
	v.SetStreamWatching("s1", true)
	var nacks atomic.Int32
	v.OnVideoNack = func(string, []NackEntry) { nacks.Add(1) }
	for _, f := range rs.to(viewer) {
		require.NoError(t, v.ProcessIncomingVideo(f.data, pid(2)))
	}
	time.Sleep(reorderWindow + 50*time.Millisecond)
	assert.Zero(t, nacks.Load())
	vc.mu.Lock()
	defer vc.mu.Unlock()
	for i := 1; i < len(vc.videoPackets); i++ {
		_, _, a := frameOf(t, vc.videoPackets[i-1].Payload)
		_, _, s := frameOf(t, vc.videoPackets[i].Payload)
		assert.Equal(t, uint16(1), s-a, "packet %d", i)
	}
}

// A relay whose own feed is cut passes its cap on: its child, which the relay lets have every
// layer, still gets the cap the stream came to the relay with, and numbers what it gets in it.
func TestLayers_CapPassesDownTheTree(t *testing.T) {
	relay, kid := pid(2), pid(3)
	b, r, bs, _, _, key := layerTree(t)
	_ = r
	m := &tree.StreamManifest{StreamID: "s1", Broadcaster: pid(1), Epoch: 3, Subscribers: []types.PeerID{relay, kid},
		ParentMap: map[types.PeerID]types.PeerID{relay: pid(1), kid: relay}, DistributionTree: map[types.PeerID][]types.PeerID{pid(1): {relay}, relay: {kid}}}
	re := tree.NewStreamTreeEngine()
	re.ApplyManifest(m)
	rs := &mockSender{}
	r2 := NewMediaDispatcher(relay, rs, nil, re)
	defer r2.Close()
	require.NoError(t, r2.SetRoomKey(key))
	r2.SetChildLayers("s1", kid, true, capAll)
	b.SetChildLayers("s1", relay, true, capMid)
	broadcast(t, b, r2, bs, layeredFrames(t, 16))
	time.Sleep(50 * time.Millisecond)
	units, hops, par := opened(t, rs, kid, key)
	assert.Len(t, units, 4*3+4*2, "base and middle frames")
	for i, u := range units {
		_, mk, _ := frameOf(t, u)
		assert.LessOrEqual(t, mk.TID, uint8(1))
		assert.Equal(t, capMid+1, hops[i])
	}
	assert.Zero(t, par[capAll])
}

// A child that asks again for a unit of a frame its cap left out gets nothing: it was never owed.
// A unit it was sent comes again with its cap.
func TestLayers_NackOfALeftOutFrameIsNotServed(t *testing.T) {
	viewer := pid(3)
	b, r, bs, rs, _, _ := layerTree(t, viewer)
	r.SetChildLayers("s1", viewer, true, capBase)
	pkts := layeredFrames(t, 4)
	broadcast(t, b, r, bs, pkts)
	time.Sleep(50 * time.Millisecond)
	sent := len(rs.to(viewer))
	// units 1-3 are the keyframe, 4-5 frame 1 (layer 2), 6-7 frame 2 (layer 1)
	assert.Empty(t, r.ServeNack(viewer, "s1", []NackEntry{{Seq: 4}, {Seq: 6}}))
	time.Sleep(30 * time.Millisecond)
	assert.Len(t, rs.to(viewer), sent, "nothing resent")
	r.ServeNack(viewer, "s1", []NackEntry{{Seq: 2}})
	require.Eventually(t, func() bool { return len(rs.to(viewer)) == sent+1 }, time.Second, 5*time.Millisecond)
	p, err := Decode(rs.to(viewer)[sent].data)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), p.Seq)
	assert.Equal(t, capBase+1, p.HopCap)
}

func TestLayers_HopCapOnTheWire(t *testing.T) {
	p := &MediaPacket{Kind: FrameVideo, Seq: 5, StreamID: "s1", FragIdx: 3, FragCount: 9, Payload: []byte{1, 2}, HopCap: capMid + 1}
	raw := p.Encode()
	got, err := Decode(raw)
	require.NoError(t, err)
	assert.Equal(t, uint8(3), got.FragIdx)
	assert.Equal(t, capMid+1, got.HopCap)
	clean := withoutHopCap(raw)
	assert.NotSame(t, &raw[0], &clean[0])
	got, err = Decode(clean)
	require.NoError(t, err)
	assert.Zero(t, got.HopCap)
	assert.Equal(t, uint8(3), got.FragIdx)
	again, _ := Decode(withHopCap(clean, capAll))
	assert.Equal(t, capAll+1, again.HopCap)
	plain := (&MediaPacket{Kind: FrameVideo, StreamID: "s1", FragCount: 1}).Encode()
	assert.Equal(t, &plain[0], &withoutHopCap(plain)[0], "a fragment without a cap is not copied")
}

func TestLayers_GapCandidates(t *testing.T) {
	mk := func(tid uint8, idx, count, l0, l1 uint16, frame uint32, cap uint8) unitLayer {
		return unitLayer{marked: true, mark: videolayer.Mark{TID: tid, Index: idx, Count: count, L0: l0, L1: l1}, frame: frame, cap: cap}
	}
	// base frame of 3 at 10-12, frames above at 13-16 left out, next base frame at 17
	last := mk(0, 2, 3, 3, 3, 10, capBase)
	c, e := layerGap(12, last, 17, mk(0, 0, 3, 4, 5, 17, capBase))
	assert.Empty(t, c)
	assert.Zero(t, e)
	// ... with the first packet of the next base frame lost
	c, e = layerGap(12, last, 18, mk(0, 1, 3, 5, 6, 17, capBase))
	assert.Equal(t, []uint32{17}, c)
	assert.Equal(t, 1, e)
	// ... with the last packet of the base frame lost too
	c, e = layerGap(11, mk(0, 1, 3, 2, 2, 10, capBase), 18, mk(0, 1, 3, 5, 6, 17, capBase))
	assert.Equal(t, []uint32{12, 17}, c)
	assert.Equal(t, 2, e)
	// a whole base frame lost: every unit between is a candidate, two of them come
	c, e = layerGap(12, last, 24, mk(0, 0, 3, 6, 9, 24, capBase))
	assert.Len(t, c, 11)
	assert.Equal(t, 2, e)
	// the whole stream: everything skipped over was lost
	c, e = layerGap(12, mk(0, 2, 3, 3, 3, 10, capAll), 15, mk(2, 0, 2, 3, 4, 15, capAll))
	assert.Equal(t, []uint32{13, 14}, c)
	assert.Equal(t, 2, e)
}

// The parities a capped child gets cover only what it gets: a base packet lost on the way is
// rebuilt from them at once, with nothing asked for again.
func TestLayers_CappedViewerRepairsFromItsParities(t *testing.T) {
	viewer := pid(3)
	b, r, bs, rs, _, key := layerTree(t, viewer)
	r.SetChildLayers("s1", viewer, true, capBase)
	broadcast(t, b, r, bs, layeredFrames(t, 40))
	time.Sleep(50 * time.Millisecond)

	ve := tree.NewStreamTreeEngine()
	ve.ApplyManifest(&tree.StreamManifest{StreamID: "s1", Broadcaster: pid(1), Epoch: 2, Subscribers: []types.PeerID{pid(2), viewer},
		ParentMap: map[types.PeerID]types.PeerID{pid(2): pid(1), viewer: pid(2)}, DistributionTree: map[types.PeerID][]types.PeerID{pid(1): {pid(2)}, pid(2): {viewer}}})
	vc := &mockConsumer{}
	v := NewMediaDispatcher(viewer, &mockSender{}, vc, ve)
	defer v.Close()
	require.NoError(t, v.SetRoomKey(key))
	v.SetStreamWatching("s1", true)
	var nacks, losses atomic.Int32
	v.OnVideoNack = func(string, []NackEntry) { nacks.Add(1) }
	v.OnVideoLoss = func(string, types.PeerID) { losses.Add(1) }
	video := 0
	for _, f := range rs.to(viewer) {
		if p, _ := Decode(f.data); p.Kind == FrameVideo {
			if video++; video == 11 {
				continue
			}
		}
		require.NoError(t, v.ProcessIncomingVideo(f.data, pid(2)))
	}
	_, got := vc.counts()
	assert.Equal(t, 10*3, got, "every base packet plays")
	assert.Equal(t, uint64(1), v.Stats().Recovered)
	time.Sleep(reorderWindow + 50*time.Millisecond)
	assert.Zero(t, nacks.Load())
	assert.Zero(t, losses.Load())
}
