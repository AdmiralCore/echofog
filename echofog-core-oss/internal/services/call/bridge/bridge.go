package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

// Local capture roles fed by the UI. They map to the first four m-lines of the bridge offer,
// in this order, so the UI attaches tracks with replaceTrack and never renegotiates.
const (
	RoleMic    = "mic"
	RoleCamera = "camera"
	RoleScreen = "screen"
	// RoleScreenAudio is the sound of the shared screen; it goes out with the screen stream.
	RoleScreenAudio = "screenaudio"
)

var localRoles = []string{RoleMic, RoleCamera, RoleScreen, RoleScreenAudio}

func isAudioRole(role string) bool { return role == RoleMic || role == RoleScreenAudio }

const (
	// AudioSlots is how many remote speakers can be heard at once; each gets its own track so
	// the UI mixes, meters and sets volume per person.
	AudioSlots = 16
	// VideoSlots is how many remote streams can be watched at once.
	VideoSlots = 4
	// StreamAudioSlots carry the sound of watched streams, one per stream, apart from the voices
	// so the UI gives a stream its own volume and does not take it for somebody speaking.
	StreamAudioSlots = VideoSlots
	// slotReuseIdle: a slot whose source has been silent this long may be handed to a new source.
	slotReuseIdle = 5 * time.Second

	audioClock = 48000
	videoClock = 90000
)

var ErrNoSlot = errors.New("all media slots are busy")

// SlotEvent tells the UI which remote source a playback track carries.
type SlotEvent struct {
	Slot     string `json:"slot"`     // "a0".."a15" / "v0".."v3" / "s0".."s3", also the MediaStream id suffix
	Kind     string `json:"kind"`     // "audio" | "video" | "streamaudio"
	Peer     string `json:"peer"`     // broadcaster peer ID (hex)
	StreamID string `json:"streamId"` // stream ID for video and stream audio, "voice" for voices
	Active   bool   `json:"active"`
}

// LocalRTPHandler receives RTP packets captured by the UI, tagged with their role.
type LocalRTPHandler func(role string, payload []byte)

type slot struct {
	id    string
	kind  string
	clock uint32
	track *webrtc.TrackLocalStaticRTP

	owner    string // audio: peer hex; video: stream ID
	peer     string
	lastUsed time.Time

	// RTP rewriting keeps one continuous sequence/timestamp space per playback track, so a
	// track handed to a new source looks like a pause, not a corrupt stream.
	started, rebase bool
	srcSSRC         uint32
	seqOffset       uint16
	tsOffset        uint32
	lastSeq         uint16
	lastTS          uint32
	lastWrite       time.Time
}

// LocalMediaBridge is the WebRTC loopback between the Wails UI (browser engine) and the Go core.
// It uses Pure Go Pion WebRTC without any CGO dependencies.
type LocalMediaBridge struct {
	mu sync.Mutex

	peerConnection *webrtc.PeerConnection
	localSSRC      map[string]uint32 // role -> SSRC of the UI capture track
	audio, video   []*slot
	streamAudio    []*slot

	onLocalRTP RTPPacketHandlerCompat
	onSlot     func(SlotEvent)
	onKeyframe func(streamID string)
	ctx        context.Context
	cancel     context.CancelFunc
	now        func() time.Time

	// The bridge runs per RTP packet; write and parse failures go through lim.
	log *slog.Logger
	lim *calllog.Limiter
}

// RTPPacketHandlerCompat is the callback signature used by the call manager.
type RTPPacketHandlerCompat = LocalRTPHandler

// Options wires the bridge into the call manager.
type Options struct {
	OnLocalRTP LocalRTPHandler
	// OnSlot reports slot assignments; the UI uses it to label tracks with people.
	OnSlot func(SlotEvent)
	// OnKeyframeNeeded fires when the UI decoder of a watched stream sends PLI/FIR.
	OnKeyframeNeeded func(streamID string)
}

