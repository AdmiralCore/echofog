package node

import (
	"encoding/base64"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
)

// The protocol 2 groups and channels domain (T11.3d): the methods of groupBackend over the app layer.
// The DTOs are the ones the frontend knows; people are named by the node ID. What the app layer has no
// counterpart for (the snapshot file, adding a member by key, deleting a group for everybody) stays
// ErrV2NotBuilt through the embedded pending backend.

// kind names of v1 for the kinds of the app layer.
func v1Kind(k string) string {
	switch k {
	case app.GroupChannel:
		return "channel"
	case app.GroupOpen:
		return "swarm"
	}
	return "group"
}

func appKind(k string) string {
	switch k {
	case "channel":
		return app.GroupChannel
	case "swarm", "open":
		return app.GroupOpen
	case "", "group", "private":
		return app.GroupPrivate
	}
	return k // the app refuses it
}

// groupDTO is a group's line. The creator is the owner the log names; a group whose roster the log has
// not brought yet shows none.
func (b *v2MsgBackend) groupDTO(g app.GroupInfo) GroupDTO {
	out := GroupDTO{ID: g.ID, Title: g.Title, Description: g.Description, Kind: v1Kind(g.Kind), CreatedAt: g.Created,
		Members: g.Members, MyRole: g.Role}
	if g.Pending {
		return out
	}
	if r, err := b.app.GroupRights(g.ID, ""); err == nil && r.Member && !r.Send {
		out.ReadOnly = true
	}
	if ms, err := b.app.GroupMembers(g.ID); err == nil {
		for _, m := range ms {
			if m.Owner {
				out.Creator = m.Node
				break
			}
		}
	}
	return out
}

func (b *v2MsgBackend) CreateGroup(title, description, kind string) (GroupDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	g, err := b.app.CreateGroup(ctx, title, description, appKind(kind))
	if err != nil {
		return GroupDTO{}, v2Err(err)
	}
	return b.groupDTO(g), nil
}

func (b *v2MsgBackend) ListGroups() ([]GroupDTO, error) {
	gs, err := b.app.Groups()
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]GroupDTO, 0, len(gs))
	for _, g := range gs {
		out = append(out, b.groupDTO(g))
	}
	return out, nil
}

func (b *v2MsgBackend) JoinGroup(invite string) (GroupDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	g, err := b.app.JoinGroup(ctx, invite)
	if err != nil {
		return GroupDTO{}, v2Err(err)
	}
	return b.groupDTO(g), nil
}

func (b *v2MsgBackend) LeaveGroup(groupID string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.LeaveGroup(ctx, groupID))
}

// ---- members and roles -----------------------------------------------------------------------------------------

// myStanding is what I am in a group: my rank and the permissions of my role.
type myStanding struct {
	rank  int
	perms uint32
	owner bool
	roles []app.GroupRole
}

func (b *v2MsgBackend) standing(groupID string, ms []app.GroupMember) myStanding {
	var st myStanding
	roles, _ := b.app.GroupRoles(groupID)
	st.roles = roles
	for _, m := range ms {
		if !m.Self {
			continue
		}
		st.rank, st.owner = m.Rank, m.Owner
		if m.Owner {
			st.perms = convlog.PermAll
			return st
		}
		for _, r := range roles {
			if r.ID == m.Role {
				st.perms = r.Perms
			}
		}
	}
	return st
}

