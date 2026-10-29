package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"sort"
	"strings"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
)

// Roles of a group. The built-in ones (owner, admin, moderator, member, restricted; a channel's
// subscriber) come with the app; a group's log can change their permissions, title and colour and
// define roles of its own (convlog/members.go: MemberDefine), which every member weighs the same
// way. The group service follows the log's definitions through a copy kept here: it resolves a
// role's name while holding its own locks, and the log calls the service while holding the log's.

// GroupRoleDTO is one role of a group.
type GroupRoleDTO struct {
	ID      string `json:"id"`    // "" in GroupRoleSave: a new role
	Title   string `json:"title"` // what the app shows ("" the built-in name)
	Color   uint32 `json:"color"` // 0xRRGGBB, 0 none
	Rank    int    `json:"rank"`  // members act only on lower ranks; the owner is 100
	Perms   uint32 `json:"perms"` // permission bits (convlog.Perm*)
	Builtin bool   `json:"builtin"`
	Members int    `json:"members"`
	// what I may do with it: change it (and give it), remove it
	CanEdit   bool `json:"canEdit"`
	CanRemove bool `json:"canRemove"`
}

// the log's permission bits are the group service's
var _ = [1]struct{}{}[uint32(groups.PermAll)^convlog.PermAll|uint32(groups.PermSendMedia)^convlog.PermMedia|
	uint32(groups.PermJoinVoice)^convlog.PermVoice|uint32(groups.PermPublishStream)^convlog.PermStream|
	uint32(groups.PermLinks)^convlog.PermLinks|uint32(groups.PermThreads)^convlog.PermThreads|
	uint32(groups.PermPin)^convlog.PermPin|uint32(groups.PermReact)^convlog.PermReact|
	uint32(groups.PermInvite)^convlog.PermInvite|uint32(groups.PermManageChannels)^convlog.PermManageChannels]

func roleOfLog(r convlog.Role) groups.Role {
	return groups.Role{Name: r.Name, Rank: r.Rank, Permissions: groups.PermissionMask(r.Perms), Title: r.Title, Color: r.Color}
}

// resolveRole is the group service's view of a group's role definitions.
func (f *groupsFacade) resolveRole(groupID, name string) (groups.Role, bool) {
	f.rolesMu.Lock()
	defer f.rolesMu.Unlock()
	r, ok := f.roleDefs[groupID][name]
	return r, ok
}

// takeRoles takes a group's role definitions from its log: everybody who holds a role that
// changed gets its new permissions.
func (f *groupsFacade) takeRoles(groupID string, defs []convlog.Role) {
	m := make(map[string]groups.Role, len(defs))
	for _, d := range defs {
		m[d.Name] = roleOfLog(d)
	}
	f.rolesMu.Lock()
	if f.roleDefs == nil {
		f.roleDefs = map[string]map[string]groups.Role{}
	}
	same := maps.Equal(f.roleDefs[groupID], m)
	f.roleDefs[groupID] = m
	f.rolesMu.Unlock()
	if same {
		return
	}
	f.mu.Lock()
	f.mgr.RefreshRoles(groupID)
	_ = f.persist(groupID)
	f.mu.Unlock()
	f.emitEvent(GroupEvent{Kind: "members", GroupID: groupID, Title: f.titleOf(groupID)})
}

// builtinRoles are the roles a group comes with, highest first (a channel's readers are
// subscribers, a group's members members).
func builtinRoles(t groups.GroupType) []groups.Role {
	base := groups.RoleMember
	if t == groups.GroupTypePublicChannel {
		base = groups.RoleChannelSubscriber
	}
	return []groups.Role{groups.RoleOwner, groups.RoleAdmin, groups.RoleModerator, base, groups.RoleRestricted}
}

// roles are a group's roles as they are now, highest first.
func (f *groupsFacade) roles(groupID string) []groups.Role {
	g, err := f.mgr.GetGroup(groupID)
	if err != nil {
		return nil
	}
	f.rolesMu.Lock()
	defs := f.roleDefs[groupID]
	out := []groups.Role{}
	seen := map[string]bool{}
	for _, r := range builtinRoles(g.Type) {
		if d, ok := defs[r.Name]; ok {
			r = d
		}
		seen[r.Name] = true
		out = append(out, r)
	}
	for name, r := range defs {
		if !seen[name] {
			if _, err := groups.ResolveRole(name); err != nil {
				out = append(out, r)
			}
		}
	}
	f.rolesMu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rank > out[j].Rank })
	return out
}

