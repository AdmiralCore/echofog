package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v1CallBackend is the protocol 1 implementation of callBackend: the code that used to sit in the
// NodeApi methods of this domain, moved here unchanged by T11.1d. The NodeApi methods below are
// one-line delegates, so at protocol 1 the bodies run exactly as before, and at protocol 2 the
// same delegates reach v2PendingBackend (see backends.go).
type v1CallBackend struct{ *NodeApi }

// callDomain is the calls and streams backend of this facade, chosen the same way as netDomain.
func (n *NodeApi) callDomain() callBackend {
	if n.backends.call != nil {
		return n.backends.call
	}
	if n.node != nil && n.node.Cfg != nil && n.node.Cfg.ProtocolOrDefault() == config.ProtocolV2 {
		return v2PendingBackend{}
	}
	return &v1CallBackend{n}
}

func (n *NodeApi) ActiveGroupCall(groupID string) (GroupCall, bool) {
	return n.callDomain().ActiveGroupCall(groupID)
}

func (n *NodeApi) CallStreamStats(streamID string) (StreamStatsDTO, error) {
	return n.callDomain().CallStreamStats(streamID)
}

func (n *NodeApi) CaptureRTPBudget(streamID string) int {
	return n.callDomain().CaptureRTPBudget(streamID)
}

func (n *NodeApi) EndGroupCall(groupID, callID string) error {
	return n.callDomain().EndGroupCall(groupID, callID)
}

func (n *NodeApi) EndRoomCall(groupID, room, callID string) error {
	return n.callDomain().EndRoomCall(groupID, room, callID)
}

func (n *NodeApi) GetCallManager() *call.CallManager {
	return n.callDomain().GetCallManager()
}

func (n *NodeApi) GetCallPath() CallPath {
	return n.callDomain().GetCallPath()
}

func (n *NodeApi) GetCallRelays() []CallRelay {
	return n.callDomain().GetCallRelays()
}

func (n *NodeApi) GetCallRoster() dto.CallRosterDTO {
	return n.callDomain().GetCallRoster()
}

func (n *NodeApi) GetCallSlots() []dto.CallMediaSlotDTO {
	return n.callDomain().GetCallSlots()
}

func (n *NodeApi) InviteToCall(pubKeyHex string) (CallInvite, error) {
	return n.callDomain().InviteToCall(pubKeyHex)
}

func (n *NodeApi) JoinCall(callID string, coordPeerID string) (string, error) {
	return n.callDomain().JoinCall(callID, coordPeerID)
}

func (n *NodeApi) KeepGroupCallAlive(groupID, callID, coordinator string) error {
	return n.callDomain().KeepGroupCallAlive(groupID, callID, coordinator)
}

func (n *NodeApi) KeepRoomCallAlive(groupID, room, callID, coordinator string) error {
	return n.callDomain().KeepRoomCallAlive(groupID, room, callID, coordinator)
}

func (n *NodeApi) LeaveCall() error {
	return n.callDomain().LeaveCall()
}

func (n *NodeApi) LeaveRoomCall(groupID, room, callID string) error {
	return n.callDomain().LeaveRoomCall(groupID, room, callID)
}

func (n *NodeApi) ListStreamQualities() []dto.StreamQualityDTO {
	return n.callDomain().ListStreamQualities()
}

func (n *NodeApi) PublishCaptureRTP(role string, packet []byte) {
	n.callDomain().PublishCaptureRTP(role, packet)
}

func (n *NodeApi) ReportCallMember(peerHex, reason string) error {
	return n.callDomain().ReportCallMember(peerHex, reason)
}

func (n *NodeApi) RequestRelayChange(streamID string) error {
	return n.callDomain().RequestRelayChange(streamID)
}

func (n *NodeApi) SetCallBackdrop(name string) error {
	return n.callDomain().SetCallBackdrop(name)
}

func (n *NodeApi) SetCaptureEncoder(role string, enc CaptureEncoder) error {
	return n.callDomain().SetCaptureEncoder(role, enc)
}

func (n *NodeApi) SetDisplayRate(hz int) {
	n.callDomain().SetDisplayRate(hz)
}

func (n *NodeApi) SetUploadCapacity(kbps int) error {
	return n.callDomain().SetUploadCapacity(kbps)
}

func (n *NodeApi) SetVADState(speaking bool, audioDb float64) {
	n.callDomain().SetVADState(speaking, audioDb)
}

func (n *NodeApi) SetupCallCallbacks(
	onActiveSpeaker func(speaker string, speaking bool, db float32),
	onStreamAnnounced func(streamID string, broadcaster string, mediaType string, bitrate int),
	onStreamClosed func(streamID string),
	onTopologyUpdated func(epoch uint64, coordinator string),
) {
	n.callDomain().SetupCallCallbacks(onActiveSpeaker, onStreamAnnounced, onStreamClosed, onTopologyUpdated)
}

func (n *NodeApi) SetupCallMediaCallbacks(onSlot func(dto.CallMediaSlotDTO), onKicked func(reason string)) {
	n.callDomain().SetupCallMediaCallbacks(onSlot, onKicked)
}

func (n *NodeApi) SetupStreamInfoCallback(fn func(info dto.StreamInfoDTO)) {
	n.callDomain().SetupStreamInfoCallback(fn)
}

func (n *NodeApi) StartGroupCall(groupID string) (GroupCall, error) {
	return n.callDomain().StartGroupCall(groupID)
}

func (n *NodeApi) StartRoomCall(groupID, room string) (GroupCall, error) {
	return n.callDomain().StartRoomCall(groupID, room)
}

func (n *NodeApi) StartScreenShare(streamID string, bitrate int) error {
	return n.callDomain().StartScreenShare(streamID, bitrate)
}

func (n *NodeApi) StartStream(streamID, mediaType, qualityID string) error {
	return n.callDomain().StartStream(streamID, mediaType, qualityID)
}

func (n *NodeApi) StopScreenShare(streamID string) error {
	return n.callDomain().StopScreenShare(streamID)
}

func (n *NodeApi) SubmitCallAnswer(sdpAnswer string) error {
	return n.callDomain().SubmitCallAnswer(sdpAnswer)
}

func (n *NodeApi) SwitchCallRelay(via string) error {
	return n.callDomain().SwitchCallRelay(via)
}

func (n *NodeApi) UnwatchStream(streamID string) error {
	return n.callDomain().UnwatchStream(streamID)
}

func (n *NodeApi) UpdateStreamQuality(streamID, qualityID string) error {
	return n.callDomain().UpdateStreamQuality(streamID, qualityID)
}

func (n *NodeApi) WatchStream(streamID string) error {
	return n.callDomain().WatchStream(streamID)
}
