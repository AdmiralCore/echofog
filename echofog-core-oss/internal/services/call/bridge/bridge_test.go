package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUI plays the browser side of the loopback with a second Pion peer connection.
type fakeUI struct {
	pc         *webrtc.PeerConnection
	mic, cam   *webrtc.TrackLocalStaticRTP
	screen     *webrtc.TrackLocalStaticRTP
	screenSnd  *webrtc.RTPSender
	mu         sync.Mutex
	got        map[string][]*rtp.Packet // MediaStream id -> packets
	receivers  map[string]*webrtc.RTPReceiver
	tracks     map[string]*webrtc.TrackRemote
	screenPLIs int
}

func newFakeUI(t *testing.T, offer string) (*fakeUI, string) {
	t.Helper()
	me := &webrtc.MediaEngine{}
	require.NoError(t, me.RegisterDefaultCodecs())
	for _, k := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
		require.NoError(t, me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI}, k))
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	ui := &fakeUI{pc: pc, got: map[string][]*rtp.Packet{}, receivers: map[string]*webrtc.RTPReceiver{}, tracks: map[string]*webrtc.TrackRemote{}}

	pc.OnTrack(func(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
		ui.mu.Lock()
		ui.receivers[tr.StreamID()] = rcv
		ui.tracks[tr.StreamID()] = tr
		ui.mu.Unlock()
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			ui.mu.Lock()
			ui.got[tr.StreamID()] = append(ui.got[tr.StreamID()], p)
			ui.mu.Unlock()
		}
	})

	require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}))
	require.Len(t, pc.GetTransceivers(), len(localRoles)+AudioSlots+VideoSlots)

	mk := func(mime, id string) *webrtc.TrackLocalStaticRTP {
		tr, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mime}, id, "ui-"+id)
		require.NoError(t, err)
		return tr
	}
	ui.mic, ui.cam, ui.screen = mk(webrtc.MimeTypeOpus, "mic"), mk(webrtc.MimeTypeH264, "cam"), mk(webrtc.MimeTypeH264, "screen")
	_, err = pc.AddTrack(ui.mic)
	require.NoError(t, err)
	_, err = pc.AddTrack(ui.cam)
	require.NoError(t, err)
	ui.screenSnd, err = pc.AddTrack(ui.screen)
	require.NoError(t, err)
	go func() {
		for {
			pkts, _, err := ui.screenSnd.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					ui.mu.Lock()
					ui.screenPLIs++
					ui.mu.Unlock()
				}
			}
		}
	}()

	answer, err := pc.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(answer))
	<-gathered
	return ui, pc.LocalDescription().SDP
}

func (u *fakeUI) packets(stream string) []*rtp.Packet {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*rtp.Packet(nil), u.got[stream]...)
}

func rtpUnit(t *testing.T, ssrc uint32, seq uint16, ts uint32, payload string) []byte {
	t.Helper()
	p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}, Payload: []byte(payload)}
	require.NoError(t, p.Header.SetExtension(3, []byte{1})) // negotiated by the broadcaster's session, not ours
	raw, err := p.Marshal()
	require.NoError(t, err)
	return raw
}

func peer(b byte) types.PeerID {
	var id types.PeerID
	id[0] = b
	return id
}

