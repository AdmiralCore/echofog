package media

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// With one key for the whole call, any member could seal a unit that names another member as its
// sender, and everybody played it as that member's voice or picture. Under a key whose members all
// sign, a unit has to carry its sender's signature: a member's forgery in another's name is dropped
// on the first hop (sound) or where it is watched (video), and the real units still play.
func TestMediaDispatcher_MemberCannotForgeAnotherMembersMedia(t *testing.T) {
	alice, bob, mallory := pid(1), pid(2), pid(3)
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	malloryPub, malloryPriv, _ := ed25519.GenerateKey(rand.Reader)
	bobPub, bobPriv, _ := ed25519.GenerateKey(rand.Reader)
	signers := map[types.PeerID]ed25519.PublicKey{alice: alicePub, bob: bobPub, mallory: malloryPub}
	roomKey, callKey := newKey(t), newKey(t)
	past := time.Now().Add(-MediaKeySwitch)

	engine := tree.NewStreamTreeEngine()
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: alice, Epoch: 1, Subscribers: []types.PeerID{bob},
		ParentMap:        map[types.PeerID]types.PeerID{bob: alice},
		DistributionTree: map[types.PeerID][]types.PeerID{alice: {bob}},
	})
	voice := &tree.TreeManifest{
		Coordinator: alice,
		ParentMap:   map[types.PeerID]tree.ParentAssignment{bob: {PrimaryParent: alice}, mallory: {PrimaryParent: alice}},
		ChildrenMap: map[types.PeerID][]types.PeerID{alice: {bob, mallory}},
	}
	consumer := &mockConsumer{}
	d := NewMediaDispatcher(bob, &mockSender{}, consumer, engine)
	defer d.Close()
	require.NoError(t, d.SetRoomKey(roomKey))
	d.SetSigningKey(bobPriv)
	require.NoError(t, d.SetMediaKey(callKey, signers, past))
	d.SetVoiceManifest(voice)
	d.SetStreamWatching("s1", true)
	assert.True(t, d.SignsUnits())

	// Alice's own dispatcher seals and signs as she sends
	a := NewMediaDispatcher(alice, nil, nil, nil)
	defer a.Close()
	require.NoError(t, a.SetRoomKey(roomKey))
	a.SetSigningKey(alicePriv)
	require.NoError(t, a.SetMediaKey(callKey, signers, past))
	real, err := a.seal(FrameAudio, "voice", 1, []byte("alice speaking"), 0)
	require.NoError(t, err)
	realVideo, err := a.seal(FrameVideo, "s1", 1, []byte("alice's screen"), fecReserve)
	require.NoError(t, err)

	// Mallory seals units in Alice's name: unsigned, signed with her own key, or with Alice's
	// signature taken from another unit
	forge := func(kind FrameKind, stream string, seq uint32, payload []byte) []byte {
		frags, err := SealUnit(&MediaPacket{Kind: kind, Seq: seq, Broadcaster: alice, StreamID: stream, Payload: payload}, callKey)
		require.NoError(t, err)
		return frags[0]
	}
	hdr := func(kind FrameKind, stream string, seq uint32) *MediaPacket {
		return &MediaPacket{Kind: kind, Seq: seq, Broadcaster: alice, StreamID: stream}
	}
	stolen := SignUnitPayload(hdr(FrameAudio, "voice", 9), []byte("alice said this once"), alicePriv)
	stolen = append([]byte("mallory's words"), stolen[len(stolen)-UnitSigLen:]...)
	for _, f := range [][]byte{
		forge(FrameAudio, "voice", 1, []byte("not alice")),
		forge(FrameAudio, "voice", 1, SignUnitPayload(hdr(FrameAudio, "voice", 1), []byte("not alice"), malloryPriv)),
		forge(FrameAudio, "voice", 1, stolen),
	} {
		require.NoError(t, d.ProcessIncomingAudio(f, mallory))
	}
	fake := SignUnitPayload(hdr(FrameVideo, "s1", 1), []byte("fake screen"), malloryPriv)
	require.NoError(t, d.ProcessIncomingVideo(forge(FrameVideo, "s1", 1, fake), alice))
	audio, video := consumer.counts()
	assert.Zero(t, audio, "a forgery in another's name is never played")
	assert.Zero(t, video)
	assert.Equal(t, uint64(4), d.Stats().Forged)

	require.NoError(t, d.ProcessIncomingAudio(real[0], alice))
	require.NoError(t, d.ProcessIncomingVideo(realVideo[0], alice))
	audio, video = consumer.counts()
	require.Equal(t, 1, audio, "the real unit still plays after the forgeries")
	require.Equal(t, 1, video)
	assert.Equal(t, []byte("alice speaking"), consumer.audioPackets[0].Payload, "the signature is not played")
	assert.Equal(t, []byte("alice's screen"), consumer.videoPackets[0].Payload)
}

// Video is taken only from the member the stream's tree feeds this node from: on a relay that does
// not watch the stream, and so checks no signature, another member's unit sealed under the call's
// key would otherwise take the real one's place for the whole subtree.
func TestMediaDispatcher_VideoOnlyFromTheFeeder(t *testing.T) {
	bcast, relay, leaf, other := pid(1), pid(2), pid(3), pid(4)
	key := newKey(t)
	engine := tree.NewStreamTreeEngine()
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 1, Subscribers: []types.PeerID{relay, leaf},
		ParentMap:        map[types.PeerID]types.PeerID{relay: bcast, leaf: relay},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {relay}, relay: {leaf}},
	})
	sender := &mockSender{}
	d := NewMediaDispatcher(relay, sender, nil, engine)
	defer d.Close()
	require.NoError(t, d.SetRoomKey(key))

	seal := func(seq uint32) []byte {
		f, err := SealUnit(&MediaPacket{Kind: FrameVideo, Seq: seq, Broadcaster: bcast, StreamID: "s1", Payload: []byte("frame")}, key)
		require.NoError(t, err)
		return f[0]
	}
	require.NoError(t, d.ProcessIncomingVideo(seal(1), other))
	require.NoError(t, d.ProcessIncomingVideo(seal(1), bcast))
	require.Eventually(t, func() bool { return len(sender.to(leaf)) == 1 }, time.Second, 5*time.Millisecond)

	// the tree moves the relay under another feeder: the old one is still heard for a moment
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 2, Subscribers: []types.PeerID{relay, leaf, other},
		ParentMap:        map[types.PeerID]types.PeerID{other: bcast, relay: other, leaf: relay},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {other}, other: {relay}, relay: {leaf}},
	})
	require.NoError(t, d.ProcessIncomingVideo(seal(2), bcast))
	require.NoError(t, d.ProcessIncomingVideo(seal(3), other))
	require.Eventually(t, func() bool { return len(sender.to(leaf)) == 3 }, time.Second, 5*time.Millisecond)
}
