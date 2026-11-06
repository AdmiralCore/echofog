package node

import (
	"errors"
	"sort"
	"sync"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v2Calls is the part of *app.Calls the protocol 2 calls backend uses (T9.4).
type v2Calls interface {
	Manager() *call.CallManager
	Self() types.PeerID
	Join(callID, coordinator string, uploadKbps int, backdrop string) (string, error)
	Leave() error
	NetDrops() (out, in uint64)
}

var _ v2Calls = (*app.Calls)(nil)

// v2CallBackend is the protocol 2 calls and streams backend. The call manager, the trees, the layers
// and the WebRTC bridge are the v1 ones (the format of the media inside the sealed sessions is
// v1's for 0.3.0); only the way they reach the other members differs, and it is the app layer's.
// Group and room calls, the call path and the relay choice stay pending: they need the v2 chat and
// relay layers.
type v2CallBackend struct {
	v2PendingBackend
	calls v2Calls

	mu       sync.Mutex
	upload   int    // declared upstream bandwidth used to size call streams
	backdrop string // the animated background the others see on our call tile
}

var _ callBackend = (*v2CallBackend)(nil)

// AttachV2Calls gives the facade the v2 call layer: calls, streams and the capture of the screen
// answer from it from now on. At protocol 1 it does nothing.
func (n *NodeApi) AttachV2Calls(c v2Calls) {
	if c == nil || n.node == nil || n.node.Cfg == nil || !n.node.Cfg.IsV2() {
		return
	}
	n.backends.call = &v2CallBackend{calls: c}
}

var errV2NoCall = errors.New("not in a call")

func (b *v2CallBackend) cm() *call.CallManager { return b.calls.Manager() }

func (b *v2CallBackend) uploadKbps() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.upload > 0 {
		return b.upload
	}
	return defaultUploadKbps
}

func (b *v2CallBackend) GetCallManager() *call.CallManager { return b.cm() }

// JoinCall enters the room of an invite and returns the SDP offer of the loopback bridge.
func (b *v2CallBackend) JoinCall(callID string, coordPeerID string) (string, error) {
	b.mu.Lock()
	backdrop := b.backdrop
	b.mu.Unlock()
	return b.calls.Join(callID, coordPeerID, b.uploadKbps(), backdrop)
}

// SubmitCallAnswer delivers the frontend WebRTC SDP answer to the Go loopback bridge.
func (b *v2CallBackend) SubmitCallAnswer(sdpAnswer string) error {
	return b.cm().SubmitBridgeAnswer(sdpAnswer)
}

func (b *v2CallBackend) LeaveCall() error { return b.calls.Leave() }

func (b *v2CallBackend) StartScreenShare(streamID string, bitrate int) error {
	return b.cm().StartScreenShare(streamID, "screen", bitrate)
}

func (b *v2CallBackend) StopScreenShare(streamID string) error {
	return b.cm().StopScreenShare(streamID)
}

func (b *v2CallBackend) SetDisplayRate(hz int) { b.cm().SetDisplayRate(hz) }

func (b *v2CallBackend) WatchStream(streamID string) error { return b.cm().WatchStream(streamID) }

func (b *v2CallBackend) UnwatchStream(streamID string) error { return b.cm().UnwatchStream(streamID) }

func (b *v2CallBackend) SetVADState(speaking bool, audioDb float64) {
	b.cm().SetVADState(speaking, float32(audioDb))
}

// CallStreamStats returns the counters of one stream of the active call.
func (b *v2CallBackend) CallStreamStats(streamID string) (StreamStatsDTO, error) {
	d, ok := b.cm().StreamDiag(streamID)
	if !ok {
		return StreamStatsDTO{}, errV2NoCall
	}
	out, in := b.calls.NetDrops()
	return StreamStatsDTO{Own: d.Own, UnitsSent: d.UnitsSent, Delivered: d.Delivered, Lost: d.Lost, Nacked: d.Nacked,
		Retransmitted: d.Retransmitted, Recovered: d.Recovered, SendKbps: d.SendKbps, VideoSent: d.VideoSent, VideoDropped: d.VideoDropped,
		VideoFailed: d.VideoFailed, KeyframesServed: d.KeyframesServed, NetDroppedOut: out, NetDroppedIn: in}, nil
}

func (b *v2CallBackend) SetupCallCallbacks(
	onActiveSpeaker func(speaker string, speaking bool, db float32),
	onStreamAnnounced func(streamID string, broadcaster string, mediaType string, bitrate int),
	onStreamClosed func(streamID string),
	onTopologyUpdated func(epoch uint64, coordinator string),
) {
	cm := b.cm()
	if onActiveSpeaker != nil {
		cm.OnActiveSpeaker = func(speaker types.PeerID, speaking bool, db float32) {
			onActiveSpeaker(speaker.String(), speaking, db)
		}
	}
	if onStreamAnnounced != nil {
		cm.OnStreamAnnounced = func(streamID string, broadcaster types.PeerID, mediaType string, bitrate int) {
			onStreamAnnounced(streamID, broadcaster.String(), mediaType, bitrate)
		}
	}
	if onStreamClosed != nil {
		cm.OnStreamClosed = func(streamID string) { onStreamClosed(streamID) }
	}
	if onTopologyUpdated != nil {
		cm.OnTopologyUpdated = func(manifest *tree.TreeManifest) {
			if manifest != nil {
				onTopologyUpdated(manifest.Epoch, manifest.Coordinator.String())
			}
		}
		cm.OnCoordinatorChanged = func(newCoordinator types.PeerID, epoch uint64) {
			onTopologyUpdated(epoch, newCoordinator.String())
		}
	}
}

