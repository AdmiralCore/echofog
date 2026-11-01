package convlog

import (
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// Timeouts. A moderator can keep a member from writing for a while without removing them: a
// membership event (MemberTimeout) with an end. The member stays in, reads, keeps the keys; what
// they write or react with in the meantime is refused everywhere. The log has no clock everybody
// trusts, so the time that counts is the event's own (Event.ts_ms, the author's clock) together
// with their sequence number: a timeout says the member's last event before it (upto_seq, as a
// removal does), and any later one stamped before the end is refused, wherever its author placed
// it in the log's order. A member who stamps their messages with a time after the end gets them
// in, shown at that time. Lifting a timeout early is a timeout that ends now: what was refused
// meanwhile stays refused.

// MemberTimeout keeps a member from writing until body.until (unix seconds).
const MemberTimeout uint32 = 9

// maxTimeout is the longest timeout.
const maxTimeout = 28 * 24 * 60 * 60

type timeout struct {
	until int64  // unix seconds
	upto  uint64 // the member's last event before it
}

// timeoutStep takes a timeout: from who may remove members, on a member of lower rank.
func (s *memberState) timeoutStep(author types.PeerID, b *MemberBody) bool {
	target, ok := pubToID(b.Member)
	if !ok || target == author {
		return false
	}
	actor, isMember := s.roles[author]
	cur, in := s.roles[target]
	if !isMember || !in || actor.Perms&PermKick == 0 || cur.Rank >= ownerRank || actor.Rank <= cur.Rank {
		return false
	}
	if s.timeouts == nil {
		s.timeouts = map[types.PeerID]timeout{}
	}
	s.timeouts[target] = timeout{until: b.Until, upto: b.UptoSeq}
	return true
}

// timedOut reports whether an event numbered seq by author, stamped tsMs, falls in their timeout.
func (m *members) timedOut(rules GroupRules, c Conv, author types.PeerID, seq uint64, tsMs int64) bool {
	final, _, ok := m.walk(rules, c, nil)
	if !ok {
		return false
	}
	t, ok := final.timeouts[author]
	return ok && seq > t.upto && tsMs < t.until*1000
}

// TimeoutOf is until when peer may not write in c (unix seconds; 0: no timeout running now).
func (l *Log) TimeoutOf(c Conv, peer types.PeerID) int64 {
	rules := l.groupRules()
	if c.Kind != KindGroup || rules == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.timeoutLocked(c, l.bc(c.ID), peer)
}

func (l *Log) timeoutLocked(c Conv, bc string, peer types.PeerID) int64 {
	rules := l.groupRules()
	if rules == nil {
		return 0
	}
	final, _, ok := l.membersOf(bc).walk(rules, c, nil)
	if !ok {
		return 0
	}
	if t, ok := final.timeouts[peer]; ok && t.until > l.now().Unix() {
		return t.until
	}
	return 0
}

// muted takes from r what a member in a timeout may not do now.
func muted(r Rights) Rights {
	r.Send, r.Threads, r.React, r.Pin, r.Media, r.Links, r.Voice, r.Stream = false, false, false, false, false, false, false, false
	return r
}

// timedOutKind: the kinds of event a timeout refuses (deleting one's own messages stays).
func timedOutKind(k uint32) bool {
	return k == KindMsg || k == KindEdit || k == KindReact || k == KindPin || k == KindThread
}

// AppendTimeout keeps member (identity key) from writing in a group until until (unix seconds; a
// time already past lifts a timeout). upto is the member's last event seen (AuthorSeq).
func (l *Log) AppendTimeout(c Conv, member []byte, until int64, upto uint64) ([]byte, EventID, error) {
	if c.Kind != KindGroup || len(member) != authorLen {
		return nil, EventID{}, ErrBadBody
	}
	b := &MemberBody{Action: MemberTimeout, Member: append([]byte(nil), member...), Until: until, UptoSeq: upto}
	body, err := proto.Marshal(b)
	if err != nil {
		return nil, EventID{}, err
	}
	rules := l.groupRules()
	if rules == nil {
		return nil, EventID{}, ErrForbidden
	}
	// weighed first: a refused membership event would stay in the history kept in memory
	l.mu.Lock()
	st, _, ok := l.membersOf(l.bc(c.ID)).walk(rules, c, nil)
	allowed := ok && st.clone().timeoutStep(l.self, b)
	l.mu.Unlock()
	if !allowed {
		return nil, EventID{}, ErrForbidden
	}
	return l.Append(c, Draft{Kind: KindMember, Body: body})
}
