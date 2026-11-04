//go:build android || ios

package bridge

import (
	"errors"

	"github.com/echofog/echofog-core/internal/services/call/media"
)

// The WebRTC bridge is a desktop-only piece: it connects the Wails webview to the call
// media. On mobile the UI is native and the media path is different, while pion would
// drag cgo-only address helpers (wlynxg/anet) into the gomobile build. This file keeps
// the package compiling with the same API and no media.

// Local capture roles fed by the UI.
const (
	RoleMic         = "mic"
	RoleCamera      = "camera"
	RoleScreen      = "screen"
	RoleScreenAudio = "screenaudio"
)

const (
	// AudioSlots is how many remote speakers can be heard at once.
	AudioSlots = 16
	// VideoSlots is how many remote streams can be watched at once.
	VideoSlots = 4
	// StreamAudioSlots carry the sound of watched streams.
	StreamAudioSlots = VideoSlots
	// PreviewSlots carry our own screen as viewers get it.
	PreviewSlots = 1
)

// ErrNoSlot reports that every media slot is busy.
var ErrNoSlot = errors.New("all media slots are busy")

// ErrUnsupported reports that the desktop media bridge is not available here.
var ErrUnsupported = errors.New("the media bridge is not available on mobile")

// SlotEvent tells the UI which remote source a playback track carries.
type SlotEvent struct {
	Slot     string `json:"slot"`
	Kind     string `json:"kind"`
	Peer     string `json:"peer"`
	StreamID string `json:"streamId"`
	Active   bool   `json:"active"`
}

// LocalRTPHandler receives RTP packets captured by the UI, tagged with their role.
type LocalRTPHandler func(role string, payload []byte)

// RTPPacketHandlerCompat is the callback signature used by the call manager.
type RTPPacketHandlerCompat = LocalRTPHandler

// Options wires the bridge into the call manager.
type Options struct {
	OnLocalRTP              LocalRTPHandler
	OnSlot                  func(SlotEvent)
	OnKeyframeNeeded        func(streamID string)
	OnPreviewKeyframeNeeded func(streamID string)
}

// LocalMediaBridge is the stub of the desktop WebRTC loopback.
type LocalMediaBridge struct{}

// NewLocalMediaBridge keeps the call code working without the desktop bridge: the call
// itself still runs and relays, only local capture and playback are absent.
func NewLocalMediaBridge(opts Options) (*LocalMediaBridge, error) { return &LocalMediaBridge{}, nil }

func (b *LocalMediaBridge) PushRemoteAudio(pkt *media.MediaPacket) error          { return nil }
func (b *LocalMediaBridge) PushRemoteStreamAudio(pkt *media.MediaPacket) error    { return nil }
func (b *LocalMediaBridge) PushLocalPreview(streamID string, packet []byte) error { return nil }
func (b *LocalMediaBridge) ReleasePreview(streamID string)                        {}
func (b *LocalMediaBridge) PushRemoteVideo(pkt *media.MediaPacket) error          { return nil }
func (b *LocalMediaBridge) ReleaseVideo(streamID string)                          {}
func (b *LocalMediaBridge) ReleaseAudio(peer string)                              {}
func (b *LocalMediaBridge) Slots() []SlotEvent                                    { return nil }
func (b *LocalMediaBridge) Close() error                                          { return nil }

func (b *LocalMediaBridge) RequestKeyframe(role string) error            { return ErrUnsupported }
func (b *LocalMediaBridge) SetSendBitrate(role string, bps uint64) error { return ErrUnsupported }
func (b *LocalMediaBridge) CreateOffer() (string, error)                 { return "", ErrUnsupported }
func (b *LocalMediaBridge) SetRemoteAnswer(sdpAnswer string) error       { return ErrUnsupported }
