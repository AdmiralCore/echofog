package media

import (
	"encoding/binary"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rtpH264 builds an H.264 RTP packet of size bytes whose payload is an FU-A fragment of a NAL
// unit of type nal.
func rtpH264(seq uint16, marker bool, nal byte, size int) []byte {
	b := make([]byte, size)
	b[0] = 0x80
	b[1] = 96
	if marker {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:4], seq)
	b[12] = 0x60 | 28 // FU-A indicator
	b[13] = nal
	for i := 14; i < size; i++ {
		b[i] = byte(seq) ^ byte(i)
	}
	return b
}

func TestRTPKeyframeDetection(t *testing.T) {
	assert.True(t, rtpKeyframe(rtpH264(1, false, 5, 100)), "FU-A of an IDR slice")
	assert.False(t, rtpKeyframe(rtpH264(1, false, 1, 100)), "FU-A of an ordinary slice")
	assert.True(t, rtpKeyframeEnd(rtpH264(1, true, 5, 100)))
	assert.False(t, rtpKeyframeEnd(rtpH264(1, false, 5, 100)))

	single := []byte{0x80, 96, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0x65, 1, 2}
	assert.True(t, rtpKeyframe(single), "single NAL unit, IDR")
	single[12] = 0x41
	assert.False(t, rtpKeyframe(single))

	stap := []byte{0x80, 96, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0x78, 0, 2, 0x67, 9, 0, 2, 0x68, 9}
	assert.True(t, rtpKeyframe(stap), "STAP-A with SPS and PPS")

	ext := []byte{0x90, 96, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0xbe, 0xde, 0, 1, 1, 2, 3, 4, 0x65, 1}
	assert.True(t, rtpKeyframe(ext), "past a header extension")
	assert.False(t, rtpKeyframe([]byte{1, 2, 3}))
}

// A parity, its header and the largest fragment it covers fit one datagram even with the longest
// stream ID and the most fragments a parity names.
func TestFECParityFitsADatagram(t *testing.T) {
	id := strings.Repeat("s", MaxStreamIDLen)
	frags, err := sealUnit(&MediaPacket{Kind: FrameVideo, Seq: 1, StreamID: id, Payload: make([]byte, 5000)}, newKey(t), fecReserve)
	require.NoError(t, err)
	p, err := Decode(frags[0])
	require.NoError(t, err)

	members := make([]fecRef, fecGroupFrags)
	ff := make([]fecFrag, fecGroupFrags)
	for i := range members {
		members[i] = fecRef{seq: uint32(1 + 25*i), idx: 15, count: 16}
		ff[i] = fecFrag{uint32(i), make([]byte, len(p.Payload))}
	}
	pkt := (&MediaPacket{Kind: FrameVideoFEC, Seq: 1, StreamID: id, FragIdx: fecLanes - 1, FragCount: fecLanes, Payload: encodeFECPayload(members, ff)}).Encode()
	assert.LessOrEqual(t, len(pkt), DatagramBudget)
	for _, f := range frags {
		assert.LessOrEqual(t, len(f), DatagramBudget-fecReserve)
	}

	q, err := decodeFECPayload(encodeFECPayload(members, ff), time.Now())
	require.NoError(t, err)
	assert.Equal(t, members, q.members, "the members survive the round trip")
}

// sealedUnits seals n two-fragment units of one stream, of NAL type nal, starting at seq first.
func sealedUnits(t *testing.T, key []byte, bcast types.PeerID, first uint32, n int, nal byte) (wire [][]byte, rtp [][]byte) {
	for seq := first; seq < first+uint32(n); seq++ {
		r := rtpH264(uint16(seq), false, nal, 1800)
		frags, err := sealUnit(&MediaPacket{Kind: FrameVideo, Seq: seq, Broadcaster: bcast, StreamID: "s1", Payload: r}, key, fecReserve)
		require.NoError(t, err)
		require.Len(t, frags, 2)
		wire = append(wire, frags...)
		rtp = append(rtp, r)
	}
	return wire, rtp
}

