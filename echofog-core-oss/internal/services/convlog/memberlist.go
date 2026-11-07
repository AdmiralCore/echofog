package convlog

import (
	"sort"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// GroupMember is one member of a group as its log has it now.
type GroupMember struct {
	Peer types.PeerID
	Pub  [32]byte // the identity key; zero when the log never named it
	Role Role
}

// GroupBan is one banned account of a group.
type GroupBan struct {
	Peer types.PeerID
	Pub  [32]byte
}

// Members lists who is in a group by its log, with the keys the membership events name: the
// founder's roster, joins and role changes. Nil when the log holds no roster yet. The owner comes
// first, then by rank, then by id.
func (l *Log) Members(c Conv) []GroupMember {
	rules := l.groupRules()
	if c.Kind != KindGroup || rules == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.membersOf(l.bc(c.ID))
	st, _, ok := m.walk(rules, c, nil)
	if !ok {
		return nil
	}
	keys := m.keys()
	out := make([]GroupMember, 0, len(st.roles))
	for id, r := range st.roles {
		out = append(out, GroupMember{Peer: id, Pub: keys[id], Role: r})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role.Rank != out[j].Role.Rank {
			return out[i].Role.Rank > out[j].Role.Rank
		}
		return string(out[i].Peer[:]) < string(out[j].Peer[:])
	})
	return out
}

// Banned lists the accounts a group's log bans now.
func (l *Log) Banned(c Conv) []GroupBan {
	rules := l.groupRules()
	if c.Kind != KindGroup || rules == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.membersOf(l.bc(c.ID))
	st, _, ok := m.walk(rules, c, nil)
	if !ok {
		return nil
	}
	keys := m.keys()
	out := make([]GroupBan, 0, len(st.banned))
	for id := range st.banned {
		out = append(out, GroupBan{Peer: id, Pub: keys[id]})
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].Peer[:]) < string(out[j].Peer[:]) })
	return out
}

// keys collects the identity keys the membership events name, by peer id.
func (m *members) keys() map[types.PeerID][32]byte {
	out := map[types.PeerID][32]byte{}
	note := func(b []byte) {
		if id, ok := pubToID(b); ok {
			out[id] = [32]byte(b)
		}
	}
	for _, ev := range m.evs {
		note(ev.body.Member)
		for _, r := range ev.body.Roster {
			note(r.Member)
		}
	}
	return out
}
