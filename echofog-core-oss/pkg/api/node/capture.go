package node

import (
	"errors"

	"github.com/echofog/echofog-core/internal/services/call/media"
)

// CaptureEncoder is a capture the app encodes itself instead of the webview (the desktop's own
// screen capture on Windows): the call's rate control and keyframe requests for its role go to it.
type CaptureEncoder interface {
	SetBitrate(bps uint64)
	RequestKeyframe()
}

// SetCaptureEncoder routes the rate and keyframe requests of a capture role ("screen") to enc;
// nil gives the role back to the webview.
func (n *NodeApi) SetCaptureEncoder(role string, enc CaptureEncoder) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	n.node.CallManager.SetLocalEncoder(role, enc)
	return nil
}

// PublishCaptureRTP sends one RTP packet of such a capture into the call, as the webview's
// capture goes through the media bridge: the stream started for its role carries it.
func (n *NodeApi) PublishCaptureRTP(role string, packet []byte) {
	if n.node == nil || n.node.CallManager == nil {
		return
	}
	n.node.CallManager.PublishLocalRTP(role, packet)
}

// CaptureRTPBudget is the largest RTP packet of a stream that travels to the first relay as one
// datagram.
func (n *NodeApi) CaptureRTPBudget(streamID string) int { return media.VideoUnitBudget(len(streamID)) }
