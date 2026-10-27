package node

import (
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Group chats on the conversation log: who may do what in a group, which group an event names, a
// group's events and old-style messages, and the sync between members.

// groupPolicy: members take part; admins and moderators pin; who may delete others' messages
// (moderators) may delete anyone's.
type groupPolicy struct{ n *NodeApi }

func (p groupPolicy) role(c convlog.Conv, a types.PeerID) (groups.Role, bool) {
	gf, err := p.n.groupsFacade()
	if err != nil {
		return groups.Role{}, false
	}
	return gf.role(c.Group, a)
}

func (p groupPolicy) Participant(c convlog.Conv, a types.PeerID) bool {
	_, ok := p.role(c, a)
	return ok
}

func (p groupPolicy) CanPin(c convlog.Conv, a types.PeerID) bool {
	r, ok := p.role(c, a)
	return ok && r.Permissions&(groups.PermManageGroup|groups.PermDeleteMessages) != 0
}

func (p groupPolicy) CanModerate(c convlog.Conv, a types.PeerID) bool {
	r, ok := p.role(c, a)
	return ok && r.Permissions&groups.PermDeleteMessages != 0
}

// hookGroupsToLog connects the group service to the conversation log: the events that come sealed
// in group envelopes, the old-style messages (mirrored, so the chat shows everything), the way to
// hand an event to one member in its own session, and who is connected.
func (n *NodeApi) hookGroupsToLog(gf *groupsFacade) {
	gf.onEvent = func(groupID string, origin types.PeerID, raw []byte) {
		f, err := n.convsFacade()
		if err != nil {
			return
		}
		id, fresh, err := f.log.Ingest(convlog.Group(groupID), raw)
		if err != nil || !fresh {
			return
		}
		if kind, _ := convlog.Peek(raw); kind == convlog.KindMsg {
			if e, _, err := convlog.Decode(raw); err == nil {
				f.groupMessageArrived(groupID, convlog.AuthorID(e), id)
			}
		}
	}
	gf.onLegacy = func(groupID string, m GroupMessageDTO) {
		if f, err := n.convsFacade(); err == nil {
			f.mirrorGroup(groupID, m)
		}
	}
	gf.sendEvent = func(target types.PeerID, text string, raw []byte, track bool) error {
		if n.node == nil || n.node.Messenger == nil {
			return errNoChat
		}
		_, err := n.node.Messenger.SendEvent(target, text, messenger.SendOptions{ConvEvent: raw, NoTrack: !track})
		return err
	}
	gf.connected = func(id types.PeerID) bool {
		return n.node != nil && n.node.Swarm != nil && n.node.Swarm.GetPeer(id) != nil
	}
	gf.onJoined = func(groupID string, peer types.PeerID) {
		if f, err := n.convsFacade(); err == nil {
			f.markCapable(peer) // a member of a group on the log speaks it
			f.sendSync(peer, f.log.Hello([]convlog.Conv{convlog.Group(groupID)}))
		}
	}
}

// groupConv finds the group, among ours, a conversation id names.
func (f *convsFacade) groupConv(id convlog.ConvID) (convlog.Conv, bool) {
	gf, err := f.n.groupsFacade()
	if err != nil {
		return convlog.Conv{}, false
	}
	list, err := gf.List()
	if err != nil {
		return convlog.Conv{}, false
	}
	for _, g := range list {
		if c := convlog.Group(g.ID); c.ID == id {
			return c, true
		}
	}
	return convlog.Conv{}, false
}

// sharedConvs are the conversations we share with a peer that speaks the log: our direct chat, and
// every group on the log it is a member of.
func (f *convsFacade) sharedConvs(peer types.PeerID) []convlog.Conv {
	out := []convlog.Conv{convlog.DM(f.self, peer)}
	gf, err := f.n.groupsFacade()
	if err != nil {
		return out
	}
	list, _ := gf.List()
	for _, g := range list {
		if gf.isMember(g.ID, peer) && gf.LogMode(g.ID) {
			out = append(out, convlog.Group(g.ID))
		}
	}
	return out
}

const (
	antiEntropyFirst  = 30 * time.Second
	antiEntropyEvery  = 10 * time.Minute
	antiEntropyFanout = 3
)

// antiEntropy compares, every so often, each group's log with a few connected members and each
// direct chat with the other side when it is connected: that is how the members of a big group get
// what was pushed to others, and anybody what it missed.
func (f *convsFacade) antiEntropy() {
	if f.n.node == nil {
		return
	}
	ctx := f.n.node.Ctx
	wait := antiEntropyFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		f.syncGroups()
		f.syncDirect()
		wait = antiEntropyEvery
	}
}

func (f *convsFacade) syncDirect() {
	if f.n.node.Swarm == nil {
		return
	}
	for _, p := range f.n.node.Swarm.GetAllPeers() {
		if f.isCapable(p.ID()) {
			f.sendSync(p.ID(), f.log.Hello([]convlog.Conv{convlog.DM(f.self, p.ID())}))
		}
	}
}

