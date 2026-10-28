package convlog

import (
	"bytes"
	"sort"

	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// Group membership on the log. The founder (named by the signed genesis) opens a group's
// membership with a roster; every join, role change, removal and ban after it is an event of the
// group's log. What an author may do at a point of the log follows from the membership events
// before that point in the log's order (Lamport clock, then id), so every device decides the same
// whatever order the events arrived in. Until a group's log holds its roster, the group's present
// state decides (GroupRules.Current), as before membership was on the log.

// KindMember changes who is in a group (body MemberBody).
const KindMember uint32 = 6

// Member actions.
const (
	MemberRoster uint32 = 1
	MemberJoin   uint32 = 2
	MemberRole   uint32 = 3
	MemberRemove uint32 = 4
	MemberBan    uint32 = 5
	MemberUnban  uint32 = 6
)

// Permission bits of a role, the same values as the group service's.
const (
	PermSend        uint32 = 1 << 0
	PermDelete      uint32 = 1 << 4
	PermKick        uint32 = 1 << 5
	PermBan         uint32 = 1 << 6
	PermManageRoles uint32 = 1 << 7
	PermManageGroup uint32 = 1 << 8
)

// ownerRank is the rank of a group's owner: nobody acts on them.
const ownerRank = 100

// Role is a member's role in a group.
type Role struct {
	Name  string
	Rank  int
	Perms uint32
}

// Rights are what an author may do at one point of a conversation.
type Rights struct {
	Member   bool // takes part: may react, edit and delete their own messages
	Send     bool // may write messages
	Pin      bool // may pin for everyone
	Moderate bool // may delete other people's messages
}

func rightsOf(r Role) Rights {
	return Rights{Member: true, Send: r.Perms&PermSend != 0,
		Pin: r.Perms&(PermManageGroup|PermDelete) != 0, Moderate: r.Perms&PermDelete != 0}
}

// GroupRules is what the log needs to know about groups from the group service.
type GroupRules interface {
	// Current is what author may do in the group now; used until the group's log has a roster.
	Current(c Conv, author types.PeerID) Rights
	// Founder is the author of the group's signed genesis, the only one who may write its roster.
	Founder(c Conv) (types.PeerID, bool)
	// Role resolves a role name.
	Role(name string) (Role, bool)
	// DefaultRole is the role a member who joins gets (a channel's readers get a read-only one).
	DefaultRole(c Conv) string
}

// pos is a point of a conversation's order.
type pos struct {
	L  uint64
	ID string
}

func (p pos) less(o pos) bool {
	if p.L != o.L {
		return p.L < o.L
	}
	return p.ID < o.ID
}

// memberEv is one membership event of a group's log.
type memberEv struct {
	at     pos
	author types.PeerID
	body   *MemberBody
}

// members is the membership history of one group, in log order.
type members struct {
	evs []memberEv
}

// memberState is who is in a group, and with which role, at one point of its log.
type memberState struct {
	roles  map[types.PeerID]Role
	banned map[types.PeerID]bool
}

func (s *memberState) clone() *memberState {
	out := &memberState{roles: make(map[types.PeerID]Role, len(s.roles)), banned: make(map[types.PeerID]bool, len(s.banned))}
	for k, v := range s.roles {
		out.roles[k] = v
	}
	for k, v := range s.banned {
		out.banned[k] = v
	}
	return out
}

func pubToID(b []byte) (types.PeerID, bool) {
	if len(b) != authorLen {
		return types.PeerID{}, false
	}
	var pub types.PeerPublicKey
	copy(pub[:], b)
	return types.PeerPubKeyToID(pub), true
}

func decodeMember(e *Event) (*MemberBody, bool) {
	var b MemberBody
	if Stripped(e) || proto.Unmarshal(e.Body, &b) != nil {
		return nil, false
	}
	switch b.Action {
	case MemberRoster:
		if len(b.Roster) == 0 || len(b.Roster) > 10000 {
			return nil, false
		}
		for _, r := range b.Roster {
			if len(r.Member) != authorLen {
				return nil, false
			}
		}
	case MemberJoin, MemberRole, MemberRemove, MemberBan, MemberUnban:
		if len(b.Member) != authorLen {
			return nil, false
		}
	default:
		return nil, false
	}
	return &b, true
}

// step applies one membership event (not a roster) to s and reports whether it was allowed.
func (s *memberState) step(rules GroupRules, c Conv, author types.PeerID, b *MemberBody) bool {
	target, ok := pubToID(b.Member)
	if !ok {
		return false
	}
	actor, isMember := s.roles[author]
	cur, targetIn := s.roles[target]
	// nobody acts on the owner; anyone else only on a lower rank
	outranks := func() bool { return cur.Rank < ownerRank && actor.Rank > cur.Rank }
	switch b.Action {
	case MemberJoin:
		if s.banned[target] {
			return false
		}
		if targetIn {
			return true // already in: nothing changes
		}
		name := b.Role
		if name == "" {
			name = rules.DefaultRole(c)
		}
		r, ok := rules.Role(name)
		if !ok {
			return false
		}
		if author == target {
			// anyone holding an invitation joins on their own, with the group's default role
			if name != rules.DefaultRole(c) {
				return false
			}
		} else if !isMember || actor.Perms&PermManageRoles == 0 || (actor.Rank < ownerRank && r.Rank >= actor.Rank) {
			return false
		}
		s.roles[target] = r
		return true
	case MemberRole:
		r, ok := rules.Role(b.Role)
		if !ok || !isMember || !targetIn || actor.Perms&PermManageRoles == 0 {
			return false
		}
		if r.Rank >= ownerRank {
			// ownership moves only from the owner, who becomes an admin
			if actor.Rank < ownerRank || author == target {
				return false
			}
			s.roles[target] = r
			if admin, ok := rules.Role("admin"); ok {
				s.roles[author] = admin
			}
			return true
		}
		if !outranks() || (actor.Rank < ownerRank && r.Rank >= actor.Rank) {
			return false
		}
		s.roles[target] = r
		return true
	case MemberRemove:
		if !targetIn {
			return false
		}
		if author != target && (!isMember || actor.Perms&PermKick == 0 || !outranks()) {
			return false
		}
		if author == target && cur.Rank >= ownerRank {
			return false // the owner hands the group over before leaving
		}
		delete(s.roles, target)
		return true
	case MemberBan:
		if !isMember || actor.Perms&PermBan == 0 || author == target || (targetIn && !outranks()) {
			return false
		}
		delete(s.roles, target)
		s.banned[target] = true
		return true
	case MemberUnban:
		if !isMember || actor.Perms&PermBan == 0 || !s.banned[target] {
			return false
		}
		delete(s.banned, target)
		return true
	}
	return false
}

// rosterState is the state a roster sets.
func rosterState(rules GroupRules, b *MemberBody) *memberState {
	s := &memberState{roles: map[types.PeerID]Role{}, banned: map[types.PeerID]bool{}}
	for _, r := range b.Roster {
		id, ok := pubToID(r.Member)
		if !ok {
			continue
		}
		if role, ok := rules.Role(r.Role); ok {
			s.roles[id] = role
		}
	}
	return s
}

// walk replays the membership history: it calls visit for each event with the state before it
// and whether it was allowed. The roster is the founder's first one in log order; the state before
// it is the roster's own (the founder lists who was in the group when membership moved onto the
// log), and membership events before it count for nothing. ok is false when there is no roster.
func (m *members) walk(rules GroupRules, c Conv, visit func(ev memberEv, before *memberState, allowed bool) bool) (final *memberState, roster pos, ok bool) {
	founder, hasFounder := rules.Founder(c)
	ri := -1
	if hasFounder {
		for i, ev := range m.evs {
			if ev.body.Action == MemberRoster && ev.author == founder {
				ri = i
				break
			}
		}
	}
	if ri < 0 {
		for _, ev := range m.evs {
			if visit != nil && !visit(ev, nil, false) {
				break
			}
		}
		return nil, pos{}, false
	}
	s := rosterState(rules, m.evs[ri].body)
	for i, ev := range m.evs {
		if i < ri {
			if visit != nil && !visit(ev, s, false) {
				return s, m.evs[ri].at, true
			}
			continue
		}
		if i == ri {
			if visit != nil && !visit(ev, s, true) {
				return s, m.evs[ri].at, true
			}
			continue
		}
		before := s
		if visit != nil {
			before = s.clone()
		}
		allowed := ev.body.Action != MemberRoster && s.step(rules, c, ev.author, ev.body)
		if visit != nil && !visit(ev, before, allowed) {
			break
		}
	}
	return s, m.evs[ri].at, true
}

// stateAt is the membership in force just before p (false: no roster yet).
func (m *members) stateAt(rules GroupRules, c Conv, p pos) (*memberState, bool) {
	var out *memberState
	final, _, ok := m.walk(rules, c, func(ev memberEv, before *memberState, _ bool) bool {
		if !ev.at.less(p) {
			out = before
			return false
		}
		return true
	})
	if !ok {
		return nil, false
	}
	if out == nil {
		out = final
	}
	return out, true
}

// allowed reports whether the membership event at p was allowed.
func (m *members) allowed(rules GroupRules, c Conv, p pos) bool {
	res := false
	m.walk(rules, c, func(ev memberEv, _ *memberState, ok bool) bool {
		if ev.at == p {
			res = ok
			return false
		}
		return true
	})
	return res
}

// mentions reports whether id took part in the group's membership history at all (an author or a
// member of a membership event): their events are kept even when not allowed, so a later
// membership event that allows them takes effect, and their sequence has no holes.
func (m *members) mentions(id types.PeerID, pub []byte) bool {
	for _, ev := range m.evs {
		if ev.author == id || bytes.Equal(ev.body.Member, pub) {
			return true
		}
		for _, r := range ev.body.Roster {
			if bytes.Equal(r.Member, pub) {
				return true
			}
		}
	}
	return false
}

func (m *members) add(ev memberEv) {
	i := sort.Search(len(m.evs), func(i int) bool { return !m.evs[i].at.less(ev.at) })
	if i < len(m.evs) && m.evs[i].at == ev.at {
		return
	}
	m.evs = append(m.evs, memberEv{})
	copy(m.evs[i+1:], m.evs[i:])
	m.evs[i] = ev
}
