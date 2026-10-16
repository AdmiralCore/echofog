package media

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sentFrame struct {
	peer     types.PeerID
	kind     net_types.MessageType
	data     []byte
	reliable bool
}

type mockSender struct {
	mu      sync.Mutex
	sent    []sentFrame
	block   map[types.PeerID]chan struct{} // peers whose sends hang until the channel closes
	tooBig  bool
	failFor map[types.PeerID]bool
}

func (m *mockSender) record(peer types.PeerID, kind net_types.MessageType, data []byte, reliable bool) error {
	m.mu.Lock()
	gate := m.block[peer]
	fail := m.failFor[peer]
	m.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if fail {
		return errors.New("peer not connected")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentFrame{peer, kind, data, reliable})
	return nil
}

func (m *mockSender) SendRawFrame(peerID types.PeerID, msgType net_types.MessageType, payload []byte) error {
	return m.record(peerID, msgType, payload, true)
}

func (m *mockSender) SendUnreliableFrame(peerID types.PeerID, msgType net_types.MessageType, payload []byte) error {
	if m.tooBig {
		return network.ErrDatagramTooLarge
	}
	return m.record(peerID, msgType, payload, false)
}

func (m *mockSender) to(peer types.PeerID) []sentFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentFrame
	for _, f := range m.sent {
		if f.peer == peer {
			out = append(out, f)
		}
	}
	return out
}

func (m *mockSender) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

type mockConsumer struct {
	mu           sync.Mutex
	audioPackets []*MediaPacket
	videoPackets []*MediaPacket
}

func (m *mockConsumer) OnLocalAudio(packet *MediaPacket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audioPackets = append(m.audioPackets, packet)
}

func (m *mockConsumer) OnLocalVideo(packet *MediaPacket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.videoPackets = append(m.videoPackets, packet)
}

func (m *mockConsumer) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.audioPackets), len(m.videoPackets)
}

func pid(b byte) types.PeerID {
	var id types.PeerID
	id[0] = b
	return id
}

