package node

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/services/groups"
)

// GroupInvitePreview is what a group invitation says about its group, read from the invitation
// alone: nothing is fetched and nothing joined. A chat shows it on a card for an invitation
// someone posted; joining stays a separate, deliberate step.
type GroupInvitePreview struct {
	GroupID string `json:"groupId"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`    // group, channel or room
	Members int    `json:"members"` // the members the invitation names (who can let one in)
	// Approval: who comes with it waits until someone who lets members in accepts them
	Approval bool  `json:"approval,omitempty"`
	Exp      int64 `json:"exp,omitempty"` // until when it lets people in, unix seconds; 0 no term
	Expired  bool  `json:"expired,omitempty"`
	Joined   bool  `json:"joined,omitempty"` // I am in the group already
}

// InspectGroupInvite reads a group invitation (any of its forms) without joining: the group's
// creator signature is checked, so the title is the one its creator gave.
func (n *NodeApi) InspectGroupInvite(text string) (GroupInvitePreview, error) {
	invite := groupInviteText(text)
	var g *groups.GroupGenesisManifest
	var w groupWire
	switch {
	case strings.HasPrefix(invite, inviteScheme2):
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, inviteScheme2))
		if err != nil || json.Unmarshal(raw, &w) != nil || w.Genesis == nil {
			return GroupInvitePreview{}, errors.New("malformed invite")
		}
		g = w.Genesis
	case strings.HasPrefix(invite, inviteScheme):
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, inviteScheme))
		var old groups.GroupGenesisManifest
		if err != nil || json.Unmarshal(raw, &old) != nil {
			return GroupInvitePreview{}, errors.New("malformed invite")
		}
		g = &old
	default:
		return GroupInvitePreview{}, errors.New("not a group invite")
	}
	if groups.VerifyGenesis(g) != nil {
		return GroupInvitePreview{}, errors.New("the invitation's group is not signed by its creator")
	}
	p := GroupInvitePreview{GroupID: g.GroupID, Title: g.Title, Kind: "group", Members: len(w.Members), Approval: w.Ap}
	if len([]rune(p.Title)) > maxGroupTitle {
		p.Title = string([]rune(p.Title)[:maxGroupTitle])
	}
	switch g.Type {
	case groups.GroupTypePublicChannel:
		p.Kind = "channel"
	case groups.GroupTypeOpenSwarm:
		p.Kind = "room"
	}
	if w.Inv != nil {
		p.Exp = w.Inv.Exp
		p.Expired = w.Inv.Exp > 0 && time.Now().Unix() > w.Inv.Exp
	}
	if f, err := n.groupsFacade(); err == nil {
		f.mu.Lock()
		if f.load() == nil {
			_, err := f.mgr.GetMemberRole(g.GroupID, f.selfID)
			p.Joined = err == nil
		}
		f.mu.Unlock()
	}
	return p, nil
}