func TestBridge_PerSourceTracksRolesAndKeyframes(t *testing.T) {
	type local struct {
		role    string
		payload []byte
	}
	var (
		mu       sync.Mutex
		locals   []local
		slots    []SlotEvent
		keyframe []string
	)
	b, err := NewLocalMediaBridge(Options{
		OnLocalRTP:       func(role string, p []byte) { mu.Lock(); locals = append(locals, local{role, p}); mu.Unlock() },
		OnSlot:           func(ev SlotEvent) { mu.Lock(); slots = append(slots, ev); mu.Unlock() },
		OnKeyframeNeeded: func(s string) { mu.Lock(); keyframe = append(keyframe, s); mu.Unlock() },
	})
	require.NoError(t, err)
	defer b.Close()

	offer, err := b.CreateOffer()
	require.NoError(t, err)
	assert.Contains(t, offer, "a=candidate", "offer must carry ICE candidates: the UI has no trickle channel")
	assert.Equal(t, 2+VideoSlots, strings.Count(offer, "m=video"), "camera + screen + playback slots")
	assert.Equal(t, 1+AudioSlots, strings.Count(offer, "m=audio"), "mic + playback slots")
	ui, answer := newFakeUI(t, offer)
	defer ui.pc.Close()
	require.NoError(t, b.SetRemoteAnswer(answer))

	connected := make(chan struct{})
	ui.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("loopback did not connect")
	}

	// 1. UI capture tracks arrive tagged with the right role.
	require.Eventually(t, func() bool {
		_ = ui.mic.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, SSRC: 1}, Payload: []byte("mic")})
		_ = ui.cam.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, SSRC: 2}, Payload: []byte("cam")})
		_ = ui.screen.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, SSRC: 3}, Payload: []byte("scr")})
		mu.Lock()
		defer mu.Unlock()
		seen := map[string]string{}
		for _, l := range locals {
			var p rtp.Packet
			if p.Unmarshal(l.payload) == nil {
				seen[l.role] = string(p.Payload)
			}
		}
		return seen[RoleMic] == "mic" && seen[RoleCamera] == "cam" && seen[RoleScreen] == "scr"
	}, 5*time.Second, 50*time.Millisecond)

	// 2. Two speakers land on two different tracks, extensions stripped.
	alice, bob := peer(1), peer(2)
	for i := uint16(0); i < 5; i++ {
		require.NoError(t, b.PushRemoteAudio(&media.MediaPacket{Broadcaster: alice, StreamID: "voice", Payload: rtpUnit(t, 0xA, 100+i, 960*uint32(i), "alice")}))
		require.NoError(t, b.PushRemoteAudio(&media.MediaPacket{Broadcaster: bob, StreamID: "voice", Payload: rtpUnit(t, 0xB, 7000+i, 960*uint32(i), "bob")}))
	}
	require.Eventually(t, func() bool { return len(ui.packets("efslot-a0")) == 5 && len(ui.packets("efslot-a1")) == 5 }, 5*time.Second, 20*time.Millisecond)
	a0, a1 := ui.packets("efslot-a0"), ui.packets("efslot-a1")
	assert.Equal(t, "alice", string(a0[0].Payload))
	assert.Equal(t, "bob", string(a1[0].Payload))
	assert.Nil(t, a0[0].Header.GetExtension(3), "foreign header extension leaked")

	mu.Lock()
	require.Len(t, slots, 2)
	assert.Equal(t, SlotEvent{Slot: "a0", Kind: "audio", Peer: alice.String(), StreamID: "voice", Active: true}, slots[0])
	mu.Unlock()

	// 3. A watched stream gets a video slot; a released slot is reused with continuous numbering.
	for i := uint16(0); i < 3; i++ {
		require.NoError(t, b.PushRemoteVideo(&media.MediaPacket{Broadcaster: alice, StreamID: "s1", Payload: rtpUnit(t, 0x51, 500+i, 3000*uint32(i), "s1")}))
	}
	require.Eventually(t, func() bool { return len(ui.packets("efslot-v0")) == 3 }, 5*time.Second, 20*time.Millisecond)
	b.ReleaseVideo("s1")
	for i := uint16(0); i < 3; i++ {
		require.NoError(t, b.PushRemoteVideo(&media.MediaPacket{Broadcaster: bob, StreamID: "s2", Payload: rtpUnit(t, 0x52, 40000+i, 9000*uint32(i), "s2")}))
	}
	require.Eventually(t, func() bool { return len(ui.packets("efslot-v0")) == 6 }, 5*time.Second, 20*time.Millisecond)
	v0 := ui.packets("efslot-v0")
	for i := 1; i < len(v0); i++ {
		assert.Equal(t, v0[i-1].SequenceNumber+1, v0[i].SequenceNumber, "sequence must stay continuous across owners")
	}
	assert.Equal(t, "s2", string(v0[5].Payload))
	assert.Len(t, b.Slots(), 3) // a0, a1, v0

	// 4. The UI decoder asking for a keyframe reaches the stream on that slot.
	ui.mu.Lock()
	tr := ui.tracks["efslot-v0"]
	ui.mu.Unlock()
	require.NoError(t, ui.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())}}))
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(keyframe) > 0 && keyframe[0] == "s2" }, 5*time.Second, 20*time.Millisecond)

	// 5. Network keyframe requests reach the UI screen encoder.
	require.NoError(t, b.RequestKeyframe(RoleScreen))
	require.Eventually(t, func() bool { ui.mu.Lock(); defer ui.mu.Unlock(); return ui.screenPLIs > 0 }, 5*time.Second, 20*time.Millisecond)

	// Garbage is rejected.
	assert.Error(t, b.PushRemoteAudio(&media.MediaPacket{Broadcaster: alice, Payload: []byte{1}}))
	require.NoError(t, b.Close())
}