// NewLocalMediaBridge creates and configures the local WebRTC bridge instance.
func NewLocalMediaBridge(opts Options) (*LocalMediaBridge, error) {
	ctx, cancel := context.WithCancel(context.Background())

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		cancel()
		return nil, fmt.Errorf("register default codecs: %w", err)
	}
	// MID lets Pion route UI tracks attached later with replaceTrack, before their SSRC is known.
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
		if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI}, kind); err != nil {
			cancel()
			return nil, err
		}
	}

	// Without receiver reports and transport-cc feedback the browser's bandwidth estimate never
	// leaves its low start value and the encoder smears every moving frame. The real network cap
	// comes separately, as REMB from SetSendBitrate.
	registry := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		cancel()
		return nil, err
	}
	if err := webrtc.ConfigureTWCCSender(mediaEngine, registry); err != nil {
		cancel()
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(registry))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{}})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	b := &LocalMediaBridge{
		peerConnection: pc,
		localSSRC:      make(map[string]uint32),
		onLocalRTP:     opts.OnLocalRTP,
		onSlot:         opts.OnSlot,
		onKeyframe:     opts.OnKeyframeNeeded,
		ctx:            ctx,
		cancel:         cancel,
		now:            time.Now,
		log:            slog.Default().With("component", "call-bridge"),
		lim:            calllog.New(calllog.Every),
	}
	fail := func(err error) (*LocalMediaBridge, error) {
		cancel()
		_ = pc.Close()
		return nil, err
	}

	// m-lines 0..3: what the UI captures (mic, camera, screen, screen audio).
	h264 := h264Codecs(mediaEngine)
	for _, role := range localRoles {
		kind := webrtc.RTPCodecTypeVideo
		if isAudioRole(role) {
			kind = webrtc.RTPCodecTypeAudio
		}
		t, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if err != nil {
			return fail(err)
		}
		if kind == webrtc.RTPCodecTypeVideo && len(h264) > 0 {
			// relays never transcode, so every broadcaster must send what every viewer decodes
			if err := t.SetCodecPreferences(h264); err != nil {
				return fail(err)
			}
		}
	}

	// Then the playback pool: one track per remote speaker / watched stream.
	for i := 0; i < AudioSlots; i++ {
		s, err := b.addSlot(fmt.Sprintf("a%d", i), "audio", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: audioClock, Channels: 2}, audioClock)
		if err != nil {
			return fail(err)
		}
		b.audio = append(b.audio, s)
	}
	for i := 0; i < VideoSlots; i++ {
		s, err := b.addSlot(fmt.Sprintf("v%d", i), "video", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: videoClock}, videoClock)
		if err != nil {
			return fail(err)
		}
		b.video = append(b.video, s)
	}
	// Last, so the voice and video slots keep their places: the sound of watched streams.
	for i := 0; i < StreamAudioSlots; i++ {
		s, err := b.addSlot(fmt.Sprintf("s%d", i), "streamaudio", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: audioClock, Channels: 2}, audioClock)
		if err != nil {
			return fail(err)
		}
		b.streamAudio = append(b.streamAudio, s)
	}

	// The loopback to the UI never crosses a network, so a failed or disconnected state means the
	// webview side went away or broke: from then on the user hears and sees nothing.
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			b.log.Warn("Media bridge to the UI lost", "state", st.String())
		case webrtc.PeerConnectionStateConnected:
			b.log.Info("Media bridge to the UI connected")
		default:
			b.log.Debug("Media bridge state changed", "state", st.String())
		}
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		if st == webrtc.ICEConnectionStateFailed {
			b.log.Warn("Media bridge ICE failed")
			return
		}
		b.log.Debug("Media bridge ICE state changed", "state", st.String())
	})
	pc.OnSignalingStateChange(func(st webrtc.SignalingState) {
		b.log.Debug("Media bridge signaling state changed", "state", st.String())
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		role := b.roleOf(receiver)
		if role == "" {
			b.log.Warn("Ignored UI track on an unexpected transceiver", "kind", track.Kind().String(), "codec", track.Codec().MimeType)
			return
		}
		b.log.Info("UI capture track started", "role", role, "codec", track.Codec().MimeType, "ssrc", uint32(track.SSRC()))
		b.mu.Lock()
		b.localSSRC[role] = uint32(track.SSRC())
		b.mu.Unlock()
		go b.readIncomingTrack(role, track)
	})

	return b, nil
}

// h264Codecs mirrors the packetization-mode=1 H264 entries of Pion's RegisterDefaultCodecs
// (same payload types), High profile first. Mode 0 cannot carry the large NAL units of 4K frames.
func h264Codecs(_ *webrtc.MediaEngine) []webrtc.RTPCodecParameters {
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: webrtc.TypeRTCPFBTransportCC}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	mk := func(pt webrtc.PayloadType, profile string) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: videoClock,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile,
				RTCPFeedback: fb,
			},
			PayloadType: pt,
		}
	}
	return []webrtc.RTPCodecParameters{mk(112, "64001f"), mk(106, "42e01f"), mk(102, "42001f"), mk(127, "4d001f")}
}

