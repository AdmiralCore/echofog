package convlog

import (
	"encoding/hex"
	"sort"

	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// Invitations on the log. Who may invite makes an invitation as a membership event (MemberInvite):
// how long it lets people in, how many, the role they get and a note. Its link carries a token
// with the same nonce, signed by its maker (pkg/api/node/group_invite.go). Whoever lets a newcomer
// in names the invitation in the join (MemberBody.via), and the log counts its uses there: every
// member sees the same count, and a join past the limit or after a revocation is refused
// everywhere, whichever member let the newcomer in. Two members letting people in at the same
// moment may go past the limit by the joins they did not see yet.

// In a group that takes invitations (a private one, InviteRules), a member who writes their own
// join counts only with an invitation that lets them in by itself (one that asks for approval
// does not): knowing the group's ID is not enough to be on its member list.

// InviteRules is what GroupRules tells, when it can, about a group's way in.
type InviteRules interface {
	// NeedsInvite: joining takes an invitation (a private group); an open room or a channel's
	// readers join freely.
	NeedsInvite(c Conv) bool
}

// MemberInvite makes or revokes an invitation (body.invite).
const MemberInvite uint32 = 8

const (
	maxInviteLabel = 64
	maxInvites     = 500 // invitations a group keeps, revoked ones included
)

// Invite is an invitation of a group as the log has it.
type Invite struct {
	N       string `json:"n"`
	Exp     int64  `json:"exp"`
	Uses    uint32 `json:"uses,omitempty"`
	Used    uint32 `json:"used"`
	Role    string `json:"role,omitempty"`
	Label   string `json:"label,omitempty"`
	Revoked bool   `json:"revoked,omitempty"`
	// Approval: who comes with it waits until someone who lets members in accepts them
	Approval bool         `json:"approval,omitempty"`
	Issuer   types.PeerID `json:"-"`
	Created  uint64       `json:"-"` // the clock of the event that made it: the order of the list
}

// Spent reports whether the invitation lets nobody in any more by its count or a revocation.
func (i Invite) Spent() bool { return i.Revoked || i.Uses > 0 && i.Used >= i.Uses }

func validInvite(d *InviteDef) bool {
	if d == nil || len(d.N) != 32 || len(d.Label) > maxInviteLabel || !validText(d.Label) || d.Exp <= 0 ||
		d.Role != "" && !validRoleID(d.Role) {
		return false
	}
	_, err := hex.DecodeString(d.N)
	return err == nil
}

// invite takes an invitation event: a new one from who may invite (a role for the people it lets
// in only from who manages roles, below their own rank), a revocation from its maker or from who
// manages roles. Nothing else about an invitation changes.
func (s *memberState) invite(rules GroupRules, author types.PeerID, at pos, d *InviteDef) bool {
	actor, isMember := s.roles[author]
	if !isMember {
		return false
	}
	if cur, ok := s.invites[d.N]; ok {
		if !d.Revoked || cur.Revoked || (author != cur.Issuer && actor.Perms&PermManageRoles == 0) {
			return false
		}
		next := *cur
		next.Revoked = true
		s.invites[d.N] = &next
		return true
	}
	if d.Revoked || actor.Perms&PermInvite == 0 || len(s.invites) >= maxInvites {
		return false
	}
	if d.Role != "" {
		r, ok := s.role(rules, d.Role)
		if !ok || actor.Perms&PermManageRoles == 0 || r.Rank >= ownerRank || (actor.Rank < ownerRank && r.Rank >= actor.Rank) {
			return false
		}
	}
	s.invites[d.N] = &Invite{N: d.N, Exp: d.Exp, Uses: d.Uses, Role: d.Role, Label: d.Label, Approval: d.Approval,
		Issuer: author, Created: at.L}
	return true
}

// useInvite takes one use of the invitation a join names: false when it lets nobody in.
func (s *memberState) useInvite(n string) bool {
	cur, ok := s.invites[n]
	if !ok || cur.Spent() {
		return false
	}
	next := *cur
	next.Used++
	s.invites[n] = &next
	return true
}

// Invites are a group's invitations, the newest first.
func (l *Log) Invites(c Conv) []Invite {
	rules := l.groupRules()
	if c.Kind != KindGroup || rules == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, _, ok := l.membersOf(l.bc(c.ID)).walk(rules, c, nil)
	if !ok {
		return nil
	}
	out := make([]Invite, 0, len(st.invites))
	for _, i := range st.invites {
		out = append(out, *i)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].N < out[j].N
	})
	return out
}

// InviteOf is one invitation of a group (false: the log has none such).
func (l *Log) InviteOf(c Conv, n string) (Invite, bool) {
	for _, i := range l.Invites(c) {
		if i.N == n {
			return i, true
		}
	}
	return Invite{}, false
}

// AppendInvite makes an invitation of a group, or revokes one (d.Revoked).
func (l *Log) AppendInvite(c Conv, d *InviteDef) ([]byte, EventID, error) {
	if c.Kind != KindGroup {
		return nil, EventID{}, ErrForbidden
	}
	body, err := proto.Marshal(&MemberBody{Action: MemberInvite, Invite: d})
	if err != nil {
		return nil, EventID{}, err
	}
	if _, ok := decodeMember(&Event{Kind: KindMember, Body: body}); !ok {
		return nil, EventID{}, ErrBadBody
	}
	rules := l.groupRules()
	if rules == nil {
		return nil, EventID{}, ErrForbidden
	}
	l.mu.Lock()
	st, _, ok := l.membersOf(l.bc(c.ID)).walk(rules, c, nil)
	allowed := ok && st.clone().invite(rules, l.self, atEnd, d)
	l.mu.Unlock()
	if !allowed {
		return nil, EventID{}, ErrForbidden
	}
	return l.Append(c, Draft{Kind: KindMember, Body: body})
}
