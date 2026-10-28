package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// A group is a space: besides its main chat it has text channels and voice rooms (see
// convlog/channels.go). A text channel is a ChatRef with Channel set; a voice room is joined as the
// group's call in that room, which never rings the members (they see it going on instead).

// ChannelDTO is one channel of a group.
type ChannelDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Voice  bool   `json:"voice"`
	Unread int    `json:"unread"`
	LastTS int64  `json:"lastTs"` // ms
	// Call: a voice room's call going on now
	Call *GroupCall `json:"call,omitempty"`
}

var errNotGroup = errors.New("only a group has channels")

// ChatChannels lists a group's channels and voice rooms, oldest first.
func (n *NodeApi) ChatChannels(ref ChatRef) ([]ChannelDTO, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return nil, err
	}
	if c.Kind != convlog.KindGroup {
		return nil, errNotGroup
	}
	meta, _ := f.log.Meta(c)
	var calls map[string]GroupCall
	if gf, err := n.groupsFacade(); err == nil {
		calls = gf.ActiveRoomCalls(c.Group)
	}
	out := []ChannelDTO{}
	for _, ch := range f.log.Channels(c) {
		d := ChannelDTO{ID: ch.ID, Name: ch.Name, Voice: ch.Voice}
		if st := meta.Chans[ch.ID]; st != nil {
			d.Unread, d.LastTS = st.Unread, st.LastTS
		}
		if call, ok := calls[ch.ID]; ok && ch.Voice {
			d.Call = &call
		}
		out = append(out, d)
	}
	return out, nil
}

// ChatChannelSave creates a channel (id "") or renames one; voice makes it a voice room.
func (n *NodeApi) ChatChannelSave(ref ChatRef, id, name string, voice bool) (ChannelDTO, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return ChannelDTO{}, err
	}
	if c.Kind != convlog.KindGroup {
		return ChannelDTO{}, errNotGroup
	}
	if id == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	raw, eid, err := f.log.AppendChannel(c, id, name, voice, false)
	if err != nil {
		return ChannelDTO{}, err
	}
	_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	return ChannelDTO{ID: id, Name: name, Voice: voice}, nil
}

// ChatChannelRemove removes a channel (its messages stay in the log, out of sight).
func (n *NodeApi) ChatChannelRemove(ref ChatRef, id string) error {
	f, c, err := n.chat(ref)
	if err != nil {
		return err
	}
	if c.Kind != convlog.KindGroup {
		return errNotGroup
	}
	raw, eid, err := f.log.AppendChannel(c, id, "", false, true)
	if err != nil {
		return err
	}
	_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	return nil
}

// StartRoomCall joins a group's voice room: the call going on in it, or a new one (which does not
// ring anybody).
func (n *NodeApi) StartRoomCall(groupID, room string) (GroupCall, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupCall{}, err
	}
	if room == "" {
		return GroupCall{}, errors.New("no room")
	}
	if c, ok := f.ActiveCall(groupID, room); ok {
		return c, nil
	}
	return f.StartCall(groupID, room)
}

// KeepRoomCallAlive tells the members that the call in a voice room is still going.
func (n *NodeApi) KeepRoomCallAlive(groupID, room, callID, coordinator string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.KeepCallAlive(groupID, room, callID, coordinator)
}

// EndRoomCall ends the call in a voice room (the last one out does).
func (n *NodeApi) EndRoomCall(groupID, room, callID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.EndCall(groupID, room, callID)
}
