package convlog

import (
	"bytes"
	"sort"
	"strings"

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
	MemberDefine uint32 = 7 // defines a role (body.def)
)

// Permission bits of a role, the same values as the group service's.
const (
	PermSend           uint32 = 1 << 0  // write messages
	PermMedia          uint32 = 1 << 1  // send files
	PermVoice          uint32 = 1 << 2  // join calls and voice rooms
	PermStream         uint32 = 1 << 3  // share a screen or a camera
	PermDelete         uint32 = 1 << 4  // delete other people's messages
	PermKick           uint32 = 1 << 5  // remove members
	PermBan            uint32 = 1 << 6  // ban members
	PermManageRoles    uint32 = 1 << 7  // give members roles, define roles
	PermManageGroup    uint32 = 1 << 8  // the group's name, description, picture
	PermLinks          uint32 = 1 << 9  // post links
	PermThreads        uint32 = 1 << 10 // write in threads (comments)
	PermPin            uint32 = 1 << 11 // pin for everyone
	PermReact          uint32 = 1 << 12 // react to messages
	PermInvite         uint32 = 1 << 13 // hand out invitations
	PermManageChannels uint32 = 1 << 14 // create, change and order channels and categories

	PermAll uint32 = 1<<15 - 1
)

// ownerRank is the rank of a group's owner: nobody acts on them.
const ownerRank = 100

const (
	maxRoleID    = 32
	maxRoleTitle = 32
	maxRoles     = 50 // custom roles of a group
)

// Role is a member's role in a group.
type Role struct {
	Name  string // its id
	Title string // what the app shows ("" the name)
	Color uint32 // 0xRRGGBB, 0 none
	Rank  int
	Perms uint32
}

// Rights are what an author may do at one point of a conversation.
type Rights struct {
	Member   bool // takes part: may edit and delete their own messages
	Send     bool // may write messages
	Media    bool // may send files
	Links    bool // may post links
	Threads  bool // may write in threads
	React    bool // may react
	Pin      bool // may pin for everyone
	Moderate bool // may delete other people's messages
	Manage   bool // may change the group's channels
	Invite   bool // may hand out invitations
	Voice    bool // may join calls and voice rooms
	Stream   bool // may share a screen or a camera
	Roles    bool // may give roles and define them
}

// PermRights are the rights of a member whose role has the permission bits perms.
func PermRights(perms uint32) Rights {
	has := func(p uint32) bool { return perms&p != 0 }
	return Rights{Member: true, Send: has(PermSend), Media: has(PermMedia), Links: has(PermLinks),
		Threads: has(PermThreads), React: has(PermReact), Pin: has(PermPin), Moderate: has(PermDelete),
		Manage: has(PermManageChannels), Invite: has(PermInvite), Voice: has(PermVoice), Stream: has(PermStream),
		Roles: has(PermManageRoles)}
}

func rightsOf(r Role) Rights { return PermRights(r.Perms) }

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
	seq    uint64 // the author's sequence number of the event
	ts     int64  // the author's clock, ms (display)
	author types.PeerID
	body   *MemberBody
}

// members is the membership history of one group, in log order.
type members struct {
	evs []memberEv
}

// memberState is who is in a group, and with which role, at one point of its log.
type memberState struct {
	roles    map[types.PeerID]Role
	banned   map[types.PeerID]bool
	defs     map[string]Role // roles defined on the log: custom ones, and changed built-in ones
	invites  map[string]*Invite
	timeouts map[types.PeerID]timeout
}

func newMemberState() *memberState {
	return &memberState{roles: map[types.PeerID]Role{}, banned: map[types.PeerID]bool{}, defs: map[string]Role{},
		invites: map[string]*Invite{}}
}

func (s *memberState) clone() *memberState {
	out := &memberState{roles: make(map[types.PeerID]Role, len(s.roles)), banned: make(map[types.PeerID]bool, len(s.banned)),
		defs: make(map[string]Role, len(s.defs)), invites: make(map[string]*Invite, len(s.invites))}
	for k, v := range s.invites {
		out.invites[k] = v // copied on write (invite, useInvite)
	}
	if len(s.timeouts) > 0 {
		out.timeouts = make(map[types.PeerID]timeout, len(s.timeouts))
		for k, v := range s.timeouts {
			out.timeouts[k] = v
		}
	}
	for k, v := range s.roles {
		out.roles[k] = v
	}
	for k, v := range s.banned {
		out.banned[k] = v
	}
	for k, v := range s.defs {
		out.defs[k] = v
	}
	return out
}