// mayEditRole: who manages roles changes those below their rank (the owner any but the owner's).
func mayEditRole(me, r groups.Role) bool {
	return groups.HasPermission(me, groups.PermManageRoles) && r.Rank < groups.RoleOwner.Rank &&
		(me.Rank >= groups.RoleOwner.Rank || r.Rank < me.Rank)
}

// GroupRoles lists a group's roles, highest first, with what I may do with each.
func (n *NodeApi) GroupRoles(groupID string) ([]GroupRoleDTO, error) {
	gf, err := n.groupsFacade()
	if err != nil {
		return nil, err
	}
	gf.mu.Lock()
	defer gf.mu.Unlock()
	if err := gf.load(); err != nil {
		return nil, err
	}
	me, meErr := gf.mgr.GetMemberRole(groupID, gf.selfID)
	members, _ := gf.mgr.ListMembers(groupID)
	count := map[string]int{}
	for _, m := range members {
		count[m.Role.Name]++
	}
	out := []GroupRoleDTO{}
	for _, r := range gf.roles(groupID) {
		_, berr := groups.ResolveRole(r.Name)
		d := GroupRoleDTO{ID: r.Name, Title: r.Title, Color: r.Color, Rank: r.Rank, Perms: uint32(r.Permissions),
			Builtin: berr == nil, Members: count[r.Name]}
		if meErr == nil && mayEditRole(me, r) {
			d.CanEdit, d.CanRemove = true, !d.Builtin
		}
		out = append(out, d)
	}
	return out, nil
}

// roleLog is a group's log, once its membership is on it (roles are defined there).
func (n *NodeApi) roleLog(groupID string) (*convsFacade, convlog.Conv, error) {
	f, err := n.convsFacade()
	if err != nil {
		return nil, convlog.Conv{}, err
	}
	c := convlog.Group(groupID)
	if !f.log.HasRoster(c) {
		return nil, c, errors.New("the group's members are not on its log yet: try again in a minute")
	}
	return f, c, nil
}

// GroupRoleSave defines a role (r.ID "": a new one) or changes one: its title, colour, rank and
// permissions. A built-in role keeps its rank.
func (n *NodeApi) GroupRoleSave(groupID string, r GroupRoleDTO) (GroupRoleDTO, error) {
	f, c, err := n.roleLog(groupID)
	if err != nil {
		return GroupRoleDTO{}, err
	}
	r.Title = strings.TrimSpace(r.Title)
	if r.ID == "" {
		if r.Title == "" {
			return GroupRoleDTO{}, errors.New("a new role needs a name")
		}
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		r.ID = "r-" + hex.EncodeToString(b)
	}
	if len([]rune(r.Title)) > 32 {
		return GroupRoleDTO{}, errors.New("a role's name is 32 characters at most")
	}
	raw, id, err := f.log.AppendRole(c, &convlog.RoleDef{Id: r.ID, Title: r.Title, Color: r.Color, Rank: int32(r.Rank),
		Perms: r.Perms & convlog.PermAll})
	if err != nil {
		return GroupRoleDTO{}, roleError(err)
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
	n.followRoles(groupID)
	return r, nil
}

// GroupRoleRemove removes a custom role: its members get the group's default role.
func (n *NodeApi) GroupRoleRemove(groupID, roleID string) error {
	f, c, err := n.roleLog(groupID)
	if err != nil {
		return err
	}
	raw, id, err := f.log.AppendRole(c, &convlog.RoleDef{Id: roleID, Removed: true})
	if err != nil {
		return roleError(err)
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
	n.followRoles(groupID)
	return nil
}

func roleError(err error) error {
	switch err {
	case convlog.ErrForbidden:
		return errors.New("you may change only roles below your own, granting only what you may do yourself")
	case convlog.ErrBadBody:
		return errors.New("the role is not valid")
	}
	return err
}

// followRoles hands a group's role definitions on its log to the group service.
func (n *NodeApi) followRoles(groupID string) {
	f, err := n.convsFacade()
	if err != nil {
		return
	}
	gf, err := n.groupsFacade()
	if err != nil {
		return
	}
	gf.takeRoles(groupID, f.log.Roles(convlog.Group(groupID)))
}

// followAllRoles takes the role definitions of every group (at start).
func (n *NodeApi) followAllRoles(gf *groupsFacade) {
	list, err := gf.List()
	if err != nil {
		return
	}
	for _, g := range list {
		n.followRoles(g.ID)
	}
}
