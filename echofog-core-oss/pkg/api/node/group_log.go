package node

import (
	"errors"
	"math/rand/v2"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Groups on the conversation log (docs/design/conversation-log.md, section 7). A group's events
// travel sealed with its sender key, in the same envelopes as sealed messages (marked by cevMagic),
// so they get the epochs, the envelope cache and the gap repair those already have. A member of an
// older version cannot open them as messages and drops them; that is why a group runs on the log
// only once every member speaks it (logMode), and the old way until then.

// cevMagic marks a sealed envelope whose plaintext is a conversation-log event.
var cevMagic = []byte("fogcev1\x00")

// pushFanout is how many members get a big group's event straight from its author; the rest sync
// it from them.
const pushFanout = 8

// takeEvent takes a log event that came sealed in a group envelope, from its author's member
// sender. Call with f.mu held; the log gets it once the lock is let go.
func (f *groupsFacade) takeEvent(origin types.PeerID, groupID string, sender types.PeerID, v *senderkeys.Envelope, raw []byte) []func() {
	e, _, err := convlog.Decode(raw)
	if err != nil || convlog.AuthorID(e) != sender {
		return nil
	}
	f.noteVersion(sender, wireVersion) // only a log speaker seals events
	kr := f.keyring(groupID)
	_ = f.saveKeyring(groupID)
	f.cacheEnv(groupID, v, origin, false)
	var after []func()
	if f.onEvent != nil {
		hook, ev := f.onEvent, append([]byte(nil), raw...)
		after = append(after, func() { hook(groupID, origin, ev) })
	}
	for _, st := range kr.Have() {
		if st.Sender == v.Sender && len(st.Missing) > 0 {
			after = append(after, f.requestSync(groupID, origin)...)
			break
		}
	}
	return after
}

// logModeLocked reports whether the group runs on the log: no member was last heard speaking an
// older version. A member we have not heard from yet counts as speaking it (every build
// since the log does), so one member who never comes back does not hold the group back forever.
// Call with f.mu held.
func (f *groupsFacade) logModeLocked(groupID string) bool {
	members, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return false
	}
	f.loadCapable()
	for _, m := range members {
		if m.PeerID != f.selfID && f.oldPeers[m.PeerID] {
			return false
		}
	}
	return true
}

// LogMode reports whether the group runs on the conversation log.
func (f *groupsFacade) LogMode(groupID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.load() != nil {
		return false
	}
	return f.logModeLocked(groupID)
}

// role returns a member's role (current, not banned).
func (f *groupsFacade) role(groupID string, id types.PeerID) (groups.Role, bool) {
	r, err := f.mgr.GetMemberRole(groupID, id)
	return r, err == nil
}

// pushTargets are the members an event of ours goes to: all of a small group; for a big one a few,
// connected ones first.
func (f *groupsFacade) pushTargets(members []groups.MemberProfile) []groups.MemberProfile {
	out := make([]groups.MemberProfile, 0, len(members))
	for _, m := range members {
		if m.PeerID != f.selfID {
			out = append(out, m)
		}
	}
	if len(out) <= meshLimit {
		return out
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	picked := make([]groups.MemberProfile, 0, pushFanout)
	var rest []groups.MemberProfile
	for _, m := range out {
		if f.connected != nil && f.connected(m.PeerID) && len(picked) < pushFanout {
			picked = append(picked, m)
		} else {
			rest = append(rest, m)
		}
	}
	for _, m := range rest {
		if len(picked) == pushFanout {
			break
		}
		picked = append(picked, m)
	}
	return picked
}

// postEvent hands a log event of ours to the members: sealed with the group's sender key when its
// epoch is clean, else in each member's own session (somebody no longer a member still holds the
// epoch in use, and the next is not made yet). track keeps it in the outbox until confirmed; text
// is what a message shows on a node that reads it the old way.
func (f *groupsFacade) postEvent(groupID string, raw []byte, track bool, text string) (delivered, queued, failed int) {
	members, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return 0, 0, 0
	}
	var env *senderkeys.Envelope
	f.mu.Lock()
	before := f.maybeRotate(groupID, false)
	if !f.tainted(groupID) {
		kr := f.keyring(groupID)
		if kr.Current() != nil {
			plain := append(append([]byte(nil), cevMagic...), raw...)
			if v, err := kr.Seal(plain); err == nil && f.saveKeyring(groupID) == nil {
				env = v
				f.cacheEnv(groupID, v, f.selfID, false)
			}
		}
	}
	f.mu.Unlock()
	for _, fn := range before {
		fn() // a new epoch reaches the members before the first event sealed in it
	}
	for _, m := range f.pushTargets(members) {
		var q bool
		var err error
		switch {
		case env != nil:
			q, err = f.sendTo(m.PeerID, groupWire{Kind: "smsg", Group: groupID, Env: env})
		case f.sendEvent != nil:
			err = f.sendEvent(m.PeerID, text, raw, track)
		default:
			err = errors.New("messaging is not available")
		}
		switch {
		case err != nil:
			failed++
		case q:
			queued++
		default:
			delivered++
		}
	}
	return delivered, queued, failed
}

// memberBody is a membership change of one member.
func memberBody(action uint32, member types.IdentityPublicKey, role string) *convlog.MemberBody {
	return &convlog.MemberBody{Action: action, Member: append([]byte(nil), member[:]...), Role: role}
}

// rosterLocked is the group's present membership, as the founder writes it to open the log's
// membership. Call with f.mu held.
func (f *groupsFacade) rosterLocked(groupID string) *convlog.MemberBody {
	list, err := f.mgr.ListMembers(groupID)
	if err != nil {
		return nil
	}
	b := &convlog.MemberBody{Action: convlog.MemberRoster}
	for _, m := range list {
		if m.IsBanned {
			continue
		}
		b.Roster = append(b.Roster, &convlog.RosterEntry{Member: append([]byte(nil), m.PubKey[:]...), Role: m.Role.Name})
	}
	return b
}

// memberChange hands a membership change of ours to the group's log. Call without f.mu.
func (f *groupsFacade) memberChange(groupID string, b *convlog.MemberBody) {
	if b != nil && f.onMember != nil {
		f.onMember(groupID, b)
	}
}