func (b *v2MsgBackend) ListGroupMembers(groupID string) ([]GroupMemberDTO, error) {
	ms, err := b.app.GroupMembers(groupID)
	if err != nil {
		return nil, v2Err(err)
	}
	st := b.standing(groupID, ms)
	color := map[string]uint32{}
	for _, r := range st.roles {
		color[r.ID] = r.Color
	}
	out := make([]GroupMemberDTO, 0, len(ms))
	for _, m := range ms {
		d := GroupMemberDTO{PeerID: m.Node, Role: m.Role, RoleTitle: m.Title, RoleColor: color[m.Role], IsMe: m.Self, Online: m.Online || m.Self,
			Timeout: m.Timeout}
		below := !m.Self && !m.Owner && m.Rank < st.rank
		d.CanKick = below && st.perms&convlog.PermKick != 0
		d.CanBan = below && st.perms&convlog.PermBan != 0
		d.CanTimeout = d.CanKick
		d.CanTransfer = st.owner && !m.Self
		if below && st.perms&convlog.PermManageRoles != 0 {
			for _, r := range st.roles {
				if r.Rank < st.rank && r.ID != "owner" {
					d.Roles = append(d.Roles, r.ID)
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (b *v2MsgBackend) SetGroupRole(groupID, memberID, role string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupSetRole(ctx, groupID, memberID, role))
}

func (b *v2MsgBackend) TransferGroupOwnership(groupID, memberID string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupTransfer(ctx, groupID, memberID))
}

func (b *v2MsgBackend) RemoveGroupMember(groupID, memberID string, ban bool) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupRemove(ctx, groupID, memberID, ban))
}

func (b *v2MsgBackend) GroupTimeout(groupID, memberID string, minutes int) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupTimeout(ctx, groupID, memberID, minutes))
}

func (b *v2MsgBackend) roleDTO(r app.GroupRole, st myStanding, count map[string]int) GroupRoleDTO {
	d := GroupRoleDTO{ID: r.ID, Title: r.Title, Color: r.Color, Rank: r.Rank, Perms: r.Perms, Builtin: r.Builtin, Members: count[r.ID]}
	d.CanEdit = st.perms&convlog.PermManageRoles != 0 && r.Rank < st.rank
	d.CanRemove = d.CanEdit && !r.Builtin
	return d
}

func (b *v2MsgBackend) GroupRoles(groupID string) ([]GroupRoleDTO, error) {
	roles, err := b.app.GroupRoles(groupID)
	if err != nil {
		return nil, v2Err(err)
	}
	ms, _ := b.app.GroupMembers(groupID)
	st := b.standing(groupID, ms)
	count := map[string]int{}
	for _, m := range ms {
		count[m.Role]++
	}
	out := make([]GroupRoleDTO, 0, len(roles))
	for _, r := range roles {
		out = append(out, b.roleDTO(r, st, count))
	}
	return out, nil
}

func (b *v2MsgBackend) GroupRoleSave(groupID string, r GroupRoleDTO) (GroupRoleDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	saved, err := b.app.GroupRoleSave(ctx, groupID, app.GroupRole{ID: r.ID, Title: r.Title, Color: r.Color, Rank: r.Rank, Perms: r.Perms})
	if err != nil {
		return GroupRoleDTO{}, v2Err(err)
	}
	ms, _ := b.app.GroupMembers(groupID)
	st := b.standing(groupID, ms)
	count := map[string]int{}
	for _, m := range ms {
		count[m.Role]++
	}
	return b.roleDTO(saved, st, count), nil
}

func (b *v2MsgBackend) GroupRoleRemove(groupID, roleID string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupRoleRemove(ctx, groupID, roleID))
}

func (b *v2MsgBackend) GroupModerationLog(groupID string) ([]GroupModEntry, error) {
	log, err := b.app.GroupModerationLog(groupID)
	if err != nil {
		return nil, v2Err(err)
	}
	p := b.names()
	out := make([]GroupModEntry, 0, len(log))
	for _, e := range log {
		e.Actor, e.Target = p.of(e.Actor), p.of(e.Target)
		out = append(out, e)
	}
	return out, nil
}

// ---- invitations and requests ---------------------------------------------------------------------------------

func (b *v2MsgBackend) inviteDTO(i app.GroupInvite, canRevoke bool, p *peerNames) GroupInviteDTO {
	state := "active"
	switch {
	case i.Revoked:
		state = "revoked"
	case i.Uses > 0 && i.Used >= i.Uses:
		state = "used"
	case i.Exp > 0 && i.Exp <= time.Now().Unix():
		state = "expired"
	}
	mine := i.Link != ""
	return GroupInviteDTO{N: i.Nonce, Label: i.Label, Exp: i.Exp, Uses: i.Uses, Used: i.Used, Role: i.Role, Approval: i.Approval,
		Issuer: p.of(i.Issuer), Mine: mine, State: state, Link: i.Link, WebLink: i.Web, CanRevoke: mine || canRevoke}
}

