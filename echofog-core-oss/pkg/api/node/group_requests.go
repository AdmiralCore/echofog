package node

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Join requests. An invitation can ask for approval: who comes with it is not let in at once;
// every member who may let members in keeps the request, and the first who accepts it lets the
// newcomer in as usual (the log shows it to the others, whose request then goes). One who
// declines tells the newcomer, whose copy of the group goes.

const (
	reqPrefix     = "grp/req/"     // + group + "/" + peer: a request waiting here
	joinViaPrefix = "grp/joinvia/" // + group: the invitation we joined with
)

// joinRequest is one waiting request.
type joinRequest struct {
	Peer   string `json:"peer"`
	Pub    string `json:"pub"`
	TS     int64  `json:"ts"` // unix seconds
	Via    string `json:"via"`
	Label  string `json:"label,omitempty"`
	Origin string `json:"-"`
}

// GroupJoinRequestDTO is a request waiting for a decision.
type GroupJoinRequestDTO struct {
	PeerID string `json:"peerId"`
	TS     int64  `json:"ts"`
	Invite string `json:"invite,omitempty"` // the invitation's note
}

// noteRequestLocked keeps a newcomer's request, if we may decide on it. Call with f.mu held.
func (f *groupsFacade) noteRequestLocked(groupID string, origin types.PeerID, pubHex string, inv convlog.Invite) []func() {
	role, err := f.mgr.GetMemberRole(groupID, f.selfID)
	if err != nil || role.Permissions&groups.PermManageRoles == 0 {
		return nil
	}
	k := []byte(reqPrefix + groupID + "/" + origin.String())
	if f.store.Exists(k) {
		return nil
	}
	raw, err := json.Marshal(joinRequest{Peer: origin.String(), Pub: pubHex, TS: f.clock().Unix(), Via: inv.N, Label: inv.Label})
	if err != nil || f.store.Set(k, raw) != nil {
		return nil
	}
	title := f.titleOf(groupID)
	return []func(){func() {
		f.emitEvent(GroupEvent{Kind: "requests", GroupID: groupID, Title: title, From: origin.String()})
	}}
}

// dropRequestLocked forgets a request (decided, or the newcomer is in). Call with f.mu held.
func (f *groupsFacade) dropRequestLocked(groupID string, peer types.PeerID) {
	k := []byte(reqPrefix + groupID + "/" + peer.String())
	if f.store.Exists(k) {
		_ = f.store.Delete(k)
	}
}

