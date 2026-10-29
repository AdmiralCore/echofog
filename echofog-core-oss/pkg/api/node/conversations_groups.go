package node

import (
	"encoding/hex"

	"encoding/json"
	"google.golang.org/protobuf/proto"
	"math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Group chats on the conversation log: who may do what in a group, which group an event names, a
// group's events and old-style messages, and the sync between members.

// groupRules tells the log about our groups: their founders, the roles, and (until a group's log
// holds its roster) who may do what now.
type groupRules struct{ n *NodeApi }

func (p groupRules) facade() (*groupsFacade, bool) {
	gf, err := p.n.groupsFacade()
	return gf, err == nil
}

// Current: what a member's role lets them do, by the group service's present state.
func (p groupRules) Current(c convlog.Conv, a types.PeerID) convlog.Rights {
	gf, ok := p.facade()
	if !ok {
		return convlog.Rights{}
	}
	r, ok := gf.role(c.Group, a)
	if !ok {
		return convlog.Rights{}
	}
	return convlog.PermRights(uint32(r.Permissions))
}

func (p groupRules) Founder(c convlog.Conv) (types.PeerID, bool) {
	gf, ok := p.facade()
	if !ok {
		return types.PeerID{}, false
	}
	g, err := gf.mgr.GetGroup(c.Group)
	if err != nil || groups.VerifyGenesis(g) != nil {
		return types.PeerID{}, false
	}
	return g.Creator, true
}

func (groupRules) Role(name string) (convlog.Role, bool) {
	r, err := groups.ResolveRole(name)
	if err != nil {
		return convlog.Role{}, false
	}
	return convlog.Role{Name: r.Name, Rank: r.Rank, Perms: uint32(r.Permissions)}, true
}

// NeedsInvite: a private group counts a member's own join only with an invitation.
func (p groupRules) NeedsInvite(c convlog.Conv) bool {
	gf, ok := p.facade()
	return ok && gf.needsInvite(c.Group)
}

func (p groupRules) DefaultRole(c convlog.Conv) string {
	if gf, ok := p.facade(); ok {
		if g, err := gf.mgr.GetGroup(c.Group); err == nil && g.Type == groups.GroupTypePublicChannel {
			return groups.RoleChannelSubscriber.Name
		}
	}
	return groups.RoleMember.Name
}

// the log's permission bits are the group service's
var _ = [1]struct{}{}[uint32(groups.PermSendMessages)^convlog.PermSend|
	uint32(groups.PermDeleteMessages)^convlog.PermDelete|uint32(groups.PermKickMembers)^convlog.PermKick|
	uint32(groups.PermBanMembers)^convlog.PermBan|uint32(groups.PermManageRoles)^convlog.PermManageRoles|
	uint32(groups.PermManageGroup)^convlog.PermManageGroup]

// hookGroupsToLog connects the group service to the conversation log: the events that come sealed
// in group envelopes, the old-style messages (mirrored, so the chat shows everything), the way to
// hand an event to one member in its own session, and who is connected.
func (n *NodeApi) hookGroupsToLog(gf *groupsFacade) {
	gf.mgr.SetRoleResolver(gf.resolveRole)
	gf.inviteState = func(groupID, nonce string) (convlog.Invite, bool) {
		f, err := n.convsFacade()
		if err != nil {
			return convlog.Invite{}, false
		}
		return f.log.InviteOf(convlog.Group(groupID), nonce)
	}
	go n.followAllRoles(gf)
	gf.onEvent = func(groupID string, origin types.PeerID, raw []byte) {
		f, err := n.convsFacade()
		if err != nil {
			return
		}
		id, fresh, err := f.log.Ingest(convlog.Group(groupID), raw)
		if err != nil || !fresh {
			return
		}
		f.spread(groupID, origin, raw)
		if kind, _ := convlog.Peek(raw); kind == convlog.KindMsg {
			if e, _, err := convlog.Decode(raw); err == nil {
				f.groupMessageArrived(groupID, convlog.AuthorID(e), id)
				if gf.joinedAfter(groupID, e.TsMs) {
					f.historySoon(groupID) // what was said before we joined, handed over by a member
				}
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
	gf.onMember = func(groupID string, b *convlog.MemberBody) {
		f, err := n.convsFacade()
		if err != nil {
			return
		}
		f.postMember(groupID, b)
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
		f.openMembership()
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
	var channel, thread string
	if m.File != nil {
		channel, thread = m.File.Channel, m.File.Thread
	}
	_, _ = f.log.AddLegacy(convlog.Group(groupID), []convlog.Legacy{{ID: m.ID, Author: author, TS: m.Timestamp,
		Text: m.Text, Metadata: meta, Channel: channel, Thread: thread}}, true)
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

// postMember writes a membership event of ours to a group's log and hands it to the members. One
// the log does not allow yet (our roster or our own admission has not arrived) is kept and sent
// all the same: it takes effect once what allows it is in the log.
func (f *convsFacade) postMember(groupID string, b *convlog.MemberBody) {
	c := convlog.Group(groupID)
	if (b.Action == convlog.MemberRemove || b.Action == convlog.MemberBan || b.Action == convlog.MemberTimeout) && len(b.Member) == 32 && b.UptoSeq == 0 {
		// the last of their events that counts: nothing they write after this is let in, wherever
		// they place it in the log's order
		var pub types.PeerPublicKey
		copy(pub[:], b.Member)
		if id := types.PeerPubKeyToID(pub); id != f.n.node.ID {
			b.UptoSeq = f.log.AuthorSeq(c, id)
		}
	}
	body, err := proto.Marshal(b)
	if err != nil {
		return
	}
	raw, id, err := f.log.AppendKept(c, convlog.Draft{Kind: convlog.KindMember, Body: body})
	if err != nil || raw == nil {
		return
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
}

// openMembership has the founder write the roster of each of its groups whose log has none yet,
// and has us ask to be let in where the log's membership does not have us although the group
// does (we joined while nobody who may let members in was around).
func (f *convsFacade) openMembership() {
	gf, err := f.n.groupsFacade()
	if err != nil {
		return
	}
	list, err := gf.List()
	if err != nil {
		return
	}
	for _, g := range list {
		f.openGroupMembership(gf, g.ID)
	}
}

func (f *convsFacade) openGroupMembership(gf *groupsFacade, groupID string) {
	if !gf.LogMode(groupID) || !gf.isMember(groupID, f.self) {
		return
	}
	c := convlog.Group(groupID)
	if !f.log.HasRoster(c) {
		if founder, ok := (groupRules{n: f.n}).Founder(c); ok && founder == f.self {
			gf.mu.Lock()
			roster := gf.rosterLocked(groupID)
			gf.mu.Unlock()
			if roster != nil {
				f.postMember(groupID, roster)
			}
		}
		return
	}
	if !f.log.Participant(c, f.self) {
		// once an hour at most: a refusal (a ban) must not fill the log with requests
		f.mu.Lock()
		asked := time.Since(f.joinAsked[groupID]) < time.Hour
		if !asked {
			f.joinAsked[groupID] = time.Now()
		}
		f.mu.Unlock()
		if !asked {
			join := memberBody(convlog.MemberJoin, gf.selfPub, "")
			if via, err := gf.store.Get([]byte(joinViaPrefix + groupID)); err == nil {
				join.Via = string(via)
			}
			if join.Via == "" && gf.needsInvite(groupID) {
				// a private group counts our own join only with an invitation: ask those who let
				// members in, who have us on their list, to write it
				gf.broadcast(groupID, groupWire{Kind: "join", Group: groupID, Pub: hex.EncodeToString(gf.selfPub[:])})
			} else {
				f.postMember(groupID, join)
			}
		}
	}
}

// spreadFanout is how many members each member hands a big group's fresh event on to.
const spreadFanout = 3

// spread passes an event that just reached us in a big group on to a few connected members: the
// author pushes to only pushFanout of them, and without this the others would wait for the next
// anti-entropy round (minutes). Each member passes an event on once, the first time it has it,
// so it reaches everybody in a few hops at about spreadFanout messages per member.
func (f *convsFacade) spread(groupID string, from types.PeerID, raw []byte) {
	gf, err := f.n.groupsFacade()
	if err != nil || gf.sendEvent == nil {
		return
	}
	e, _, err := convlog.Decode(raw)
	if err != nil {
		return
	}
	author := convlog.AuthorID(e)
	members, err := gf.mgr.GetAuthorizedRecipients(groupID)
	if err != nil || len(members) <= meshLimit {
		return
	}
	rand.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	sent := 0
	for _, m := range members {
		if sent == spreadFanout {
			break
		}
		if m.PeerID == f.self || m.PeerID == from || m.PeerID == author || gf.connected == nil || !gf.connected(m.PeerID) {
			continue
		}
		if gf.sendEvent(m.PeerID, "", raw, false) == nil {
			sent++
		}
	}
}

// historySoon tells the app, once for a burst, that a group's earlier messages came in (a newcomer's
// history arrives through the log's sync or the group's envelope sync, whichever is first).
func (f *convsFacade) historySoon(groupID string) {
	f.mu.Lock()
	if f.historyTimers == nil {
		f.historyTimers = map[string]*time.Timer{}
	}
	if _, pending := f.historyTimers[groupID]; pending {
		f.mu.Unlock()
		return
	}
	f.historyTimers[groupID] = time.AfterFunc(300*time.Millisecond, func() {
		f.mu.Lock()
		delete(f.historyTimers, groupID)
		f.mu.Unlock()
		f.groupHistoryArrived(groupID)
	})
	f.mu.Unlock()
}

// followMembers hands the removals and bans of a group's log to the group service. They used to
// reach it only through the "rm" message the remover sends once to the members it reaches; a
// member away at that moment kept the removed one on its list, handed it the group's keys when
// asked and let it back in. A removal the service already has (the "rm" came too) changes nothing.
func (f *convsFacade) followMembers(groupID string, changes []convlog.MemberChange) {
	gf, err := f.n.groupsFacade()
	if err != nil {
		return
	}
	roles := false
	for _, m := range changes {
		roles = roles || m.Action == convlog.MemberDefine
		if (m.Action == convlog.MemberRemove || m.Action == convlog.MemberBan) && m.Author != f.n.node.ID && len(m.Member) == 32 {
			gf.removedOnLog(groupID, m.Author, m.Member, m.Action == convlog.MemberBan)
		}
	}
	if roles {
		f.n.followRoles(groupID)
	}
}