func (b *v2MsgBackend) GroupInviteCreate(groupID string, spec GroupInviteSpec) (GroupInviteDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	i, err := b.app.GroupInviteCreate(ctx, groupID, app.InviteSpec{TTL: time.Duration(spec.Hours) * time.Hour, Uses: spec.Uses,
		Role: spec.Role, Label: spec.Label, Approval: spec.Approval})
	if err != nil {
		return GroupInviteDTO{}, v2Err(err)
	}
	return b.inviteDTO(i, true, b.names()), nil
}

func (b *v2MsgBackend) GroupInvites(groupID string) ([]GroupInviteDTO, error) {
	list, err := b.app.GroupInvites(groupID)
	if err != nil {
		return nil, v2Err(err)
	}
	ms, _ := b.app.GroupMembers(groupID)
	canRevoke := b.standing(groupID, ms).perms&convlog.PermManageRoles != 0
	p := b.names()
	out := make([]GroupInviteDTO, 0, len(list))
	for _, i := range list {
		out = append(out, b.inviteDTO(i, canRevoke, p))
	}
	return out, nil
}

func (b *v2MsgBackend) GroupInviteRevoke(groupID, nonce string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupInviteRevoke(ctx, groupID, nonce))
}

// GetGroupInvite is the quick link: an invitation of the default role that lets people in for a week.
func (b *v2MsgBackend) GetGroupInvite(groupID string) (string, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	i, err := b.app.GroupInviteCreate(ctx, groupID, app.InviteSpec{TTL: 7 * 24 * time.Hour})
	if err != nil {
		return "", v2Err(err)
	}
	return i.Link, nil
}

// AddGroupMember invites a contact: a personal invitation in their chat. They are a member once they
// accept it. The argument is the contact's node ID (the key of its account is taken too, as in v1).
func (b *v2MsgBackend) AddGroupMember(groupID, pubKeyHex string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(asContact(pubKeyHex, func(node string) error { return b.app.GroupAddMember(ctx, groupID, node) }))
}

// GroupSnapshotExport writes a group file (.fogzip of protocol 2) sealed with the password.
func (b *v2MsgBackend) GroupSnapshotExport(groupID, path, password string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupSnapshotExport(ctx, groupID, path, password))
}

// GroupSnapshotImport opens a group file; the file of v1 is refused with its own text.
func (b *v2MsgBackend) GroupSnapshotImport(path, password string) (GroupSnapshotInfo, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	g, n, err := b.app.GroupSnapshotImport(ctx, path, password)
	if err != nil {
		return GroupSnapshotInfo{}, v2Err(err)
	}
	return GroupSnapshotInfo{Group: b.groupDTO(g), Events: n}, nil
}

func (b *v2MsgBackend) GroupInviteContacts(groupID string, peerIDs []string, role string) (int, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	n, err := b.app.GroupInviteContacts(ctx, groupID, peerIDs, role)
	return n, v2Err(err)
}

func (b *v2MsgBackend) InspectGroupInvite(text string) (GroupInvitePreview, error) {
	p, err := b.app.InspectGroupInvite(text)
	if err != nil {
		return GroupInvitePreview{}, v2Err(err)
	}
	return GroupInvitePreview{GroupID: p.GroupID, Title: p.Title, Kind: v1Kind(p.Kind), Approval: p.Approval, Exp: p.Expires, Joined: p.Member}, nil
}

func (b *v2MsgBackend) GroupJoinRequests(groupID string) ([]GroupJoinRequestDTO, error) {
	list, err := b.app.GroupJoinRequests(groupID)
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]GroupJoinRequestDTO, 0, len(list))
	for _, r := range list {
		out = append(out, GroupJoinRequestDTO{PeerID: r.Node, TS: r.TS, Invite: r.Invite})
	}
	return out, nil
}

func (b *v2MsgBackend) GroupJoinDecide(groupID, peerID string, accept bool) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupJoinDecide(ctx, groupID, peerID, accept))
}

// ---- channels, categories, threads ---------------------------------------------------------------------------