func TestFECGroupsAreDealtInLanes(t *testing.T) {
	key := newKey(t)
	s := newFECSender()
	now := time.Now()
	add := func(first uint32, n int, nal byte) (out []fecParityOut) {
		wire, rtp := sealedUnits(t, key, pid(1), first, n, nal)
		for i := range rtp {
			out = append(out, s.add("s1", first+uint32(i), wire[2*i:2*i+2], rtp[i], now)...)
		}
		return out
	}
	parities := add(1, 20, 1)
	require.Len(t, parities, fecLanes, "forty fragments of ordinary frames: ten in each lane")
	for i, p := range parities {
		q, err := decodeFECPayload(p.payload, now)
		require.NoError(t, err)
		require.Len(t, q.members, fecGroupFrags)
		for j := 1; j < len(q.members); j++ {
			a, b := q.members[j-1], q.members[j]
			assert.Equal(t, uint32(fecLanes), 2*(b.seq-a.seq)+uint32(b.idx)-uint32(a.idx), "lane %d holds every fourth fragment", i)
		}
	}

	parities = add(21, 8, 5)
	assert.Len(t, parities, fecLanes, "sixteen fragments of a keyframe: four in each lane")

	assert.Empty(t, add(29, 1, 1))
	assert.Empty(t, s.flush(now))
	left := s.flush(now.Add(fecGroupAge + time.Millisecond))
	require.Len(t, left, 2, "groups a quiet stream left open go out by themselves")
	assert.Equal(t, uint32(29), left[0].first)
}

// fecPair is a broadcaster and one viewer of its stream.
func fecPair(t *testing.T) (b, v *MediaDispatcher, bs *mockSender, consumer *mockConsumer, viewer types.PeerID) {
	bcast, viewer := pid(1), pid(2)
	key := newKey(t)
	manifest := &tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 2, Subscribers: []types.PeerID{viewer},
		ParentMap:        map[types.PeerID]types.PeerID{viewer: bcast},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {viewer}},
	}
	be, ve := tree.NewStreamTreeEngine(), tree.NewStreamTreeEngine()
	be.ApplyManifest(manifest)
	ve.ApplyManifest(manifest)
	bs = &mockSender{}
	b = NewMediaDispatcher(bcast, bs, nil, be)
	consumer = &mockConsumer{}
	v = NewMediaDispatcher(viewer, &mockSender{}, consumer, ve)
	require.NoError(t, b.SetRoomKey(key))
	require.NoError(t, v.SetRoomKey(key))
	v.SetStreamWatching("s1", true)
	t.Cleanup(func() { b.Close(); v.Close() })
	return b, v, bs, consumer, viewer
}

// A burst of four fragments in a row, one in each lane, is rebuilt from the parities at once:
// every unit plays, nothing is asked for again and no keyframe is needed.
func TestFECRebuildsABurst(t *testing.T) {
	b, v, bs, consumer, viewer := fecPair(t)
	var nacks, losses atomic.Int32
	v.OnVideoNack = func(string, []NackEntry) { nacks.Add(1) }
	v.OnVideoLoss = func(string, types.PeerID) { losses.Add(1) }

	for seq := uint16(1); seq <= 20; seq++ {
		require.NoError(t, b.BroadcastLocalVideo("s1", rtpH264(seq, seq%5 == 0, 1, 1800)))
	}
	require.Eventually(t, func() bool { return len(bs.to(viewer)) == 44 }, time.Second, 5*time.Millisecond, "40 fragments and 4 parities")

	parities, n := 0, 0
	for _, f := range bs.to(viewer) {
		p, err := Decode(f.data)
		require.NoError(t, err)
		if p.Kind == FrameVideoFEC {
			parities++
		} else if n++; n >= 11 && n <= 14 { // fragments 11..14: units 6 and 7, both whole
			continue
		}
		require.NoError(t, v.ProcessIncomingVideo(f.data, pid(1)))
	}
	assert.Equal(t, fecLanes, parities)
	_, got := consumer.counts()
	require.Equal(t, 20, got)
	assert.Equal(t, uint64(4), v.Stats().Recovered)
	assert.Equal(t, uint64(4), v.StreamStats()["s1"].Recovered)
	consumer.mu.Lock()
	var got6 []byte
	for _, p := range consumer.videoPackets {
		if p.Seq == 6 {
			got6 = p.Payload
		}
	}
	consumer.mu.Unlock()
	assert.Equal(t, rtpH264(6, false, 1, 1800), got6)

	time.Sleep(reorderWindow + 50*time.Millisecond)
	assert.Zero(t, losses.Load())
	assert.Zero(t, nacks.Load(), "repaired before a NACK was due")
}

