package node

import (
	"encoding/hex"
	"sort"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The chat list as the app shows it, in one call: every contact, message request and group, with
// what the log knows of each (last message, unread) and what I may do there. The app used to build
// it from five calls and its own copy of the group roles.

// ConvCan is what I may do in a chat.
type ConvCan struct {
	Send     bool `json:"send"`     // write messages
	Files    bool `json:"files"`    // send files
	Links    bool `json:"links"`    // post links
	Threads  bool `json:"threads"`  // write in threads
	React    bool `json:"react"`    // react to messages
	Pin      bool `json:"pin"`      // pin for everyone
	Manage   bool `json:"manage"`   // change the group: members' roles, its settings
	Channels bool `json:"channels"` // create, change and order channels and categories
	Roles    bool `json:"roles"`    // give roles and define them
	Invite   bool `json:"invite"`   // hand out the group's invitation
	Call     bool `json:"call"`     // start or join a call
	Stream   bool `json:"stream"`   // share a screen or a camera
}

// myRights is what I may do in c now: in a group whose log has no roster yet, by the group's
// present state.
func (f *convsFacade) myRights(c convlog.Conv) convlog.Rights {
	if c.Kind == convlog.KindDM {
		return convlog.DMRights
	}
	if !f.log.HasRoster(c) {
		return (groupRules{n: f.n}).Current(c, f.self)
	}
	return f.log.RightsOf(c, f.self)
}

// describe fills the parts of a chat-list entry that come from outside the log.
func (f *convsFacade) describe(d *ConversationDTO, c convlog.Conv) {
	switch c.Kind {
	case convlog.KindDM:
		peer := hex.EncodeToString(c.Peer[:])
		d.Kind, d.PeerID = "direct", peer
		if f.n.node == nil {
			return
		}
		d.Online = f.n.peerOnline(peer)
		if alias, avatar, _, ok := f.n.contactOf(peer); ok {
			d.Name, d.Avatar = alias, avatar
		}
		if cs := f.n.node.Contact; cs != nil {
			d.Blocked = cs.IsBlocked(peer)
		}
		if d.Name == "" {
			d.Name = peer[:8]
		}
		d.Can = ConvCan{Send: !d.Blocked, Files: !d.Blocked && d.PubKey != "", Links: !d.Blocked, Threads: !d.Blocked,
			React: d.Upgraded && !d.Blocked, Pin: d.Upgraded, Call: !d.Blocked && d.PubKey != "", Stream: !d.Blocked}
	case convlog.KindGroup:
		d.Kind = "group"
		gf, err := f.n.groupsFacade()
		if err != nil {
			return
		}
		g, gerr := gf.mgr.GetGroup(c.Group)
		role, rerr := gf.mgr.GetMemberRole(c.Group, f.self)
		list, _ := gf.mgr.ListMembers(c.Group)
		if gerr == nil {
			d.Name, d.Title = g.Title, g.Title
			if g.Type == groups.GroupTypePublicChannel {
				d.Kind = "channel"
			}
		}
		d.Members = 0
		for _, m := range list {
			if !m.IsBanned {
				d.Members++
			}
		}
		if rerr != nil {
			return // not (or no longer) a member
		}
		d.Role = role.Name
		r := f.myRights(c)
		manage := role.Permissions&(groups.PermManageRoles|groups.PermManageGroup|groups.PermManageChannels) != 0
		d.Can = ConvCan{Send: r.Send, Files: r.Send && r.Media, Links: r.Links, Threads: r.Threads,
			React: d.Upgraded && r.React, Pin: d.Upgraded && r.Pin, Manage: manage, Channels: r.Manage, Roles: r.Roles,
			Invite: r.Invite || (gerr == nil && g.Type != groups.GroupTypePrivateGroup),
			Call:   r.Voice, Stream: r.Voice && r.Stream}
	}
}

// withCan fills in what I may do with each message of c; conv is c's chat-list entry.
func (f *convsFacade) withCan(c convlog.Conv, msgs []convlog.Message, conv *ConversationDTO) []convlog.Message {
	r := f.myRights(c)
	for i := range msgs {
		m := &msgs[i]
		live := !m.Deleted
		m.Can = convlog.MsgCan{
			Edit:   conv.Upgraded && live && m.Mine && m.Kind == "text" && r.Member,
			Delete: conv.Upgraded && live && (m.Mine && r.Member || r.Moderate),
			Pin:    live && m.Thread == "" && conv.Can.Pin,
			React:  live && conv.Can.React,
			Reply:  live && conv.Can.Send,
		}
	}
	return msgs
}

// pubKeyOf is a contact's or a requester's identity key (hex), "" when unknown.
func (f *convsFacade) pubKeyOf(peer types.PeerID) string {
	id := hex.EncodeToString(peer[:])
	if cs, err := f.n.GetContacts(); err == nil {
		for _, c := range cs {
			if c.PeerID == id {
				return c.PubKey
			}
		}
	}
	if reqs, err := f.n.GetMessageRequests(); err == nil {
		for _, r := range reqs {
			if r.PeerID == id {
				return r.PubKey
			}
		}
	}
	return ""
}

// ChatRows lists every chat, the most recently active first: the contacts and message requests
// (with or without messages yet) and the groups.
func (n *NodeApi) ChatRows() ([]ConversationDTO, error) {
	f, err := n.convsFacade()
	if err != nil {
		return nil, err
	}
	f.migrate()
	f.migrateGroups()
	metas, err := f.log.Conversations()
	if err != nil {
		return nil, err
	}
	seen := map[convlog.ConvID]bool{}
	var out []ConversationDTO
	add := func(c convlog.Conv, m convlog.Meta) {
		if seen[c.ID] {
			return
		}
		seen[c.ID] = true
		out = append(out, f.conversation(c, m))
	}
	for _, m := range metas {
		if c, ok := convOfMeta(m); ok {
			add(c, m)
		}
	}
	// contacts and requests without a message yet, and groups without one
	if n.node.Contact != nil {
		if cs, err := n.GetContacts(); err == nil {
			for _, ct := range cs {
				if id, err := types.PeerIDFromString(ct.PeerID); err == nil {
					add(convlog.DM(f.self, id), convlog.Meta{})
				}
			}
		}
		if reqs, err := n.GetMessageRequests(); err == nil {
			for _, r := range reqs {
				if id, err := types.PeerIDFromString(r.PeerID); err == nil {
					add(convlog.DM(f.self, id), convlog.Meta{})
				}
			}
		}
	}
	if gf, err := n.groupsFacade(); err == nil {
		if list, err := gf.List(); err == nil {
			for _, g := range list {
				add(convlog.Group(g.ID), convlog.Meta{LastTS: g.CreatedAt})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

// dress fills in the delivery states and what I may do, for messages of c read for the app.
func (f *convsFacade) dress(c convlog.Conv, msgs []convlog.Message) []convlog.Message {
	meta, _ := f.log.Meta(c)
	conv := f.conversation(c, meta)
	return f.withCan(c, f.withStates(c, msgs), &conv)
}