func newKey(t testing.TB) []byte {
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

func TestMediaPacket_SealOpenFragmentsAndAAD(t *testing.T) {
	key := newKey(t)
	payload := make([]byte, 3500) // > 3 fragments
	_, _ = rand.Read(payload)

	unit := &MediaPacket{Kind: FrameVideo, Seq: 77, Timestamp: 123, Broadcaster: pid(1), StreamID: "screen-1", Payload: payload}
	frags, err := SealUnit(unit, key)
	require.NoError(t, err)
	require.Len(t, frags, 4)

	var ct []byte
	for i, raw := range frags {
		assert.LessOrEqual(t, len(raw), MaxFragmentPayload+fixedHeaderLen+fragFieldsLen+len(unit.StreamID))
		p, err := Decode(raw)
		require.NoError(t, err)
		assert.Equal(t, uint8(i), p.FragIdx)
		assert.Equal(t, uint8(4), p.FragCount)
		assert.Equal(t, unit.Broadcaster, p.Broadcaster)
		ct = append(ct, p.Payload...)
	}
	hdr, _ := Decode(frags[0])
	plain, err := OpenUnit(hdr, ct, key)
	require.NoError(t, err)
	assert.Equal(t, payload, plain)

	// A relay re-labelling the broadcaster, stream or seq breaks authentication.
	forged := *hdr
	forged.Broadcaster = pid(66)
	_, err = OpenUnit(&forged, ct, key)
	assert.ErrorIs(t, err, ErrDecryptionFailed)
	forged = *hdr
	forged.Seq++
	_, err = OpenUnit(&forged, ct, key)
	assert.ErrorIs(t, err, ErrDecryptionFailed)

	_, err = OpenUnit(hdr, ct, newKey(t))
	assert.ErrorIs(t, err, ErrDecryptionFailed)

	_, err = SealUnit(unit, []byte("short"))
	assert.ErrorIs(t, err, ErrBadKey)
	_, err = SealUnit(&MediaPacket{Kind: FrameVideo, StreamID: "s", Payload: make([]byte, MaxFragments*MaxFragmentPayload)}, key)
	assert.ErrorIs(t, err, ErrUnitTooLarge)
}

func TestMediaPacket_DecodeRejectsMalformed(t *testing.T) {
	good := (&MediaPacket{Kind: FrameAudio, Seq: 1, StreamID: "voice", FragCount: 1, Payload: []byte("x")}).Encode()

	_, err := Decode(good[:10])
	assert.ErrorIs(t, err, ErrPacketTooShort)

	bad := append([]byte(nil), good...)
	bad[1] = 1
	_, err = Decode(bad)
	assert.ErrorIs(t, err, ErrBadPacketVersion)

	bad = append([]byte(nil), good...)
	bad[fixedHeaderLen+5+1] = 0 // frag count 0
	_, err = Decode(bad)
	assert.ErrorIs(t, err, ErrBadFragment)

	bad = append([]byte(nil), good...)
	bad[fixedHeaderLen+5] = 3 // idx >= count
	_, err = Decode(bad)
	assert.ErrorIs(t, err, ErrBadFragment)

	bad = append([]byte(nil), good...)
	bad[42] = 200 // stream id length beyond limit
	_, err = Decode(bad)
	assert.ErrorIs(t, err, ErrStreamIDTooLong)
}

func TestReplayWindow(t *testing.T) {
	var w replayWindow
	now := time.Now()
	assert.True(t, w.check(100, 0, now))
	assert.False(t, w.check(100, 0, now), "duplicate")
	assert.True(t, w.check(100, 1, now), "other fragment of same unit")
	assert.True(t, w.check(99, 0, now), "late but inside window")
	assert.True(t, w.check(2000, 0, now))
	assert.False(t, w.check(2000-replayWindowSize, 0, now), "too old")
	assert.True(t, w.check(2000-replayWindowSize+1, 0, now))

	// uint32 wrap-around
	var wrap replayWindow
	assert.True(t, wrap.check(^uint32(0)-1, 0, now))
	assert.True(t, wrap.check(^uint32(0), 0, now))
	assert.True(t, wrap.check(0, 0, now))
	assert.True(t, wrap.check(1, 0, now))
	assert.False(t, wrap.check(^uint32(0), 0, now))
}

func TestReassembler_OutOfOrderAndExpiry(t *testing.T) {
	key := newKey(t)
	payload := bytes.Repeat([]byte{7}, 2500)
	frags, err := SealUnit(&MediaPacket{Kind: FrameVideo, Seq: 5, Broadcaster: pid(1), StreamID: "s", Payload: payload}, key)
	require.NoError(t, err)
	require.Len(t, frags, 3)

	r := newReassembler()
	now := time.Now()
	for _, i := range []int{2, 0} {
		p, _ := Decode(frags[i])
		_, done := r.add(p, now)
		assert.False(t, done)
	}
	p, _ := Decode(frags[1])
	ct, done := r.add(p, now)
	require.True(t, done)
	plain, err := OpenUnit(p, ct, key)
	require.NoError(t, err)
	assert.Equal(t, payload, plain)

	p, _ = Decode(frags[0])
	_, done = r.add(p, now)
	assert.False(t, done)
	assert.Equal(t, 0, r.expire(now.Add(reassemblyTimeout/2)))
	assert.Equal(t, 1, r.expire(now.Add(reassemblyTimeout)))
}

func TestMediaDispatcher_VoiceFloodAndDelivery(t *testing.T) {
	coord, relay, leaf, other := pid(1), pid(2), pid(3), pid(4)
	key := newKey(t)
	sender := &mockSender{}
	consumer := &mockConsumer{}
	d := NewMediaDispatcher(relay, sender, consumer, tree.NewStreamTreeEngine())
	defer d.Close()
	require.NoError(t, d.SetRoomKey(key))
	d.SetVoiceManifest(&tree.TreeManifest{
		Coordinator: coord,
		ParentMap:   map[types.PeerID]tree.ParentAssignment{relay: {PrimaryParent: coord, StandbyParent: other}, leaf: {PrimaryParent: relay}},
		ChildrenMap: map[types.PeerID][]types.PeerID{coord: {relay, other}, relay: {leaf}},
	})

	frags, err := SealUnit(&MediaPacket{Kind: FrameAudio, Seq: 1, Broadcaster: coord, StreamID: "voice", Payload: []byte("opus-from-coord")}, key)
	require.NoError(t, err)
	require.NoError(t, d.ProcessIncomingAudio(frags[0], coord))
	require.NoError(t, d.ProcessIncomingAudio(frags[0], coord)) // duplicate

	assert.Eventually(t, func() bool { return len(sender.to(leaf)) == 1 }, time.Second, 5*time.Millisecond)
	assert.Empty(t, sender.to(coord), "never echo back to the sender")
	a, _ := consumer.counts()
	assert.Equal(t, 1, a)
	assert.Equal(t, []byte("opus-from-coord"), consumer.audioPackets[0].Payload)
	assert.Equal(t, uint64(1), d.Stats().Duplicates)
	assert.False(t, sender.to(leaf)[0].reliable, "media must use datagrams")

	// Audio from the leaf goes up to the coordinator.
	frags, _ = SealUnit(&MediaPacket{Kind: FrameAudio, Seq: 9, Broadcaster: leaf, StreamID: "voice", Payload: []byte("up")}, key)
	require.NoError(t, d.ProcessIncomingAudio(frags[0], leaf))
	assert.Eventually(t, func() bool { return len(sender.to(coord)) == 1 }, time.Second, 5*time.Millisecond)

	// Standby failover: upstream switches, an adopted orphan gets fed too.
	d.UseStandbyParent(other)
	orphan := pid(9)
	d.Adopt(orphan)
	require.NoError(t, d.BroadcastLocalAudio([]byte("mine")))
	assert.Eventually(t, func() bool {
		return len(sender.to(other)) == 1 && len(sender.to(orphan)) == 1 && len(sender.to(leaf)) == 2
	}, time.Second, 5*time.Millisecond)
	assert.Len(t, sender.to(coord), 1, "dead primary parent is bypassed")
}

func TestMediaDispatcher_RejectsTamperedAndPlaintext(t *testing.T) {
	self, peer := pid(2), pid(1)
	consumer := &mockConsumer{}
	sender := &mockSender{}
	d := NewMediaDispatcher(self, sender, consumer, tree.NewStreamTreeEngine())
	defer d.Close()

	// Without a key nothing is sent: no silent plaintext fallback.
	assert.ErrorIs(t, d.BroadcastLocalAudio([]byte("secret")), ErrBadKey)
	assert.ErrorIs(t, d.SetRoomKey([]byte("short")), ErrBadKey)
	require.NoError(t, d.SetRoomKey(newKey(t)))

	// Ciphertext under another key is dropped, never played as noise.
	frags, _ := SealUnit(&MediaPacket{Kind: FrameAudio, Seq: 1, Broadcaster: peer, StreamID: "voice", Payload: []byte("x")}, newKey(t))
	require.NoError(t, d.ProcessIncomingAudio(frags[0], peer))
	a, _ := consumer.counts()
	assert.Equal(t, 0, a)
	assert.Equal(t, uint64(1), d.Stats().DecryptFailures)

	// Garbage is rejected.
	assert.Error(t, d.ProcessIncomingVideo([]byte{1, 2, 3}, peer))
	assert.Equal(t, uint64(1), d.Stats().Malformed)
}

func TestMediaDispatcher_VideoTreeWatchingAndLoss(t *testing.T) {
	bcast, self, child := pid(1), pid(2), pid(3)
	key := newKey(t)
	engine := tree.NewStreamTreeEngine()
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 2, Subscribers: []types.PeerID{self, child},
		ParentMap:        map[types.PeerID]types.PeerID{self: bcast, child: self},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {self}, self: {child}},
	})
	sender := &mockSender{}
	consumer := &mockConsumer{}
	d := NewMediaDispatcher(self, sender, consumer, engine)
	defer d.Close()
	require.NoError(t, d.SetRoomKey(key))

	var losses atomic.Int32
	d.OnVideoLoss = func(string, types.PeerID) { losses.Add(1) }

	send := func(seq uint32) {
		frags, err := SealUnit(&MediaPacket{Kind: FrameVideo, Seq: seq, Broadcaster: bcast, StreamID: "s1", Payload: bytes.Repeat([]byte{byte(seq)}, 1800)}, key)
		require.NoError(t, err)
		for _, f := range frags {
			require.NoError(t, d.ProcessIncomingVideo(f, bcast))
		}
	}

	send(1) // relay only: not watching
	_, v := consumer.counts()
	assert.Equal(t, 0, v)
	assert.Eventually(t, func() bool { return len(sender.to(child)) == 2 }, time.Second, 5*time.Millisecond)

	d.SetStreamWatching("s1", true)
	send(2)
	send(4) // seq 3 lost
	_, v = consumer.counts()
	assert.Equal(t, 2, v)
	assert.Equal(t, int32(0), losses.Load(), "a gap waits out the reorder window")
	assert.Eventually(t, func() bool { return losses.Load() == 1 }, time.Second, 5*time.Millisecond)

	// merely reordered units are not loss
	send(5)
	send(7)
	send(6)
	time.Sleep(reorderWindow + 100*time.Millisecond)
	assert.Equal(t, int32(1), losses.Load())
	assert.Equal(t, bytes.Repeat([]byte{2}, 1800), consumer.videoPackets[0].Payload)
}