func TestBridge_SlotExhaustionAndIdleReuse(t *testing.T) {
	b, err := NewLocalMediaBridge(Options{})
	require.NoError(t, err)
	defer b.Close()
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }

	for i := 0; i < VideoSlots; i++ {
		require.NoError(t, b.PushRemoteVideo(&media.MediaPacket{Broadcaster: peer(1), StreamID: string(rune('a' + i)), Payload: rtpUnit(t, 1, 1, 1, "x")}))
	}
	assert.ErrorIs(t, b.PushRemoteVideo(&media.MediaPacket{Broadcaster: peer(1), StreamID: "late", Payload: rtpUnit(t, 1, 1, 1, "x")}), ErrNoSlot)

	now = now.Add(slotReuseIdle)
	require.NoError(t, b.PushRemoteVideo(&media.MediaPacket{Broadcaster: peer(1), StreamID: "late", Payload: rtpUnit(t, 1, 1, 1, "x")}))
	ids := map[string]bool{}
	for _, s := range b.Slots() {
		ids[s.StreamID] = true
	}
	assert.True(t, ids["late"])
	assert.Len(t, ids, VideoSlots)
}

func TestSlotRewrite_ContinuousAcrossSources(t *testing.T) {
	s := &slot{clock: 90000}
	t0 := time.Unix(0, 0)
	p := &rtp.Packet{Header: rtp.Header{SequenceNumber: 65534, Timestamp: 1000, SSRC: 1}}
	s.rewrite(p, t0)
	assert.Equal(t, uint16(65534), p.SequenceNumber)
	p = &rtp.Packet{Header: rtp.Header{SequenceNumber: 65535, Timestamp: 4000, SSRC: 1}}
	s.rewrite(p, t0.Add(33*time.Millisecond))

	// new source with unrelated numbering, 1 s later
	s.rebase = true
	p = &rtp.Packet{Header: rtp.Header{SequenceNumber: 12, Timestamp: 777, SSRC: 2}}
	s.rewrite(p, t0.Add(time.Second+33*time.Millisecond))
	assert.Equal(t, uint16(0), p.SequenceNumber, "wraps to the next number")
	assert.InDelta(t, 4000+90000, float64(p.Timestamp), 10, "timestamp advances by the wall-clock pause")

	// reordered packet of the same source keeps its relative position
	p = &rtp.Packet{Header: rtp.Header{SequenceNumber: 11, Timestamp: 700, SSRC: 2}}
	s.rewrite(p, t0.Add(time.Second+40*time.Millisecond))
	assert.Equal(t, uint16(65535), p.SequenceNumber)
	assert.Equal(t, uint16(0), s.lastSeq, "late packet does not move the high-water mark")
}