// requestsLocked are the requests waiting in a group, the oldest first; those let in meanwhile
// (by somebody else) go. Call with f.mu held.
func (f *groupsFacade) requestsLocked(groupID string) []joinRequest {
	entries, err := f.store.FindValues([]byte(reqPrefix+groupID+"/"), 0)
	if err != nil {
		return nil
	}
	var out []joinRequest
	for _, e := range entries {
		var r joinRequest
		if json.Unmarshal(e.Value, &r) != nil {
			continue
		}
		id, ok := parsePeer(r.Peer)
		if !ok {
			continue
		}
		if _, in := f.member(groupID, id); in {
			_ = f.store.Delete(e.Key)
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

// GroupJoinRequests lists the requests to join a group waiting for us to decide.
func (n *NodeApi) GroupJoinRequests(groupID string) ([]GroupJoinRequestDTO, error) {
	gf, err := n.groupsFacade()
	if err != nil {
		return nil, err
	}
	gf.mu.Lock()
	defer gf.mu.Unlock()
	if err := gf.load(); err != nil {
		return nil, err
	}
	out := []GroupJoinRequestDTO{}
	for _, r := range gf.requestsLocked(groupID) {
		out = append(out, GroupJoinRequestDTO{PeerID: r.Peer, TS: r.TS, Invite: r.Label})
	}
	return out, nil
}

// GroupJoinDecide accepts a request (the newcomer is let in) or declines it (they are told).
func (n *NodeApi) GroupJoinDecide(groupID, peerID string, accept bool) error {
	gf, err := n.groupsFacade()
	if err != nil {
		return err
	}
	peer, ok := parsePeer(peerID)
	if !ok {
		return errors.New("bad member id")
	}
	gf.mu.Lock()
	if err := gf.load(); err != nil {
		gf.mu.Unlock()
		return err
	}
	var req *joinRequest
	for _, r := range gf.requestsLocked(groupID) {
		if strings.EqualFold(r.Peer, peerID) {
			r := r
			req = &r
		}
	}
	if req == nil {
		gf.mu.Unlock()
		return errors.New("no such request (somebody may have decided already)")
	}
	if role, err := gf.mgr.GetMemberRole(groupID, gf.selfID); err != nil || role.Permissions&groups.PermManageRoles == 0 {
		gf.mu.Unlock()
		return errors.New("your role may not let members in")
	}
	var after []func()
	if accept {
		pub, ok := parsePub(req.Pub)
		if !ok {
			gf.mu.Unlock()
			return errors.New("bad request")
		}
		after = gf.admitLocked(groupID, peer, req.Pub, pub, req.Via)
	} else {
		gf.dropRequestLocked(groupID, peer)
	}
	gf.mu.Unlock()
	for _, fn := range after {
		fn()
	}
	if !accept {
		// the newcomer's copy of the group goes: they were never let in
		_, _ = gf.sendTo(peer, groupWire{Kind: "rm", Group: groupID, Target: peerID})
	}
	gf.emitEvent(GroupEvent{Kind: "requests", GroupID: groupID, Title: gf.titleOf(groupID)})
	return nil
}

// quick invitations: the one an invitation link shows by default. In a private group whose
// membership is on the log it is an invitation on the log too (a week, no limit), kept and shown
// again until a day before it ends, so a self-join with it counts there.
const quickInvPrefix = "grp/quickinv/" // + group: {"l": link, "n": nonce, "e": end}

type quickInvite struct {
	L string `json:"l"`
	N string `json:"n"`
	E int64  `json:"e"`
}

// quickInvite is the group's default invitation link.
func (n *NodeApi) quickInvite(groupID string) (string, error) {
	gf, err := n.groupsFacade()
	if err != nil {
		return "", err
	}
	f, c, err := n.roleLog(groupID)
	if err != nil || !gf.needsInvite(groupID) {
		return gf.Invite(groupID) // an open group, or its membership is not on the log yet
	}
	k := []byte(quickInvPrefix + groupID)
	if raw, err := gf.store.Get(k); err == nil {
		var q quickInvite
		if json.Unmarshal(raw, &q) == nil && q.E > time.Now().Add(24*time.Hour).Unix() {
			if st, ok := f.log.InviteOf(c, q.N); ok && !st.Spent() {
				return q.L, nil
			}
		}
	}
	link, tok, err := gf.inviteFor(groupID, inviteTTL)
	if err != nil || tok == nil {
		return link, err
	}
	raw, id, err := f.log.AppendInvite(c, &convlog.InviteDef{N: tok.N, Exp: tok.Exp})
	if err != nil {
		return link, nil // a token alone still lets people in through a member who admits them
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
	if q, err := json.Marshal(quickInvite{L: link, N: tok.N, E: tok.Exp}); err == nil {
		_ = gf.store.Set(k, q)
	}
	return link, nil
}

// GroupTimeout keeps a member from writing for minutes (0 lifts a timeout); the member stays.
func (n *NodeApi) GroupTimeout(groupID, memberID string, minutes int) error {
	f, c, err := n.roleLog(groupID)
	if err != nil {
		return err
	}
	gf, err := n.groupsFacade()
	if err != nil {
		return err
	}
	target, ok := parsePeer(memberID)
	if !ok || target == f.self {
		return errors.New("bad member id")
	}
	if minutes < 0 || minutes > 28*24*60 {
		return errors.New("a timeout lasts up to 28 days")
	}
	gf.mu.Lock()
	m, in := gf.member(groupID, target)
	gf.mu.Unlock()
	if !in {
		return errors.New("this person is not in the group")
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute).Unix()
	if minutes == 0 {
		until = time.Now().Unix() - 1
	}
	raw, id, err := f.log.AppendTimeout(c, m.PubKey[:], until, f.log.AuthorSeq(c, target))
	if err != nil {
		if err == convlog.ErrForbidden {
			return errors.New("you may give a timeout only to a member below your rank, if you may remove members")
		}
		return err
	}
	_, _ = f.sendEvent(c, raw, id, convlog.KindMember, "", "")
	gf.emitEvent(GroupEvent{Kind: "members", GroupID: groupID, Title: gf.titleOf(groupID)})
	return nil
}

// GroupModEntry is one line of a group's moderation log.
type GroupModEntry = convlog.ModEntry

// GroupModerationLog is what a group's members did to each other, the newest first.
func (n *NodeApi) GroupModerationLog(groupID string) ([]GroupModEntry, error) {
	f, c, err := n.roleLog(groupID)
	if err != nil {
		return nil, err
	}
	out := f.log.ModLog(c, 500)
	if out == nil {
		out = []GroupModEntry{}
	}
	return out, nil
}