func (f *convsFacade) syncGroups() {
	gf, err := f.n.groupsFacade()
	if err != nil || f.n.node.Swarm == nil {
		return
	}
	list, _ := gf.List()
	for _, g := range list {
		if !gf.LogMode(g.ID) {
			continue
		}
		members, err := gf.mgr.GetAuthorizedRecipients(g.ID)
		if err != nil {
			continue
		}
		var online []types.PeerID
		for _, m := range members {
			if m.PeerID != f.self && f.n.node.Swarm.GetPeer(m.PeerID) != nil {
				online = append(online, m.PeerID)
			}
		}
		rand.Shuffle(len(online), func(i, j int) { online[i], online[j] = online[j], online[i] })
		if len(online) > antiEntropyFanout {
			online = online[:antiEntropyFanout]
		}
		hello := f.log.Hello([]convlog.Conv{convlog.Group(g.ID)})
		for _, p := range online {
			f.sendSync(p, hello)
		}
	}
}

// groupMessageArrived tells the group's listeners (the current group screen) of a new message.
func (f *convsFacade) groupMessageArrived(groupID string, from types.PeerID, id convlog.EventID) {
	gf, err := f.n.groupsFacade()
	if err != nil || from == f.self {
		return
	}
	gf.emitEvent(GroupEvent{Kind: "message", GroupID: groupID, Title: gf.titleOf(groupID),
		From: from.String(), MessageID: id.String()})
}

// syncTarget names the group a sync message brings events for, with its log's fingerprint before.
func (f *convsFacade) syncTarget(m *convlog.Sync) (string, []byte) {
	ev, ok := m.M.(*convlog.Sync_Events)
	if !ok {
		return "", nil
	}
	id, err := convlog.ParseConvID(hex.EncodeToString(ev.Events.GetConv()))
	if err != nil {
		return "", nil
	}
	c, ok := f.groupConv(id)
	if !ok {
		return "", nil
	}
	meta, _ := f.log.Meta(c)
	return c.Group, meta.Digest()
}

// groupHistoryArrived tells the group's listeners that earlier messages came in.
func (f *convsFacade) groupHistoryArrived(groupID string) {
	if gf, err := f.n.groupsFacade(); err == nil {
		gf.emitEvent(GroupEvent{Kind: "history", GroupID: groupID, Title: gf.titleOf(groupID)})
	}
}

// groupFileMeta is how a group file offer rides in a log message's metadata.
type groupFileMeta struct {
	File *GroupFileDTO `json:"group_file,omitempty"`
}

// mirrorGroup records an old-style group message in the log.
func (f *convsFacade) mirrorGroup(groupID string, m GroupMessageDTO) {
	author, ok := parsePeer(m.Sender)
	if !ok {
		return
	}
	meta := ""
	if m.File != nil {
		if raw, err := json.Marshal(groupFileMeta{File: m.File}); err == nil {
			meta = string(raw)
		}
	}
	_, _ = f.log.AddLegacy(convlog.Group(groupID), []convlog.Legacy{{ID: m.ID, Author: author, TS: m.Timestamp,
		Text: m.Text, Metadata: meta}}, true)
}

const migratedGroupsKey = "cl/x/migrated-groups-v1"

// migrateGroups brings the group messages kept before the log into it, once.
func (f *convsFacade) migrateGroups() {
	if f.st.Exists([]byte(migratedGroupsKey)) {
		return
	}
	f.migMu.Lock()
	defer f.migMu.Unlock()
	if f.st.Exists([]byte(migratedGroupsKey)) {
		return
	}
	gf, err := f.n.groupsFacade()
	if err != nil {
		return
	}
	list, err := gf.List()
	if err != nil {
		return
	}
	for _, g := range list {
		c := convlog.Group(g.ID)
		_ = f.log.Open(c)
		msgs, err := gf.Messages(g.ID, 0)
		if err != nil {
			continue
		}
		old := make([]convlog.Legacy, 0, len(msgs))
		for _, m := range msgs {
			author, ok := parsePeer(m.Sender)
			if !ok {
				continue
			}
			meta := ""
			if m.File != nil {
				if raw, err := json.Marshal(groupFileMeta{File: m.File}); err == nil {
					meta = string(raw)
				}
			}
			old = append(old, convlog.Legacy{ID: m.ID, Author: author, TS: m.Timestamp, Text: m.Text, Metadata: meta})
		}
		if _, err := f.log.AddLegacy(c, old, false); err != nil {
			return // try again next start
		}
	}
	_ = f.st.Set([]byte(migratedGroupsKey), []byte{1})
}

// groupDTOOf shows a log message the way the old group API does.
func groupDTOOf(groupID string, m convlog.Message) GroupMessageDTO {
	d := GroupMessageDTO{ID: m.ID, GroupID: groupID, Sender: m.Author, Text: m.Text, Timestamp: m.TS,
		IsMine: m.Mine, Verified: true}
	if m.Metadata != "" {
		var fm groupFileMeta
		if json.Unmarshal([]byte(m.Metadata), &fm) == nil && fm.File != nil {
			d.File = fm.File
		}
	}
	return d
}