func (b *LocalMediaBridge) addSlot(id, kind string, capability webrtc.RTPCodecCapability, clock uint32) (*slot, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(capability, id, "efslot-"+id)
	if err != nil {
		return nil, err
	}
	t, err := b.peerConnection.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		return nil, err
	}
	s := &slot{id: id, kind: kind, clock: clock, track: track}
	go b.readSlotRTCP(s, t.Sender())
	return s, nil
}

// roleOf maps an incoming UI track to its capture role through the transceiver order.
func (b *LocalMediaBridge) roleOf(receiver *webrtc.RTPReceiver) string {
	for i, t := range b.peerConnection.GetTransceivers() {
		if i >= len(localRoles) {
			break
		}
		if t.Receiver() == receiver {
			return localRoles[i]
		}
	}
	return ""
}

// readIncomingTrack forwards RTP packets from local UI microphone/camera/screen into Go core.
func (b *LocalMediaBridge) readIncomingTrack(role string, track *webrtc.TrackRemote) {
	for {
		select {
		case <-b.ctx.Done():
			return
		default:
		}
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if b.ctx.Err() == nil {
				// the UI stopped this capture (mic muted, screen share ended) or the bridge broke
				if errors.Is(err, io.EOF) {
					b.log.Info("UI capture track ended", "role", role)
				} else {
					b.log.Warn("UI capture track read failed", "role", role, "err", err)
				}
			}
			return
		}
		if b.onLocalRTP == nil {
			continue
		}
		raw, err := pkt.Marshal()
		if err != nil {
			b.lim.Log(b.log, slog.LevelWarn, "marshal:"+role, "Dropped UI capture packet that could not be re-encoded", "role", role, "err", err)
			continue
		}
		b.onLocalRTP(role, raw)
	}
}

// readSlotRTCP drains sender feedback and turns the UI decoder's keyframe requests into
// network keyframe requests for the stream on that slot.
func (b *LocalMediaBridge) readSlotRTCP(s *slot, sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			if b.ctx.Err() == nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				b.log.Debug("Playback slot feedback ended", "slot", s.id, "err", err)
			}
			return
		}
		if s.kind != "video" || b.onKeyframe == nil {
			continue
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				b.mu.Lock()
				owner := s.owner
				b.mu.Unlock()
				if owner != "" {
					b.lim.Log(b.log, slog.LevelDebug, "pli:"+s.id, "UI decoder asked for a keyframe", "slot", s.id, "stream", owner)
					b.onKeyframe(owner)
				}
			}
		}
	}
}

// PushRemoteAudio plays a remote voice unit (a full RTP packet) on its speaker's own track.
func (b *LocalMediaBridge) PushRemoteAudio(pkt *media.MediaPacket) error {
	peer := pkt.Broadcaster.String()
	return b.push(b.audio, peer, peer, "voice", pkt.Payload)
}

// PushRemoteStreamAudio plays the sound of a watched stream (a full RTP packet) on its own track.
func (b *LocalMediaBridge) PushRemoteStreamAudio(pkt *media.MediaPacket) error {
	return b.push(b.streamAudio, pkt.StreamID, pkt.Broadcaster.String(), pkt.StreamID, pkt.Payload)
}

// PushRemoteVideo plays a remote stream unit (a full RTP packet) on that stream's own track.
func (b *LocalMediaBridge) PushRemoteVideo(pkt *media.MediaPacket) error {
	return b.push(b.video, pkt.StreamID, pkt.Broadcaster.String(), pkt.StreamID, pkt.Payload)
}

