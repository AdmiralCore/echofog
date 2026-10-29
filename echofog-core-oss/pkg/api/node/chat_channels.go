package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// A group is a space: besides its main chat it has text channels and voice rooms (see
// convlog/channels.go). A text channel is a ChatRef with Channel set; a voice room is joined as the
// group's call in that room, which never rings the members (they see it going on instead).

// ChannelDTO is one channel of a group, a category, or (ChatMainChannel) the main channel.
type ChannelDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Voice    bool   `json:"voice"`
	Topic    string `json:"topic,omitempty"`
	ReadOnly bool   `json:"readOnly"` // only managers post (an announcements channel)
	Position int32  `json:"position"`
	Parent   string `json:"parent,omitempty"`   // the category it is in
	Category bool   `json:"category,omitempty"` // a category: a heading for the channels in it
	SlowSecs uint32 `json:"slowSecs,omitempty"` // slow mode: seconds between a member's messages
	Open     string `json:"open,omitempty"`     // the main channel: the channel the group opens on
	Unread   int    `json:"unread"`
	LastTS   int64  `json:"lastTs"` // ms
	// CanSend: I may post in it
	CanSend bool `json:"canSend"`
	// Call: a voice room's call going on now, with who is in it
	Call *GroupCall `json:"call,omitempty"`
}

// ChannelSpec is a channel or a category as the app wants it (ChatChannelSave), or the main
// channel's settings (ChatMainChannelSave: ID, Voice, Category and Parent are not used).
type ChannelSpec struct {
	ID       string `json:"id"` // "" creates a new channel
	Name     string `json:"name"`
	Voice    bool   `json:"voice"`
	Topic    string `json:"topic"`
	ReadOnly bool   `json:"readOnly"`
	Position int32  `json:"position"`
	Parent   string `json:"parent"`
	Category bool   `json:"category"`
	SlowSecs uint32 `json:"slowSecs"`
	Open     string `json:"open"`
}

func channelDTO(ch convlog.Channel) ChannelDTO {
	return ChannelDTO{ID: ch.ID, Name: ch.Name, Voice: ch.Voice, Topic: ch.Topic, ReadOnly: ch.ReadOnly, Position: ch.Position,
		Parent: ch.Parent, Category: ch.Category, SlowSecs: ch.SlowSecs, Open: ch.Open}
}

func specOf(ch convlog.Channel) convlog.ChannelSpec {
	return convlog.ChannelSpec{ID: ch.ID, Name: ch.Name, Topic: ch.Topic, Voice: ch.Voice, ReadOnly: ch.ReadOnly,
		Position: ch.Position, Parent: ch.Parent, Category: ch.Category, SlowSecs: ch.SlowSecs, Open: ch.Open}
}

// kindOf: channels are ordered among their kind (text channels, voice rooms, categories).
func kindOf(ch convlog.Channel) int {
	switch {
	case ch.Category:
		return 2
	case ch.Voice:
		return 1
	}
	return 0
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
		d := channelDTO(ch)
		d.CanSend = !ch.Category && r.Send && (!ch.ReadOnly || r.Manage)
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
	if spec.Category {
		spec.Voice, spec.Parent, spec.ReadOnly, spec.SlowSecs = false, "", false, 0
	}
	if spec.ID == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		spec.ID = hex.EncodeToString(b)
		n := 0
		for _, ch := range f.log.Channels(c) {
			if kindOf(ch) == kindOf(convlog.Channel{Voice: spec.Voice, Category: spec.Category}) {
				n++
			}
		}
		spec.Position = int32(n) // a new one goes to the end of its kind
	} else if cur, ok := f.log.ChannelOf(c, spec.ID); ok {
		// a text channel stays text, a room a room, a category a category
		spec.Voice, spec.Category = cur.Voice, cur.Category
	}
	if spec.Parent != "" {
		if p, ok := f.log.ChannelOf(c, spec.Parent); !ok || !p.Category {
			return ChannelDTO{}, errors.New("no such category")
		}
	}
	s := convlog.ChannelSpec{ID: spec.ID, Name: spec.Name, Topic: spec.Topic, Voice: spec.Voice, ReadOnly: spec.ReadOnly,
		Position: spec.Position, Parent: spec.Parent, Category: spec.Category, SlowSecs: spec.SlowSecs}
	raw, eid, err := f.log.AppendChannel(c, s)
	if err != nil {
		return ChannelDTO{}, err
	}
	_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	return channelDTO(convlog.Channel{ID: s.ID, Name: s.Name, Voice: s.Voice, Topic: s.Topic, ReadOnly: s.ReadOnly,
		Position: s.Position, Parent: s.Parent, Category: s.Category, SlowSecs: s.SlowSecs}), nil
}

