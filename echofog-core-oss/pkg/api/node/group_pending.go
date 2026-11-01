package node

import "encoding/json"

// A newcomer's wait. Joining by an invitation puts the group on our list at once, but what makes it
// a group to us (the member list, the keys, the history) comes from a member who lets us in: at
// once, after an admin accepts an invitation that asks for approval, or never when no member can
// be reached. Until then the app says which of these it is instead of showing an empty chat.

const pendingPrefix = "grp/pending/" // + group: {"ap": approval asked, "sent": the join reached a member, "at": unix s}

type pendingJoin struct {
	Approval bool  `json:"ap,omitempty"`
	Sent     bool  `json:"sent,omitempty"`
	At       int64 `json:"at"`
}

// Pending states of a group we joined (GroupDTO.Pending, ConversationDTO.Pending).
const (
	PendingApproval  = "approval"  // the join reached an admin; one of them has to accept it
	PendingAdmission = "admission" // the join reached a member; the member list and keys are coming
	PendingNoRoute   = "noroute"   // the join reached nobody yet: it waits until a member is reachable
)

// notePending records that we joined groupID and wait to be let in.
func (f *groupsFacade) notePending(groupID string, approval, sent bool) {
	raw, err := json.Marshal(pendingJoin{Approval: approval, Sent: sent, At: f.clock().Unix()})
	if err == nil {
		_ = f.store.Set([]byte(pendingPrefix+groupID), raw)
	}
}

// admitted ends the wait: a member let us in (its member list or the group's keys came).
func (f *groupsFacade) admitted(groupID string) {
	k := []byte(pendingPrefix + groupID)
	if f.store.Exists(k) {
		_ = f.store.Delete(k)
	}
}

func (f *groupsFacade) pending(groupID string) (pendingJoin, bool) {
	raw, err := f.store.Get([]byte(pendingPrefix + groupID))
	if err != nil {
		return pendingJoin{}, false
	}
	var p pendingJoin
	return p, json.Unmarshal(raw, &p) == nil
}

// PendingOf is what we wait for in a group we joined ("" nothing: we are in). reachable reports
// whether the messenger has a way to the members now (a relay, or a direct link).
func (f *groupsFacade) PendingOf(groupID string, reachable func() bool) string {
	p, ok := f.pending(groupID)
	if !ok {
		return ""
	}
	switch {
	case !p.Sent && reachable != nil && !reachable():
		return PendingNoRoute
	case p.Approval:
		return PendingApproval
	default:
		return PendingAdmission
	}
}

// reachesMembers reports whether we have a way to a group's members now: a relay that carries
// messages, or a direct link to one of them.
func (f *convsFacade) reachesMembers(gf *groupsFacade, groupID string) func() bool {
	return func() bool {
		if f.n.node == nil || f.n.node.Messenger == nil {
			return false
		}
		if f.n.node.Messenger.HasRelay() {
			return true
		}
		members, err := gf.mgr.GetAuthorizedRecipients(groupID)
		if err != nil {
			return false
		}
		for _, m := range members {
			if m.PeerID != f.self && gf.connected != nil && gf.connected(m.PeerID) {
				return true
			}
		}
		return false
	}
}