func (b *v2MsgBackend) channelView(groupID string, ch convlog.Channel, stats map[string]app.ChannelStat) ChannelDTO {
	d := channelDTO(ch)
	if st, ok := stats[ch.ID]; ok {
		d.Unread, d.Mentions, d.LastTS = st.Unread, st.Mentions, st.LastTS
		if st.Last != nil {
			m := b.names().msg(*st.Last)
			d.Last = &m
		}
	}
	if ch.Voice {
		if i, ok := b.app.ActiveGroupCall(groupID, ch.ID); ok {
			c := groupCall(i)
			d.Call = &c
		}
	}
	if r, err := b.app.GroupRights(groupID, ch.ID); err == nil {
		d.CanSend = r.Send && !ch.Voice && !ch.Category
		d.CanJoin = r.Voice && ch.Voice
	}
	return d
}

func (b *v2MsgBackend) ChatChannels(ref ChatRef) ([]ChannelDTO, error) {
	chs, err := b.app.GroupChannels(ref.ID)
	if err != nil {
		return nil, v2Err(err)
	}
	stats, _ := b.app.GroupChannelStats(ref.ID) // the news are a gift: the list stands without them
	out := make([]ChannelDTO, 0, len(chs))
	for _, ch := range chs {
		out = append(out, b.channelView(ref.ID, ch, stats))
	}
	return out, nil
}

func specToApp(s ChannelSpec) convlog.ChannelSpec {
	return convlog.ChannelSpec{ID: s.ID, Name: s.Name, Topic: s.Topic, Parent: s.Parent, Open: s.Open, Voice: s.Voice, ReadOnly: s.ReadOnly,
		Category: s.Category, Position: s.Position, SlowSecs: s.SlowSecs, Overrides: s.Overrides}
}

func (b *v2MsgBackend) ChatChannelSave(ref ChatRef, spec ChannelSpec) (ChannelDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	ch, err := b.app.GroupChannelSave(ctx, ref.ID, specToApp(spec))
	if err != nil {
		return ChannelDTO{}, v2Err(err)
	}
	return b.channelView(ref.ID, ch, nil), nil
}

func (b *v2MsgBackend) ChatChannelRemove(ref ChatRef, id string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupChannelRemove(ctx, ref.ID, id))
}

func (b *v2MsgBackend) ChatChannelMove(ref ChatRef, id string, index int) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupChannelMove(ctx, ref.ID, id, index))
}

func (b *v2MsgBackend) ChatChannelPlace(ref ChatRef, id, parent string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupChannelPlace(ctx, ref.ID, id, parent))
}

func (b *v2MsgBackend) ChatCategoriesSetup(ref ChatRef, textName, voiceName string) ([]string, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	ids, err := b.app.GroupCategoriesSetup(ctx, ref.ID, textName, voiceName)
	if err != nil {
		return nil, v2Err(err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

func (b *v2MsgBackend) ChatMainChannel(ref ChatRef) (ChannelDTO, error) {
	ch, err := b.app.GroupMainChannel(ref.ID)
	if err != nil {
		return ChannelDTO{}, v2Err(err)
	}
	stats, _ := b.app.GroupChannelStats(ref.ID)
	d := b.channelView(ref.ID, ch, nil)
	if st, ok := stats[""]; ok { // the main channel is the chat itself
		d.Unread, d.Mentions, d.LastTS = st.Unread, st.Mentions, st.LastTS
		if st.Last != nil {
			m := b.names().msg(*st.Last)
			d.Last = &m
		}
	}
	return d, nil
}

func (b *v2MsgBackend) ChatMainChannelSave(ref ChatRef, spec ChannelSpec) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupMainChannelSave(ctx, ref.ID, specToApp(spec)))
}

func (b *v2MsgBackend) ChatGroupPhoto(ref ChatRef, photoBase64 string) error {
	var photo []byte
	if photoBase64 != "" {
		var err error
		if photo, err = base64.StdEncoding.DecodeString(photoBase64); err != nil {
			return &v2UserError{msg: "Не удалось прочитать картинку"}
		}
	}
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.GroupPhoto(ctx, ref.ID, photo))
}