func (b *LocalMediaBridge) push(pool []*slot, owner, peer, streamID string, unit []byte) error {
	var p rtp.Packet
	if err := p.Unmarshal(unit); err != nil {
		err = fmt.Errorf("remote unit is not an RTP packet: %w", err)
		b.lim.Log(b.log, slog.LevelWarn, "unmarshal:"+owner, "Dropped remote media unit that is not RTP",
			"stream", streamID, "peer", shortHex(peer), "err", err)
		return err
	}
	now := b.now()

	b.mu.Lock()
	s, ev, prev := b.slotForLocked(pool, owner, peer, streamID, now)
	if s == nil {
		b.mu.Unlock()
		// every slot of the pool carries a source that is still playing: this one is not heard or seen
		b.lim.Log(b.log, slog.LevelWarn, "noslot:"+owner, "No free playback slot, remote media not played",
			"stream", streamID, "peer", shortHex(peer), "slots", len(pool))
		return ErrNoSlot
	}
	s.rewrite(&p, now)
	track := s.track
	b.mu.Unlock()

	if ev != nil {
		if prev != "" {
			b.log.Info("Playback slot taken over from an idle source", "slot", ev.Slot, "kind", ev.Kind, "peer", shortHex(peer), "stream", streamID, "previous", shortHex(prev))
		} else {
			b.log.Info("Playback slot assigned", "slot", ev.Slot, "kind", ev.Kind, "peer", shortHex(peer), "stream", streamID)
		}
		if b.onSlot != nil {
			b.onSlot(*ev)
		}
	}
	if err := track.WriteRTP(&p); err != nil {
		b.lim.Log(b.log, slog.LevelWarn, "write:"+s.id, "Failed to write remote media to the UI track",
			"slot", s.id, "stream", streamID, "peer", shortHex(peer), "err", err)
		return err
	}
	return nil
}

// shortHex shortens a hex peer ID (or a stream ID) the way calllog.Peer does.
func shortHex(peer string) string {
	if len(peer) > 12 {
		return peer[:12]
	}
	return peer
}

// slotForLocked returns the slot of owner, assigning a free or long-idle one if needed.
// prev is the owner an idle slot was taken from, if any.
func (b *LocalMediaBridge) slotForLocked(pool []*slot, owner, peer, streamID string, now time.Time) (*slot, *SlotEvent, string) {
	var free, idle *slot
	for _, s := range pool {
		if s.owner == owner {
			s.lastUsed = now
			return s, nil, ""
		}
		if s.owner == "" && free == nil {
			free = s
		}
		if s.owner != "" && now.Sub(s.lastUsed) >= slotReuseIdle && (idle == nil || s.lastUsed.Before(idle.lastUsed)) {
			idle = s
		}
	}
	s := free
	if s == nil {
		s = idle
	}
	if s == nil {
		return nil, nil, ""
	}
	prev := s.owner
	s.owner, s.peer, s.lastUsed, s.rebase = owner, peer, now, true
	return s, &SlotEvent{Slot: s.id, Kind: s.kind, Peer: peer, StreamID: streamID, Active: true}, prev
}

// rewrite maps the source's RTP numbering onto the slot's continuous numbering and drops header
// extensions: their IDs were negotiated by the broadcaster's session, not ours.
func (s *slot) rewrite(p *rtp.Packet, now time.Time) {
	p.Header.Extension = false
	p.Header.Extensions = nil
	p.Header.ExtensionProfile = 0

	if s.rebase || !s.started || p.SSRC != s.srcSSRC {
		if s.started {
			s.seqOffset = s.lastSeq + 1 - p.SequenceNumber
			gap := uint32(now.Sub(s.lastWrite).Seconds()*float64(s.clock)) + 1
			s.tsOffset = s.lastTS + gap - p.Timestamp
		}
		s.started, s.rebase, s.srcSSRC = true, false, p.SSRC
	}
	p.SequenceNumber += s.seqOffset
	p.Timestamp += s.tsOffset
	if int16(p.SequenceNumber-s.lastSeq) > 0 || s.lastWrite.IsZero() {
		s.lastSeq = p.SequenceNumber
	}
	if int32(p.Timestamp-s.lastTS) > 0 || s.lastWrite.IsZero() {
		s.lastTS = p.Timestamp
	}
	s.lastWrite = now
}

// ReleaseVideo frees the slot of a stream that is no longer watched.
func (b *LocalMediaBridge) ReleaseVideo(streamID string) {
	b.release(b.video, streamID, streamID)
	b.release(b.streamAudio, streamID, streamID)
}

// ReleaseAudio frees the slot of a member that left.
func (b *LocalMediaBridge) ReleaseAudio(peer string) { b.release(b.audio, peer, "voice") }