// role resolves a role's name: as the log defines it, or built in.
func (s *memberState) role(rules GroupRules, name string) (Role, bool) {
	if r, ok := s.defs[name]; ok {
		return r, true
	}
	return rules.Role(name)
}

func validRoleID(s string) bool { return len(s) <= maxRoleID && validChannelID(s) }

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
	case MemberInvite:
		if !validInvite(b.Invite) {
			return nil, false
		}
	case MemberTimeout:
		if len(b.Member) != authorLen || b.Until < 0 {
			return nil, false
		}
	case MemberDefine:
		d := b.Def
		if d == nil || !validRoleID(d.Id) || len(d.Title) > maxRoleTitle || !validText(d.Title) || d.Color > 0xFFFFFF ||
			d.Perms&^PermAll != 0 {
			return nil, false
		}
	default:
		return nil, false
	}
	return &b, true
}

// step applies one membership event (not a roster) to s and reports whether it was allowed.
func (s *memberState) step(rules GroupRules, c Conv, author types.PeerID, at pos, b *MemberBody) bool {
	switch b.Action {
	case MemberDefine:
		return s.define(rules, c, author, b.Def)
	case MemberInvite:
		return s.invite(rules, author, at, b.Invite)
	case MemberTimeout:
		return s.timeoutStep(author, b)
	}
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
		r, ok := s.role(rules, name)
		if !ok {
			return false
		}
		if author == target {
			// anyone holding an invitation joins on their own, with the group's default role
			if name != rules.DefaultRole(c) {
				return false
			}
			if ir, ok := rules.(InviteRules); ok && ir.NeedsInvite(c) {
				if inv, ok := s.invites[b.Via]; !ok || inv.Approval {
					return false // no invitation that lets them in by itself
				}
			}
		} else if !isMember || actor.Perms&PermManageRoles == 0 || (actor.Rank < ownerRank && r.Rank >= actor.Rank) {
			return false
		}
		if b.Via != "" && !s.useInvite(b.Via) {
			return false // the invitation was used up or revoked
		}
		s.roles[target] = r
		return true
	case MemberRole:
		r, ok := s.role(rules, b.Role)
		if !ok || !isMember || !targetIn || actor.Perms&PermManageRoles == 0 {
			return false
		}
		if r.Rank >= ownerRank {
			// ownership moves only from the owner, who becomes an admin
			if actor.Rank < ownerRank || author == target {
				return false
			}
			s.roles[target] = r
			if admin, ok := s.role(rules, "admin"); ok {
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

// define takes a role's definition. Who may manage roles defines roles below their own rank (the
// owner any but the owner's) and grants no permission they do not hold: nobody makes a role that
// may do more than they may. A built-in role keeps its rank and cannot go; the owner's is not
// changed. A custom role that goes leaves its members with the group's default role.
func (s *memberState) define(rules GroupRules, c Conv, author types.PeerID, d *RoleDef) bool {
	actor, isMember := s.roles[author]
	if !isMember || actor.Perms&PermManageRoles == 0 {
		return false
	}
	owner := actor.Rank >= ownerRank
	builtin, isBuiltin := rules.Role(d.Id)
	cur, exists := s.role(rules, d.Id)
	if exists && (cur.Rank >= ownerRank || (!owner && cur.Rank >= actor.Rank)) {
		return false
	}
	if d.Removed {
		if isBuiltin || !exists {
			return false
		}
		def, ok := s.role(rules, rules.DefaultRole(c))
		if !ok {
			return false
		}
		delete(s.defs, d.Id)
		for id, r := range s.roles {
			if r.Name == d.Id {
				s.roles[id] = def
			}
		}
		return true
	}
	next := Role{Name: d.Id, Title: d.Title, Color: d.Color, Rank: int(d.Rank), Perms: d.Perms}
	if isBuiltin {
		next.Rank = builtin.Rank
	} else if next.Rank < 1 || next.Rank >= ownerRank || (!owner && next.Rank >= actor.Rank) {
		return false
	}
	var had uint32
	if exists {
		had = cur.Perms
	}
	if !owner && (next.Perms&^had)&^actor.Perms != 0 {
		return false // grants what the author may not do
	}
	if !exists && s.customRoles(rules) >= maxRoles {
		return false
	}
	s.defs[d.Id] = next
	for id, r := range s.roles {
		if r.Name == d.Id {
			s.roles[id] = next
		}
	}
	return true
}

func (s *memberState) customRoles(rules GroupRules) int {
	n := 0
	for id := range s.defs {
		if _, b := rules.Role(id); !b {
			n++
		}
	}
	return n
}

// rosterState is the state a roster sets.
func rosterState(rules GroupRules, b *MemberBody) *memberState {
	s := newMemberState()
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
		allowed := ev.body.Action != MemberRoster && s.step(rules, c, ev.author, ev.at, ev.body)
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

// admittedLater is the role of an author who is not a member at p but is let in later, without
// having been removed or banned before p: a member's first messages are written concurrently with
// the event that lets them in (they joined with the invitation and wrote at once), and land before
// it in the log's order. Nobody gains anything they would not have once let in, and what a removed
// member writes after the removal stays refused.
func (m *members) admittedLater(rules GroupRules, c Conv, author types.PeerID, pub []byte, p pos) (Role, bool) {
	var role Role
	found, removed := false, false
	final, _, ok := m.walk(rules, c, func(ev memberEv, before *memberState, allowed bool) bool {
		if ev.at.less(p) {
			if allowed && (ev.body.Action == MemberRemove || ev.body.Action == MemberBan) && bytes.Equal(ev.body.Member, pub) {
				removed = true
			}
			return true
		}
		if removed {
			return false
		}
		if before != nil {
			if r, in := before.roles[author]; in {
				role, found = r, true
				return false
			}
		}
		return true
	})
	if !ok || removed {
		return Role{}, false
	}
	if !found && final != nil {
		role, found = final.roles[author]
	}
	return role, found
}

// cutOff reports whether an event numbered seq by the member pub, at p, comes after their removal
// or ban and must be refused. An author picks the Lamport of their events, so a removed member could
// place new ones before the removal, where they still were a member. Their sequence numbers only
// grow, though: a removal says the last one that counts (MemberBody.upto_seq; for a member who left,
// the leaving event's own), and a later one counts only at a point after they were let in again.
func (m *members) cutOff(rules GroupRules, c Conv, author types.PeerID, pub []byte, seq uint64, p pos) bool {
	type cut struct {
		upto     uint64
		rejoined bool
		at       pos
	}
	var cuts []cut
	m.walk(rules, c, func(ev memberEv, _ *memberState, allowed bool) bool {
		if !allowed || !bytes.Equal(ev.body.Member, pub) {
			return true
		}
		switch ev.body.Action {
		case MemberRemove, MemberBan:
			upto := ev.body.UptoSeq
			if ev.author == author {
				upto = ev.seq // they left: nothing of theirs after the leaving counts
			}
			if upto > 0 {
				cuts = append(cuts, cut{upto: upto})
			}
		case MemberJoin:
			for i := range cuts {
				if !cuts[i].rejoined {
					cuts[i].rejoined, cuts[i].at = true, ev.at
				}
			}
		}
		return true
	})
	for _, k := range cuts {
		if seq > k.upto && (!k.rejoined || !k.at.less(p)) {
			return true
		}
	}
	return false
}

// DMRights are what both sides of a direct chat may do.
var DMRights = Rights{Member: true, Send: true, Media: true, Links: true, Threads: true, React: true, Pin: true,
	Voice: true, Stream: true}

// msgAllowed: a message's content is within its author's rights (files, links). A stripped one (a
// deleted message) was weighed when it came.
func msgAllowed(e *Event, r Rights) bool {
	if Stripped(e) || r.Media && r.Links {
		return true
	}
	var b MsgBody
	if proto.Unmarshal(e.Body, &b) != nil {
		return true // applyMsg refuses it
	}
	return (r.Media || b.Metadata == "") && (r.Links || !HasLink(b.Text))
}

// editAllowed: an edit puts in no link its author may not post.
func editAllowed(e *Event, r Rights) bool {
	if r.Links || Stripped(e) {
		return true
	}
	var b EditBody
	return proto.Unmarshal(e.Body, &b) != nil || !HasLink(b.Text)
}

// HasLink reports whether a text holds a link (what a role without the right to post links may not).
func HasLink(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "://") || strings.Contains(t, "www.")
}
