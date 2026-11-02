package media

import (
	"bytes"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A member that takes another's next sequence numbers first, with fragments it could not even
// seal, used to mute that member for everybody below it: the replay window marked the forged
// fragment as seen, and the real one that came after was dropped as a duplicate. Only units
// that authenticate count now, so the real stream behind the forgery still plays and is relayed.
func TestMediaDispatcher_ForgedFragmentsDoNotMuteTheirSource(t *testing.T) {
	coord, relay, leaf, squatter := pid(1), pid(2), pid(3), pid(4)
	key := newKey(t)
	sender := &mockSender{}
	consumer := &mockConsumer{}
	engine := tree.NewStreamTreeEngine()
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: coord, Epoch: 1, Subscribers: []types.PeerID{relay, leaf},
		ParentMap:        map[types.PeerID]types.PeerID{relay: coord, leaf: relay},
		DistributionTree: map[types.PeerID][]types.PeerID{coord: {relay}, relay: {leaf}},
	})
	d := NewMediaDispatcher(relay, sender, consumer, engine)
	defer d.Close()
	require.NoError(t, d.SetRoomKey(key))
	d.SetVoiceManifest(&tree.TreeManifest{
		Coordinator: coord,
		ParentMap:   map[types.PeerID]tree.ParentAssignment{relay: {PrimaryParent: coord}, leaf: {PrimaryParent: relay}, squatter: {PrimaryParent: coord}},
		ChildrenMap: map[types.PeerID][]types.PeerID{coord: {relay, squatter}, relay: {leaf}},
	})
	d.SetStreamWatching("s1", true)

	forge := func(kind FrameKind, stream string, seq uint32, idx, count uint8) []byte {
		return (&MediaPacket{Kind: kind, Seq: seq, Broadcaster: coord, StreamID: stream, FragIdx: idx, FragCount: count,
			Payload: bytes.Repeat([]byte{0xee}, 64)}).Encode()
	}
	for seq := uint32(1); seq <= 4; seq++ {
		require.NoError(t, d.ProcessIncomingAudio(forge(FrameAudio, "voice", seq, 0, 1), squatter))
		require.NoError(t, d.ProcessIncomingVideo(forge(FrameVideo, "s1", seq, 0, 1), squatter))
		require.NoError(t, d.ProcessIncomingVideo(forge(FrameVideo, "s1", seq, 0, 2), squatter))
	}
	for seq := uint32(1); seq <= 4; seq++ {
		a, err := SealUnit(&MediaPacket{Kind: FrameAudio, Seq: seq, Broadcaster: coord, StreamID: "voice", Payload: []byte("opus")}, key)
		require.NoError(t, err)
		require.NoError(t, d.ProcessIncomingAudio(a[0], coord))
		v, err := SealUnit(&MediaPacket{Kind: FrameVideo, Seq: seq, Broadcaster: coord, StreamID: "s1", Payload: bytes.Repeat([]byte{byte(seq)}, 900)}, key)
		require.NoError(t, err)
		require.NoError(t, d.ProcessIncomingVideo(v[0], coord))
	}

	audio, video := consumer.counts()
	assert.Equal(t, 4, audio, "every real voice unit plays")
	assert.Equal(t, 4, video, "every real video unit plays")
	real := 0
	require.Eventually(t, func() bool {
		real = 0
		for _, f := range sender.to(leaf) {
			if p, err := Decode(f.data); err == nil && !bytes.Contains(p.Payload, bytes.Repeat([]byte{0xee}, 64)) {
				real++
			}
		}
		return real >= 8
	}, time.Second, 5*time.Millisecond, "the real units reach the leaf")
	for _, f := range sender.to(leaf) {
		p, err := Decode(f.data)
		require.NoError(t, err)
		assert.NotEqual(t, bytes.Repeat([]byte{0xee}, 64), p.Payload, "a forged fragment is never relayed")
	}
}
