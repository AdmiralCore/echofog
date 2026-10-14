package node

import (
	"errors"
	"sort"

	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

var errCallsUnavailable = errors.New("call manager not initialized")

func slotDTO(ev bridge.SlotEvent) dto.CallMediaSlotDTO {
	return dto.CallMediaSlotDTO{Slot: ev.Slot, Kind: ev.Kind, Peer: ev.Peer, StreamID: ev.StreamID, Active: ev.Active}
}

// SetupCallMediaCallbacks wires per-source playback slots and moderation events to the UI.
func (n *NodeApi) SetupCallMediaCallbacks(onSlot func(dto.CallMediaSlotDTO), onKicked func(reason string)) {
	if n.node == nil || n.node.CallManager == nil {
		return
	}
	cm := n.node.CallManager
	if onSlot != nil {
		cm.OnMediaSlot = func(ev bridge.SlotEvent) { onSlot(slotDTO(ev)) }
	}
	if onKicked != nil {
		cm.OnKicked = onKicked
	}
}

// GetCallSlots returns current slot assignments, so a reloaded UI can relabel its tracks.
func (n *NodeApi) GetCallSlots() []dto.CallMediaSlotDTO {
	if n.node == nil || n.node.CallManager == nil {
		return nil
	}
	out := []dto.CallMediaSlotDTO{}
	for _, ev := range n.node.CallManager.MediaSlots() {
		out = append(out, slotDTO(ev))
	}
	return out
}

// GetCallRoster lists the members of the active call (empty when not in a call).
func (n *NodeApi) GetCallRoster() dto.CallRosterDTO {
	if n.node == nil || n.node.CallManager == nil {
		return dto.CallRosterDTO{}
	}
	s, ok := n.node.CallManager.Snapshot()
	if !ok {
		return dto.CallRosterDTO{}
	}
	r := dto.CallRosterDTO{CallID: s.CallID, Self: n.node.ID.String(), Coordinator: s.Coordinator.String(), Streams: s.Streams}
	for _, m := range s.Members {
		r.Members = append(r.Members, m.String())
	}
	sort.Strings(r.Members)
	return r
}

// ReportCallMember flags a member for spam; enough reports remove and ban it from the room.
func (n *NodeApi) ReportCallMember(peerHex, reason string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errCallsUnavailable
	}
	var id types.PeerID
	if err := id.UnmarshalText([]byte(peerHex)); err != nil {
		return err
	}
	return n.node.CallManager.ReportMember(id, reason)
}

// RequestRelayChange asks to receive a watched stream through another relay.
func (n *NodeApi) RequestRelayChange(streamID string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errCallsUnavailable
	}
	return n.node.CallManager.RequestRelayChange(streamID)
}