// ChatMainChannel is the settings of a group's main channel: its name ("" the app's own), topic,
// read-only, slow mode, and the channel the group opens on ("" the main one).
func (n *NodeApi) ChatMainChannel(ref ChatRef) (ChannelDTO, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return ChannelDTO{}, err
	}
	if c.Kind != convlog.KindGroup {
		return ChannelDTO{}, errNotGroup
	}
	r := f.myRights(c)
	d := channelDTO(f.log.MainChannel(c))
	if d.Open != "" {
		if ch, ok := f.log.ChannelOf(c, d.Open); !ok || ch.Category || ch.Voice {
			d.Open = "" // it went: the group opens on the main channel
		}
	}
	d.CanSend = r.Send && (!d.ReadOnly || r.Manage)
	return d, nil
}

// ChatMainChannelSave changes the settings of a group's main channel.
func (n *NodeApi) ChatMainChannelSave(ref ChatRef, spec ChannelSpec) error {
	f, c, err := n.chat(ref)
	if err != nil {
		return err
	}
	if c.Kind != convlog.KindGroup {
		return errNotGroup
	}
	if spec.Open != "" {
		if ch, ok := f.log.ChannelOf(c, spec.Open); !ok || ch.Category || ch.Voice {
			return errors.New("a group opens on one of its text channels")
		}
	}
	raw, eid, err := f.log.AppendChannel(c, convlog.ChannelSpec{Name: strings.TrimSpace(spec.Name), Topic: spec.Topic,
		ReadOnly: spec.ReadOnly, SlowSecs: spec.SlowSecs, Open: spec.Open})
	if err != nil {
		return err
	}
	_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
	return nil
}

// slowWait refuses a message to a channel in slow mode until the time since our last one there has
// passed; who may moderate or manage channels is not held back. The sender's app keeps to it: the
// log has no clock everybody trusts to check it against.
func (f *convsFacade) slowWait(c convlog.Conv, channel string) error {
	if c.Kind != convlog.KindGroup {
		return nil
	}
	var slow uint32
	if channel == "" {
		slow = f.log.MainChannel(c).SlowSecs
	} else if ch, ok := f.log.ChannelOf(c, channel); ok {
		slow = ch.SlowSecs
	}
	if slow == 0 {
		return nil
	}
	if r := f.myRights(c); r.Moderate || r.Manage {
		return nil
	}
	msgs, _, err := f.log.HistoryIn(c, channel, "", 50)
	if err != nil {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Mine {
			if left := time.Duration(slow)*time.Second - time.Since(time.UnixMilli(msgs[i].TS)); left > 0 {
				return fmt.Errorf("slow mode: you can write again in %d s", int(left.Seconds())+1)
			}
			break
		}
	}
	return nil
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
		if kindOf(ch) == kindOf(*moved) && ch.ID != id {
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
		s := specOf(ch)
		s.Position = int32(i)
		raw, eid, err := f.log.AppendChannel(c, s)
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
	if cur.Category {
		// its channels stay, out of any category
		for _, ch := range f.log.Channels(c) {
			if ch.Parent == id {
				s := specOf(ch)
				s.Parent = ""
				if raw, eid, err := f.log.AppendChannel(c, s); err == nil {
					_, _ = f.sendEvent(c, raw, eid, convlog.KindChannel, "", "")
				}
			}
		}
	}
	raw, eid, err := f.log.AppendChannel(c, convlog.ChannelSpec{ID: id, Name: cur.Name, Voice: cur.Voice, Category: cur.Category, Remove: true})
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