func TestOutbox_SlowPeerDoesNotBlockOthersAndFallsBack(t *testing.T) {
	slow, fast := pid(1), pid(2)
	gate := make(chan struct{})
	sender := &mockSender{block: map[types.PeerID]chan struct{}{slow: gate}}
	ob := NewOutbox(sender)
	defer ob.Close()

	for i := 0; i < videoQueueLen+500; i++ {
		ob.Send(slow, ClassVideo, net_types.TypeCallMediaVideo, []byte{1})
	}
	for i := 0; i < 50; i++ {
		ob.Send(fast, ClassVideo, net_types.TypeCallMediaVideo, []byte{2})
	}
	assert.Eventually(t, func() bool { return len(sender.to(fast)) == 50 }, time.Second, 5*time.Millisecond)
	assert.Greater(t, ob.Stats().Dropped[ClassVideo], uint64(400), "slow peer sheds load instead of buffering forever")
	close(gate)

	big := &mockSender{tooBig: true}
	ob2 := NewOutbox(big)
	defer ob2.Close()
	ob2.Send(fast, ClassAudio, net_types.TypeCallMediaAudio, []byte{3})
	assert.Eventually(t, func() bool { f := big.to(fast); return len(f) == 1 && f[0].reliable }, time.Second, 5*time.Millisecond)
}