func (b *LocalMediaBridge) release(pool []*slot, owner, streamID string) {
	b.mu.Lock()
	var ev *SlotEvent
	for _, s := range pool {
		if s.owner == owner {
			ev = &SlotEvent{Slot: s.id, Kind: s.kind, Peer: s.peer, StreamID: streamID}
			s.owner, s.peer = "", ""
		}
	}
	b.mu.Unlock()
	if ev != nil {
		b.log.Info("Playback slot released", "slot", ev.Slot, "kind", ev.Kind, "peer", shortHex(ev.Peer), "stream", streamID)
		if b.onSlot != nil {
			b.onSlot(*ev)
		}
	}
}

// Slots lists active assignments, so a reloaded UI can relabel its tracks.
func (b *LocalMediaBridge) Slots() []SlotEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []SlotEvent
	for _, pool := range [][]*slot{b.audio, b.video, b.streamAudio} {
		for _, s := range pool {
			if s.owner == "" {
				continue
			}
			sid := "voice"
			if s.kind != "audio" {
				sid = s.owner
			}
			out = append(out, SlotEvent{Slot: s.id, Kind: s.kind, Peer: s.peer, StreamID: sid, Active: true})
		}
	}
	return out
}

// RequestKeyframe asks the UI encoder of a capture role for an IDR frame via RTCP PLI, so new or
// recovering viewers do not wait for the next periodic keyframe.
func (b *LocalMediaBridge) RequestKeyframe(role string) error {
	b.mu.Lock()
	pc, ssrc := b.peerConnection, b.localSSRC[role]
	b.mu.Unlock()
	if pc == nil || ssrc == 0 {
		err := errors.New("no local video track to refresh")
		b.lim.Log(b.log, slog.LevelWarn, "pli-out:"+role, "Cannot ask the UI encoder for a keyframe", "role", role, "err", err)
		return err
	}
	if err := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}); err != nil {
		b.lim.Log(b.log, slog.LevelWarn, "pli-out:"+role, "Failed to ask the UI encoder for a keyframe", "role", role, "err", err)
		return err
	}
	return nil
}

// SetSendBitrate caps the UI encoder of a capture role with an RTCP REMB, so it lowers its rate
// when the network path backs up instead of the path dropping packets at random.
func (b *LocalMediaBridge) SetSendBitrate(role string, bps uint64) error {
	b.mu.Lock()
	pc, ssrc := b.peerConnection, b.localSSRC[role]
	b.mu.Unlock()
	if pc == nil || ssrc == 0 {
		// normal for a moment after a stream starts, before the UI attaches its track
		err := errors.New("no local video track to limit")
		b.lim.Log(b.log, slog.LevelDebug, "remb:"+role, "Cannot cap the UI encoder yet", "role", role, "err", err)
		return err
	}
	if err := pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: float32(bps), SSRCs: []uint32{ssrc}}}); err != nil {
		b.lim.Log(b.log, slog.LevelWarn, "remb:"+role, "Failed to cap the UI encoder bitrate", "role", role, "kbps", bps/1000, "err", err)
		return err
	}
	return nil
}

// CreateOffer generates a WebRTC SDP Offer for Wails frontend.
func (b *LocalMediaBridge) CreateOffer() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	offer, err := b.peerConnection.CreateOffer(nil)
	if err != nil {
		b.log.Warn("Failed to create media bridge offer", "err", err)
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(b.peerConnection)
	if err := b.peerConnection.SetLocalDescription(offer); err != nil {
		b.log.Warn("Failed to set media bridge offer", "err", err)
		return "", err
	}
	// The UI gets one SDP and no trickle channel, so it must already carry our candidates.
	select {
	case <-gathered:
	case <-time.After(2 * time.Second):
		b.log.Warn("Media bridge ICE gathering did not finish in time, the offer may lack candidates")
	}
	b.log.Info("Media bridge offer created")
	return b.peerConnection.LocalDescription().SDP, nil
}

// SetRemoteAnswer sets the WebRTC SDP Answer received from Wails frontend.
func (b *LocalMediaBridge) SetRemoteAnswer(sdpAnswer string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.peerConnection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdpAnswer}); err != nil {
		b.log.Warn("Rejected media bridge answer from the UI", "err", err)
		return err
	}
	b.log.Info("Media bridge answer applied")
	return nil
}

// Close gracefully closes the WebRTC loopback connection.
func (b *LocalMediaBridge) Close() error {
	b.cancel()
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.peerConnection != nil {
		if err := b.peerConnection.Close(); err != nil {
			b.log.Warn("Failed to close media bridge", "err", err)
			return err
		}
	}
	b.log.Debug("Media bridge closed")
	return nil
}
