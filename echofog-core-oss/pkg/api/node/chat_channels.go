package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// A group is a space: besides its main chat it has text channels and voice rooms (see
// convlog/channels.go). A text channel is a ChatRef with Channel set; a voice room is joined as the
// group's call in that room, which never rings the members (they see it going on instead).

// ChannelDTO is one channel of a group.
type ChannelDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Voice    bool   `json:"voice"`
	Topic    string `json:"topic,omitempty"`
	ReadOnly bool   `json:"readOnly"` // only managers post (an announcements channel)
	Position int32  `json:"position"`
	Unread   int    `json:"unread"`
	LastTS   int64  `json:"lastTs"` // ms
	// CanSend: I may post in it
	CanSend bool `json:"canSend"`
	// Call: a voice room's call going on now, with who is in it
	Call *GroupCall `json:"call,omitempty"`
}

// ChannelSpec is a channel as the app wants it (ChatChannelSave).
type ChannelSpec struct {
	ID       string `json:"id"` // "" creates a new channel
	Name     string `json:"name"`
	Voice    bool   `json:"voice"`
	Topic    string `json:"topic"`
	ReadOnly bool   `json:"readOnly"`
	Position int32  `json:"position"`
}

var errNotGroup = errors.New("only a group has channels")

// ChatChannels lists a group's channels and voice rooms in their order.
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
	r := f.myRights(c)
	out := []ChannelDTO{}
	for _, ch := range f.log.Channels(c) {
		d := ChannelDTO{ID: ch.ID, Name: ch.Name, Voice: ch.Voice, Topic: ch.Topic, ReadOnly: ch.ReadOnly, Position: ch.Position,
			CanSend: r.Send && (!ch.ReadOnly || r.Manage)}
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

// ChatChannelSave creates a channel (spec.ID "") or changes one: name, topic, read-only, order.
func (n *NodeApi) ChatChannelSave(ref ChatRef, spec ChannelSpec) (ChannelDTO, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return ChannelDTO{}, err
	}
	if c.Kind != convlog.KindGroup {
		return ChannelDTO{}, errNotGroup
	}
	if spec.ID == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		spec.ID = hex.EncodeToString(b)
		spec.Position = int32(len(f.log.Channels(c))) // a new channel goes to the end
	} else if cur, ok := f.log.ChannelOf(c, spec.ID); ok {
		spec.Voice = cur.Voice // a text channel stays text, a room a room
	}
	raw, eid, err := f.log.AppendChannel(c, convlog.ChannelSpec{ID: spec.ID, Name: spec.Name, Topic: spec.Topic,
		Voice: spec.Voice, ReadOnly: spec.ReadOnly, Position: spec.Position})
	if err != nil {
		return ChannelDTO{}, err
	}
	_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	return ChannelDTO{ID: spec.ID, Name: spec.Name, Voice: spec.Voice, Topic: spec.Topic, ReadOnly: spec.ReadOnly, Position: spec.Position}, nil
}

// ChatChannelMove puts a channel at index among the channels of its kind (text or voice).
func (n *NodeApi) ChatChannelMove(ref ChatRef, id string, index int) error {
	f, c, err := n.chat(ref)
	if err != nil {
		return err
	}
	var same []convlog.Channel
	var moved *convlog.Channel
	for _, ch := range f.log.Channels(c) {
		ch := ch
		if ch.ID == id {
			moved = &ch
		}
	}
	if moved == nil {
		return errors.New("no such channel")
	}
	for _, ch := range f.log.Channels(c) {
		if ch.Voice == moved.Voice && ch.ID != id {
			same = append(same, ch)
		}
	}
	if index < 0 {
		index = 0
	}
	if index > len(same) {
		index = len(same)
	}
	order := append(append(append([]convlog.Channel{}, same[:index]...), *moved), same[index:]...)
	for i, ch := range order {
		if int(ch.Position) == i {
			continue // only what changed is written
		}
		raw, eid, err := f.log.AppendChannel(c, convlog.ChannelSpec{ID: ch.ID, Name: ch.Name, Topic: ch.Topic,
			Voice: ch.Voice, ReadOnly: ch.ReadOnly, Position: int32(i)})
		if err != nil {
			return err
		}
		_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	}
	return nil
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
	cur, _ := f.log.ChannelOf(c, id)
	raw, eid, err := f.log.AppendChannel(c, convlog.ChannelSpec{ID: id, Name: cur.Name, Voice: cur.Voice, Remove: true})
	if err != nil {
		return err
	}
	_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	return nil
}

// ChatThreads are the messages of a chat's channel (ref.Channel) that have comments, the most
// recently active first.
func (n *NodeApi) ChatThreads(ref ChatRef) ([]ChatMessage, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return nil, err
	}
	return f.dress(c, f.log.Threads(c, ref.Channel, 2000)), nil
}

// ChatThreadTitle names the thread under a message ("" clears it): its author or a moderator.
func (n *NodeApi) ChatThreadTitle(ref ChatRef, root, title string) error {
	return n.chatOp(ref, root, convlog.KindThread, func(l *convlog.Log, c convlog.Conv, id convlog.EventID) ([]byte, convlog.EventID, error) {
		return l.AppendThreadTitle(c, id, strings.TrimSpace(title))
	})
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

// LeaveRoomCall says we left a voice room whose call others stay in.
func (n *NodeApi) LeaveRoomCall(groupID, room, callID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	f.LeaveCall(groupID, room, callID)
	return nil
}

// EndRoomCall ends the call in a voice room (the last one out does).
func (n *NodeApi) EndRoomCall(groupID, room, callID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.EndCall(groupID, room, callID)
}
