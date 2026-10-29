package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/echofog/echofog-core/internal/services/groups"
)

// An invitation to a private group carries a token: a random number and an end, signed by a member
// who may invite (any member who takes part: not a restricted one or a channel's reader). A "join"
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
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	t := &inviteToken{N: hex.EncodeToString(nonce), Exp: f.clock().Add(inviteTTL).Unix(), Iss: hex.EncodeToString(f.selfPub[:])}
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
	return err == nil && mayInvite(role)
}

// mayInvite: the role takes part in the group (it may write), so its invitations let people in.
func mayInvite(role groups.Role) bool {
	return groups.HasPermission(role, groups.PermSendMessages)
}