// A parity whose group lost two fragments waits; once a resend brings one back, it rebuilds the other.
func TestFECParityWaitsForASecondFragment(t *testing.T) {
	key := newKey(t)
	bcast := pid(1)
	s := newFECSender()
	r := newFECReceiver()
	now := time.Now()
	wire, rtp := sealedUnits(t, key, bcast, 1, 20, 1)
	var par []fecParityOut
	for i := range rtp {
		par = append(par, s.add("s1", uint32(i+1), wire[2*i:2*i+2], rtp[i], now)...)
	}
	require.Len(t, par, fecLanes)
	for i, w := range wire {
		if i == 2 || i == 6 { // both in lane 2
			continue
		}
		p, _ := Decode(w)
		assert.Empty(t, r.fragment(p, now))
	}
	var lane2 fecParityOut
	for _, p := range par {
		if p.lane == 2 {
			lane2 = p
		}
	}
	pp := (&MediaPacket{Kind: FrameVideoFEC, Seq: lane2.first, Broadcaster: bcast, StreamID: "s1", FragIdx: 2, FragCount: fecLanes, Payload: lane2.payload}).Encode()
	pkt, _ := Decode(pp)
	rebuilt, err := r.parity(pkt, now)
	require.NoError(t, err)
	assert.Nil(t, rebuilt, "two missing: nothing to rebuild yet")

	p, _ := Decode(wire[2]) // the resend
	out := r.fragment(p, now)
	require.Len(t, out, 1)
	assert.Equal(t, wire[6], out[0], "the rebuilt fragment is the sealed one, byte for byte")
	assert.Empty(t, r.fragment(p, now), "the parity is used up")

	_, err = r.parity(&MediaPacket{Kind: FrameVideoFEC, Payload: []byte{1, 2}}, now)
	assert.ErrorIs(t, err, errBadFEC)
}

func TestWantKeyframeWaitsForTheAnswer(t *testing.T) {
	b, v, bs, _, viewer := fecPair(t)
	assert.True(t, v.WantKeyframe("s1"))
	assert.False(t, v.WantKeyframe("s1"), "one is on its way")
	assert.True(t, v.WantKeyframe("other"), "streams are apart")

	require.NoError(t, b.BroadcastLocalVideo("s1", rtpH264(1, false, 5, 1800)))
	require.NoError(t, b.BroadcastLocalVideo("s1", rtpH264(2, true, 5, 1800)))
	require.Eventually(t, func() bool { return len(bs.to(viewer)) >= 4 }, time.Second, 5*time.Millisecond)
	for _, f := range bs.to(viewer) {
		require.NoError(t, v.ProcessIncomingVideo(f.data, pid(1)))
	}
	assert.True(t, v.WantKeyframe("s1"), "the keyframe came")

	v.mu.Lock()
	v.keyWanted["s1"] = time.Now().Add(-KeyframePending)
	v.mu.Unlock()
	assert.True(t, v.WantKeyframe("s1"), "a request that went unanswered is repeated")
}