func TestOutbox_ControlBeforeMedia(t *testing.T) {
	p := pid(1)
	gate := make(chan struct{})
	sender := &mockSender{block: map[types.PeerID]chan struct{}{p: gate}}
	ob := NewOutbox(sender)
	defer ob.Close()

	ob.Send(p, ClassVideo, net_types.TypeCallMediaVideo, []byte("v0")) // occupies the worker
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 5; i++ {
		ob.Send(p, ClassVideo, net_types.TypeCallMediaVideo, []byte("v"))
	}
	ob.Send(p, ClassAudio, net_types.TypeCallMediaAudio, []byte("a"))
	ob.Send(p, ClassControl, net_types.TypeCallSignaling, []byte("c"))
	close(gate)

	assert.Eventually(t, func() bool { return sender.count() == 8 }, time.Second, 5*time.Millisecond)
	f := sender.to(p)
	assert.Equal(t, "v0", string(f[0].data))
	assert.Equal(t, "c", string(f[1].data))
	assert.Equal(t, "a", string(f[2].data))
}

func BenchmarkDispatcher_Relay4K(b *testing.B) {
	bcast, self := pid(1), pid(2)
	key := newKey(b)
	engine := tree.NewStreamTreeEngine()
	children := []types.PeerID{pid(3), pid(4), pid(5)}
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s", Broadcaster: bcast, Epoch: 2,
		ParentMap:        map[types.PeerID]types.PeerID{self: bcast},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {self}, self: children},
	})
	d := NewMediaDispatcher(self, &mockSender{}, nil, engine)
	defer d.Close()
	_ = d.SetRoomKey(key)
	base, _ := SealUnit(&MediaPacket{Kind: FrameVideo, Seq: 0, Broadcaster: bcast, StreamID: "s", Payload: make([]byte, 900)}, key)
	raw := make([]byte, len(base[0]))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(raw, base[0])
		binary.BigEndian.PutUint32(raw[2:6], uint32(i)) // relays never decrypt, so a fresh seq is enough
		_ = d.ProcessIncomingVideo(raw, bcast)
	}
}