func (b *v2CallBackend) SetupCallMediaCallbacks(onSlot func(dto.CallMediaSlotDTO), onKicked func(reason string)) {
	cm := b.cm()
	if onSlot != nil {
		cm.OnMediaSlot = func(ev bridge.SlotEvent) { onSlot(slotDTO(ev)) }
	}
	if onKicked != nil {
		cm.OnKicked = onKicked
	}
}

func (b *v2CallBackend) GetCallSlots() []dto.CallMediaSlotDTO {
	out := []dto.CallMediaSlotDTO{}
	for _, ev := range b.cm().MediaSlots() {
		out = append(out, slotDTO(ev))
	}
	return out
}

// GetCallRoster lists the members of the active call (empty when not in a call).
func (b *v2CallBackend) GetCallRoster() dto.CallRosterDTO {
	s, ok := b.cm().Snapshot()
	if !ok {
		return dto.CallRosterDTO{}
	}
	r := dto.CallRosterDTO{CallID: s.CallID, Self: b.calls.Self().String(), Coordinator: s.Coordinator.String(), Streams: s.Streams}
	for _, m := range s.Members {
		r.Members = append(r.Members, m.String())
	}
	sort.Strings(r.Members)
	for p, bd := range s.Backdrops {
		if r.Backdrops == nil {
			r.Backdrops = make(map[string]string, len(s.Backdrops))
		}
		r.Backdrops[p.String()] = bd
	}
	return r
}

// SetCallBackdrop picks the animated background the others see on this node's call tile ("" for
// the plain one). It is kept for the calls joined later too.
func (b *v2CallBackend) SetCallBackdrop(name string) error {
	if tree.CleanBackdrop(name) != name {
		return errors.New("a backdrop name is lowercase letters, digits and dashes")
	}
	b.mu.Lock()
	b.backdrop = name
	b.mu.Unlock()
	b.cm().SetBackdrop(name)
	return nil
}

func (b *v2CallBackend) ReportCallMember(peerHex, reason string) error {
	var id types.PeerID
	if err := id.UnmarshalText([]byte(peerHex)); err != nil {
		return err
	}
	return b.cm().ReportMember(id, reason)
}

func (b *v2CallBackend) RequestRelayChange(streamID string) error {
	return b.cm().RequestRelayChange(streamID)
}

// SetUploadCapacity declares the local upstream bandwidth (kbps) used to validate stream quality.
func (b *v2CallBackend) SetUploadCapacity(kbps int) error {
	if kbps <= 0 {
		return errors.New("upload capacity must be positive")
	}
	b.mu.Lock()
	b.upload = kbps
	b.mu.Unlock()
	b.cm().SetUploadCapacity(kbps)
	return nil
}

// ListStreamQualities returns every broadcast preset and whether the current upload capacity sustains it.
func (b *v2CallBackend) ListStreamQualities() []dto.StreamQualityDTO {
	upload := b.uploadKbps()
	out := make([]dto.StreamQualityDTO, 0, len(tree.QualityPresets))
	for _, q := range tree.QualityPresets {
		out = append(out, dto.StreamQualityDTO{
			ID: q.ID, Label: q.Label, Width: q.Width, Height: q.Height, FPS: q.FPS,
			BitrateKbps: q.BitrateKbps, Codec: q.Codec, Fits: q.FitsUpload(upload),
		})
	}
	return out
}

// allowQuality honours the user's choice of a preset above the declared upload (as v1 does): the
// declared capacity is raised to what the preset needs.
func (b *v2CallBackend) allowQuality(q tree.StreamQuality) {
	if !q.FitsUpload(b.uploadKbps()) {
		_ = b.SetUploadCapacity(q.BitrateKbps)
	}
}

// StartStream announces a broadcast at a named quality preset (e.g. "2160p60").
func (b *v2CallBackend) StartStream(streamID, mediaType, qualityID string) error {
	q, err := tree.QualityByID(qualityID)
	if err != nil {
		return err
	}
	if mediaType == "" {
		mediaType = "screen"
	}
	b.allowQuality(q)
	return b.cm().StartStream(streamID, mediaType, q)
}

func (b *v2CallBackend) UpdateStreamQuality(streamID, qualityID string) error {
	q, err := tree.QualityByID(qualityID)
	if err != nil {
		return err
	}
	b.allowQuality(q)
	return b.cm().UpdateStreamQuality(streamID, q)
}

func (b *v2CallBackend) SetupStreamInfoCallback(fn func(info dto.StreamInfoDTO)) {
	if fn == nil {
		return
	}
	b.cm().OnStreamInfo = func(a signaling.StreamAnnouncePayload) {
		fn(dto.StreamInfoDTO{
			StreamID: a.StreamID, Broadcaster: a.Broadcaster.String(), MediaType: a.MediaType,
			Bitrate: a.BitrateKbps, Width: a.Width, Height: a.Height, FPS: a.FPS, Codec: a.Codec, QualityID: a.QualityID,
		})
	}
}

// SetCaptureEncoder routes the rate and keyframe requests of a capture role ("screen") to enc;
// nil gives the role back to the webview.
func (b *v2CallBackend) SetCaptureEncoder(role string, enc CaptureEncoder) error {
	b.cm().SetLocalEncoder(role, enc)
	return nil
}

// PublishCaptureRTP sends one RTP packet of the app's own capture into the call.
func (b *v2CallBackend) PublishCaptureRTP(role string, packet []byte) {
	b.cm().PublishLocalRTP(role, packet)
}

// CaptureRTPBudget is the largest RTP packet of a stream that travels to the first relay as one
// datagram.
func (b *v2CallBackend) CaptureRTPBudget(streamID string) int {
	return media.VideoUnitBudget(len(streamID))
}