func TestLossTimingFollowsTheRoundTrip(t *testing.T) {
	d := NewMediaDispatcher(pid(1), nil, nil, nil)
	defer d.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	wait, retry := d.lossTimingLocked("s")
	assert.Equal(t, reorderWindow, wait)
	assert.Equal(t, nackRetry, retry)

	d.sampleRTTLocked("s", 20*time.Millisecond)
	wait, retry = d.lossTimingLocked("s")
	assert.Equal(t, reorderWindow, wait, "a short path keeps the old window")
	assert.Equal(t, nackRetry, retry)

	d.rtt["s"] = 0
	for i := 0; i < 60; i++ {
		d.sampleRTTLocked("s", 300*time.Millisecond)
	}
	wait, retry = d.lossTimingLocked("s")
	assert.InDelta(t, float64(680*time.Millisecond), float64(wait), float64(5*time.Millisecond))
	assert.InDelta(t, float64(390*time.Millisecond), float64(retry), float64(5*time.Millisecond))

	d.sampleRTTLocked("s", 10*time.Second)
	wait, retry = d.lossTimingLocked("s")
	assert.Equal(t, maxLossWait, wait)
	assert.Equal(t, maxNackRetry, retry)
}

// On a path that loses 2% of packets at random, a unit of two fragments is lost about 4% of the
// time without repair. Parity alone, with no NACK at all, brings most of them back; what is left
// is groups that lost two packets, which NACKs then fetch.
func TestFECOnALossyPath(t *testing.T) {
	b, v, bs, consumer, viewer := fecPair(t)
	const units = 600
	for seq := uint16(1); seq <= units; seq++ {
		require.NoError(t, b.BroadcastLocalVideo("s1", rtpH264(seq, seq%8 == 0, 1, 1800)))
	}
	time.Sleep(2 * fecGroupAge)
	require.Eventually(t, func() bool { return len(bs.to(viewer)) >= 2*units+units/5 }, 2*time.Second, 5*time.Millisecond)

	rng := rand.New(rand.NewSource(7))
	dropped := 0
	for _, f := range bs.to(viewer) {
		if rng.Intn(100) < 2 {
			dropped++
			continue
		}
		require.NoError(t, v.ProcessIncomingVideo(f.data, pid(1)))
	}
	_, n := consumer.counts()
	t.Logf("dropped %d packets, %d of %d units played, %d fragments rebuilt", dropped, n, units, v.Stats().Recovered)
	assert.Greater(t, dropped, 10)
	assert.LessOrEqual(t, units-n, dropped/3, "without parity nearly every dropped packet costs a unit")
}

// Real paths lose packets in runs. With runs of one to four packets starting at 1% of packets, a
// run of two or more used to cost a unit every time; dealt in lanes, the parities rebuild them,
// except where two runs fall into the same groups.
func TestFECOnABurstyPath(t *testing.T) {
	b, v, bs, consumer, viewer := fecPair(t)
	const units = 600
	for seq := uint16(1); seq <= units; seq++ {
		require.NoError(t, b.BroadcastLocalVideo("s1", rtpH264(seq, seq%8 == 0, 1, 1800)))
	}
	time.Sleep(3 * fecGroupAge)
	require.Eventually(t, func() bool { return len(bs.to(viewer)) >= 2*units+2*units/fecGroupFrags }, 2*time.Second, 5*time.Millisecond)

	rng := rand.New(rand.NewSource(11))
	dropped, run := 0, 0
	for _, f := range bs.to(viewer) {
		if run == 0 && rng.Intn(100) == 0 {
			run = 1 + rng.Intn(fecLanes)
		}
		if run > 0 {
			run--
			dropped++
			continue
		}
		require.NoError(t, v.ProcessIncomingVideo(f.data, pid(1)))
	}
	_, n := consumer.counts()
	t.Logf("dropped %d packets in runs, %d of %d units played, %d fragments rebuilt", dropped, n, units, v.Stats().Recovered)
	assert.Greater(t, dropped, 15)
	assert.GreaterOrEqual(t, n, units*98/100, "runs no longer than the lanes are rebuilt")
}
