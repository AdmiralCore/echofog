package convlog

import (
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Per-channel permissions. A channel can let a role do more or less than it may in the rest of
// the group: an announcements channel where only moderators write, a room where only speakers
// talk, a channel where newcomers may not post links. Its overrides change only the channel's own
// permissions; the one for everyone ("*") comes first, then the one for the member's role; the
// owner is never held back. Like read-only, overrides decide what is let into the log, so every
// member weighs a message the same way. Hiding a channel from a role is not here: everybody in the
// group holds the group's keys, so that would need keys per channel.

// ChannelPerms are the permissions a channel's overrides can change.
const ChannelPerms = PermSend | PermMedia | PermLinks | PermThreads | PermReact | PermPin | PermVoice | PermStream | PermDelete

// Everyone is the role id of an override for every member.
const Everyone = "*"

const maxOverrides = 32

// Override is what one role may do in a channel otherwise than in the group.
type Override struct {
	Role  string `json:"role"`
	Allow uint32 `json:"allow,omitempty"`
	Deny  uint32 `json:"deny,omitempty"`
}

func validOverrides(list []*RoleOverride) bool {
	if len(list) > maxOverrides {
		return false
	}
	seen := map[string]bool{}
	for _, o := range list {
		if o == nil || seen[o.Role] || (o.Role != Everyone && !validRoleID(o.Role)) ||
			(o.Allow|o.Deny)&^ChannelPerms != 0 || o.Allow&o.Deny != 0 {
			return false
		}
		seen[o.Role] = true
	}
	return true
}

func overridesOf(list []*RoleOverride) []Override {
	if len(list) == 0 {
		return nil
	}
	out := make([]Override, 0, len(list))
	for _, o := range list {
		if o.Allow|o.Deny != 0 {
			out = append(out, Override{Role: o.Role, Allow: o.Allow, Deny: o.Deny})
		}
	}
	return out
}

func overridesProto(list []Override) []*RoleOverride {
	out := make([]*RoleOverride, 0, len(list))
	for _, o := range list {
		out = append(out, &RoleOverride{Role: o.Role, Allow: o.Allow, Deny: o.Deny})
	}
	return out
}

// PermsIn is what a member with role r may do in the channel.
func (ch Channel) PermsIn(r Role) uint32 {
	if r.Rank >= ownerRank {
		return r.Perms
	}
	p := r.Perms
	for _, name := range []string{Everyone, r.Name} {
		for _, o := range ch.Overrides {
			if o.Role == name {
				p = p&^o.Deny | o.Allow
			}
		}
	}
	return p
}

// overridesChange reports whether actor may turn a channel's overrides from old into next: only
// for roles below their own rank (everyone's they may always change), granting nothing they may
// not do themselves; the owner may change any.
func (s *memberState) overridesChange(rules GroupRules, actor Role, old, next []Override) bool {
	if actor.Rank >= ownerRank {
		return true
	}
	find := func(list []Override, role string) Override {
		for _, o := range list {
			if o.Role == role {
				return o
			}
		}
		return Override{Role: role}
	}
	roles := map[string]bool{}
	for _, o := range old {
		roles[o.Role] = true
	}
	for _, o := range next {
		roles[o.Role] = true
	}
	for role := range roles {
		was, now := find(old, role), find(next, role)
		if was == now {
			continue
		}
		if role != Everyone {
			if r, ok := s.role(rules, role); ok && r.Rank >= actor.Rank {
				return false
			}
		}
		if (now.Allow&^was.Allow)&^actor.Perms != 0 {
			return false // grants what the author may not do
		}
	}
	return true
}

// memberAt is the role of author at point p of a group whose membership is on the log, with the
// membership in force there (false: no roster yet, or not a member). Call with l.mu held.
func (l *Log) memberAt(c Conv, bc string, author types.PeerID, pub []byte, p pos) (Role, *memberState, bool) {
	rules := l.groupRules()
	if c.Kind != KindGroup || rules == nil {
		return Role{}, nil, false
	}
	m := l.membersOf(bc)
	st, ok := m.stateAt(rules, c, p)
	if !ok {
		return Role{}, nil, false
	}
	if r, in := st.roles[author]; in {
		return r, st, true
	}
	if p != atEnd && len(pub) == authorLen {
		if r, later := m.admittedLater(rules, c, author, pub, p); later {
			return r, st, true
		}
	}
	return Role{}, st, false
}

// eventChannel is the channel an event acts in: a message's own, or that of the message it acts on.
func (t *txn) eventChannel(e *Event) string {
	if e.Kind == KindMsg || len(e.Target) == 0 {
		return channelOf(e)
	}
	if v := t.view(hexRef(e.Target)); v.Present {
		return v.Channel
	}
	return channelOf(e)
}

// RightsIn is what peer may do in a channel of c now ("" the main one).
func (l *Log) RightsIn(c Conv, peer types.PeerID, channel string) Rights {
	l.mu.Lock()
	defer l.mu.Unlock()
	bc := l.bc(c.ID)
	r := l.rightsAt(c, bc, peer, nil, atEnd)
	if c.Kind != KindGroup || !r.Member {
		return r
	}
	ch, ok := l.storedChannel(bc, channel)
	if !ok || len(ch.Overrides) == 0 {
		return r
	}
	if role, _, ok := l.memberAt(c, bc, peer, nil, atEnd); ok {
		in := PermRights(ch.PermsIn(role))
		in.Manage, in.Invite, in.Roles = r.Manage, r.Invite, r.Roles // not the channel's to change
		return in
	}
	return r
}