func (b *v2MsgBackend) ChatThreads(ref ChatRef) ([]ChatMessage, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	ms, err := b.app.ChatThreads(ctx, appRef(ref))
	if err != nil {
		return nil, v2Err(err)
	}
	return b.names().msgs(ms), nil
}

func (b *v2MsgBackend) ChatThreadTitle(ref ChatRef, root, title string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.ChatThreadTitle(ctx, appRef(ref), root, title))
}

// ---- the group chat of v1 ----------------------------------------------------------------------------------------

func (b *v2MsgBackend) groupMsg(p *peerNames, groupID string, m convlog.Message) GroupMessageDTO {
	d := GroupMessageDTO{ID: m.ID, GroupID: groupID, Sender: p.of(m.Author), Text: m.Text, Timestamp: m.TS, IsMine: m.Mine, Verified: true}
	if m.File != nil {
		d.File = &GroupFileDTO{ID: m.File.ID, Name: m.File.Name, Size: m.File.Size, Mime: m.File.Mime, Thumb: thumbOK(m.File.Thumb),
			TW: m.File.W, TH: m.File.H, Channel: m.Channel, Thread: m.Thread}
	}
	return d
}

func (b *v2MsgBackend) GetGroupMessages(groupID string, limit int) ([]GroupMessageDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	page, err := b.app.ChatHistory(ctx, app.ChatRef{Kind: app.KindGroup, ID: groupID}, "", limit)
	if err != nil {
		return nil, v2Err(err)
	}
	p := b.names()
	out := make([]GroupMessageDTO, 0, len(page.Messages))
	for _, m := range page.Messages {
		out = append(out, b.groupMsg(p, groupID, m))
	}
	return out, nil
}

func (b *v2MsgBackend) SendGroupMessage(groupID, text string) (GroupMessageDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	m, err := b.app.ChatSend(ctx, app.ChatRef{Kind: app.KindGroup, ID: groupID}, app.SendReq{Text: text})
	if err != nil && m.ID == "" {
		return GroupMessageDTO{}, v2Err(err)
	}
	return b.groupMsg(b.names(), groupID, m), v2Err(err)
}

// SubscribeGroupEvents tells the events of the groups: a message of another member, and a change of the
// membership (the chat was computed again). Calls and the rest of the v1 kinds are the call layer's.
func (b *v2MsgBackend) SubscribeGroupEvents(cb func(GroupEvent)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancelGroup != nil {
		b.cancelGroup()
		b.cancelGroup = nil
	}
	if cb == nil {
		return
	}
	cancelEv := b.app.SubscribeGroupEvents(func(ev app.GroupEvent) {
		switch ev.Kind {
		case "requests", "added", "removed", "history", "call", "callactive", "callend":
		default:
			return
		}
		title := ev.Title
		if title == "" {
			if g, err := b.app.GroupInfoOf(ev.Group); err == nil {
				title = g.Title
			}
		}
		cb(GroupEvent{Kind: ev.Kind, GroupID: ev.Group, Title: title, From: ev.From, Reason: ev.Reason, CallID: ev.CallID, Coordinator: ev.Coordinator, Room: ev.Room})
	})
	cancelChats := b.app.SubscribeChats(func(c app.ConvChange) {
		if c.Ref.Kind != app.KindGroup {
			return
		}
		title := ""
		if g, err := b.app.GroupInfoOf(c.Ref.ID); err == nil {
			title = g.Title
		}
		if c.Reload {
			cb(GroupEvent{Kind: "members", GroupID: c.Ref.ID, Title: title})
		}
		p := b.names()
		// only a message that is new tells the user; an edit or a reaction reaches the screen as conv:change
		fresh := make(map[string]bool, len(c.Fresh))
		for _, id := range c.Fresh {
			fresh[id] = true
		}
		for _, m := range c.Messages {
			if !m.Mine && fresh[m.ID] {
				cb(GroupEvent{Kind: "message", GroupID: c.Ref.ID, Title: title, From: p.of(m.Author), MessageID: m.ID})
			}
		}
	})
	b.cancelGroup = func() { cancelEv(); cancelChats() }
}
