package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
)

// An invitation to a private group carries a token: a random number and an end, signed by a member
// who may invite (whose role has the permission: by default any member who takes part, not a
// restricted one or a channel's reader). A "join"
// is taken only with a valid one. Without it the invitation was only the group's genesis and its
// member list, so anyone who once saw a link, or learnt the group's ID, could send "join" to an
// admin and be handed the keys and the history.
//
// Every member checks a token the same way, by the issuer's present role: an invitation made by
// somebody who has since left, was removed or was restricted stops letting people in.

const (
	inviteTTL    = 7 * 24 * time.Hour  // how long an invitation lets people in
	inviteMaxTTL = 31 * 24 * time.Hour // a token claiming longer is not taken
)

// inviteToken is the signed part of an invitation.
type inviteToken struct {
	N   string `json:"n"` // 16 random bytes, hex: tells invitations apart
	Exp int64  `json:"e"` // until when it lets people in, unix seconds
	Iss string `json:"i"` // the issuer's identity public key, hex
	Sig string `json:"s"` // Ed25519 over inviteDigest
}

func inviteDigest(group, n string, exp int64, iss string) []byte {
	h := sha256.Sum256([]byte("echofog/group-invite/v1\n" + group + "\n" + n + "\n" + strconv.FormatInt(exp, 10) + "\n" + iss))
	return h[:]
}

// needsInvite reports whether joining the group takes an invitation token: a private group does;
// an open room and a channel's readers join freely, as they always did.
func (f *groupsFacade) needsInvite(groupID string) bool {
	g, err := f.mgr.GetGroup(groupID)
	return err == nil && g.Type == groups.GroupTypePrivateGroup
}

// newInviteToken signs an invitation of ours. Call with f.mu held.
func (f *groupsFacade) newInviteToken(groupID string) (*inviteToken, error) {
	return f.newInviteTokenFor(groupID, inviteTTL)
}

// newInviteTokenFor signs an invitation of ours that lets people in for ttl. Call with f.mu held.
func (f *groupsFacade) newInviteTokenFor(groupID string, ttl time.Duration) (*inviteToken, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	t := &inviteToken{N: hex.EncodeToString(nonce), Exp: f.clock().Add(ttl).Unix(), Iss: hex.EncodeToString(f.selfPub[:])}
	sig, err := f.sign(inviteDigest(groupID, t.N, t.Exp, t.Iss))
	if err != nil {
		return nil, err
	}
	t.Sig = hex.EncodeToString(sig)
	return t, nil
}

// inviteValid reports whether t lets somebody into the group now: not run out, signed by its
// issuer, who is a member that may let people in. Call with f.mu held.
func (f *groupsFacade) inviteValid(groupID string, t *inviteToken) bool {
	if t == nil {
		return false
	}
	now := f.clock()
	if now.Unix() > t.Exp || time.Unix(t.Exp, 0).Sub(now) > inviteMaxTTL {
		return false
	}
	id, pub, err := peerOfPubKey(t.Iss)
	if err != nil {
		return false
	}
	sig, err := hex.DecodeString(t.Sig)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub[:]), inviteDigest(groupID, t.N, t.Exp, t.Iss), sig) {
		return false
	}
	role, err := f.mgr.GetMemberRole(groupID, id)
	if err != nil || !mayInvite(role) {
		return false
	}
	// an invitation the group's log keeps: not used up, not revoked (who made it may end it early)
	if f.inviteState != nil {
		if st, ok := f.inviteState(groupID, t.N); ok && (st.Spent() || now.Unix() > st.Exp) {
			return false
		}
	}
	return true
}

// mayInvite: the role may hand out invitations, so its invitations let people in.
func mayInvite(role groups.Role) bool {
	return groups.HasPermission(role, groups.PermInvite)
}

// Managed invitations: made on the group's log with how long they let people in, how many and
// with which role, listed for the members who invite, and revoked there (convlog/invites.go).

// GroupInviteDTO is one invitation of a group.
type GroupInviteDTO struct {
	N     string `json:"n"`
	Label string `json:"label,omitempty"`
	Exp   int64  `json:"exp"`            // unix seconds
	Uses  uint32 `json:"uses,omitempty"` // 0: no limit
	Used  uint32 `json:"used"`
	Role  string `json:"role,omitempty"`
	// Approval: who comes with it waits until someone who lets members in accepts them
	Approval bool   `json:"approval,omitempty"`
	Issuer   string `json:"issuer"`
	Mine     bool   `json:"mine"`
	State    string `json:"state"`             // active, used, expired, revoked
	Link     string `json:"link,omitempty"`    // ours only: its maker keeps it
	WebLink  string `json:"webLink,omitempty"` // its https form
	// CanRevoke: its maker, or who manages roles
	CanRevoke bool `json:"canRevoke"`
}

// GroupInviteSpec is an invitation as the app wants it made.
type GroupInviteSpec struct {
	Label string `json:"label"`
	Hours int    `json:"hours"` // how long it lets people in: 1 hour to 31 days
	Uses  uint32 `json:"uses"`  // 0: no limit
	Role  string `json:"role"`  // "": the group's default role
	// Approval: who comes with it waits until someone who lets members in accepts them
	Approval bool `json:"approval"`
}

const invLinkPrefix = "grp/invlink/" // + group + "/" + n: the links of invitations we made

func (n *NodeApi) invitesLog(groupID string) (*convsFacade, convlog.Conv, *groupsFacade, error) {
	gf, err := n.groupsFacade()
	if err != nil {
		return nil, convlog.Conv{}, nil, err
	}
	if !gf.needsInvite(groupID) {
		return nil, convlog.Conv{}, nil, errors.New("anyone with the group's link may join it: it takes no invitations")
	}
	f, c, err := n.roleLog(groupID)
	return f, c, gf, err
}

// GroupInvites lists a private group's invitations, the newest first.
func (n *NodeApi) GroupInvites(groupID string) ([]GroupInviteDTO, error) {
	f, c, gf, err := n.invitesLog(groupID)
	if err != nil {
		return nil, err
	}
	me := f.log.RightsOf(c, f.self)
	now := time.Now().Unix()
	out := []GroupInviteDTO{}
	for _, i := range f.log.Invites(c) {
		d := GroupInviteDTO{N: i.N, Label: i.Label, Exp: i.Exp, Uses: i.Uses, Used: i.Used, Role: i.Role,
			Approval: i.Approval, Issuer: i.Issuer.String(), Mine: i.Issuer == f.self, State: "active"}
		switch {
		case i.Revoked:
			d.State = "revoked"
		case i.Spent():
			d.State = "used"
		case now > i.Exp:
			d.State = "expired"
		}
		if d.Mine {
			if raw, err := gf.store.Get([]byte(invLinkPrefix + groupID + "/" + i.N)); err == nil {
				d.Link = string(raw)
				d.WebLink = GroupWebLink(d.Link)
			}
		}
		d.CanRevoke = !i.Revoked && (d.Mine || me.Roles)
		out = append(out, d)
	}
	return out, nil
}

// GroupInviteCreate makes an invitation to a private group and returns it with its link.
func (n *NodeApi) GroupInviteCreate(groupID string, spec GroupInviteSpec) (GroupInviteDTO, error) {
	f, c, gf, err := n.invitesLog(groupID)
	if err != nil {
		return GroupInviteDTO{}, err
	}
	ttl := time.Duration(spec.Hours) * time.Hour
	if ttl < time.Hour || ttl > inviteMaxTTL {
		return GroupInviteDTO{}, errors.New("an invitation lasts from an hour to 31 days")
	}
	label := strings.TrimSpace(spec.Label)
	link, tok, err := gf.inviteFor(groupID, ttl, spec.Approval)
	if err != nil {
		return GroupInviteDTO{}, err
	}
	raw, id, err := f.log.AppendInvite(c, &convlog.InviteDef{N: tok.N, Exp: tok.Exp, Uses: spec.Uses, Role: spec.Role, Label: label,
		Approval: spec.Approval})
	if err != nil {
		if err == convlog.ErrForbidden {
			return GroupInviteDTO{}, errors.New("your role may not make this invitation (a role in it needs the right to manage roles, below your own)")
		}
		return GroupInviteDTO{}, err
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
	_ = gf.store.Set([]byte(invLinkPrefix+groupID+"/"+tok.N), []byte(link))
	return GroupInviteDTO{N: tok.N, Label: label, Exp: tok.Exp, Uses: spec.Uses, Role: spec.Role, Approval: spec.Approval, Issuer: f.self.String(),
		Mine: true, State: "active", Link: link, WebLink: GroupWebLink(link), CanRevoke: true}, nil
}

// GroupInviteRevoke ends an invitation: nobody new comes in with it.
func (n *NodeApi) GroupInviteRevoke(groupID, nonce string) error {
	f, c, _, err := n.invitesLog(groupID)
	if err != nil {
		return err
	}
	cur, ok := f.log.InviteOf(c, nonce)
	if !ok {
		return errors.New("no such invitation")
	}
	raw, id, err := f.log.AppendInvite(c, &convlog.InviteDef{N: nonce, Exp: cur.Exp, Revoked: true})
	if err != nil {
		if err == convlog.ErrForbidden {
			return errors.New("only its maker or who manages roles revokes an invitation")
		}
		return err
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
	return nil
}
