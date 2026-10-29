package convlog

import (
	"encoding/json"
	"sort"
)

// The moderation log of a group: what its members did to each other, read from the log itself
// (nothing is kept apart, so every member sees the same, and nobody can tidy it up afterwards):
// who let whom in, who left or was removed, banned, given a role or a timeout, which roles and
// invitations were made or ended, and which messages a moderator deleted. Only what the log let
// in is shown.

// ModEntry is one line of a group's moderation log.
type ModEntry struct {
	TS      int64  `json:"ts"`               // the actor's clock, ms
	Actor   string `json:"actor"`            // peer id hex
	Action  string `json:"action"`           // see modAction
	Target  string `json:"target,omitempty"` // the member acted on, peer id hex
	Role    string `json:"role,omitempty"`   // a role given, defined or removed; an invitation's role
	Detail  string `json:"detail,omitempty"` // a role's title, an invitation's note
	Until   int64  `json:"until,omitempty"`  // a timeout's end, unix seconds
	Message string `json:"message,omitempty"`
	Channel string `json:"channel,omitempty"`
	l       uint64
}

func modAction(ev memberEv, target string) string {
	b := ev.body
	self := target == peerHex(ev.author)
	switch b.Action {
	case MemberRoster:
		return "roster"
	case MemberJoin:
		if self {
			return "join"
		}
		return "admit"
	case MemberRole:
		return "role"
	case MemberRemove:
		if self {
			return "leave"
		}
		return "remove"
	case MemberBan:
		return "ban"
	case MemberUnban:
		return "unban"
	case MemberDefine:
		if b.Def.Removed {
			return "roleRemoved"
		}
		return "roleDefined"
	case MemberInvite:
		if b.Invite.Revoked {
			return "inviteRevoked"
		}
		return "invite"
	case MemberTimeout:
		if b.Until*1000 <= ev.ts {
			return "timeoutLifted"
		}
		return "timeout"
	}
	return ""
}

// ModLog is the moderation log of a group, the newest first, at most limit lines (0: 500).
func (l *Log) ModLog(c Conv, limit int) []ModEntry {
	rules := l.groupRules()
	if c.Kind != KindGroup || rules == nil {
		return nil
	}
	if limit <= 0 {
		limit = 500
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bc := l.bc(c.ID)
	var out []ModEntry
	l.membersOf(bc).walk(rules, c, func(ev memberEv, _ *memberState, allowed bool) bool {
		if !allowed {
			return true
		}
		target := ""
		if id, ok := pubToID(ev.body.Member); ok {
			target = peerHex(id)
		}
		e := ModEntry{TS: ev.ts, Actor: peerHex(ev.author), Action: modAction(ev, target), Target: target, l: ev.at.L}
		switch ev.body.Action {
		case MemberRole, MemberJoin:
			e.Role = ev.body.Role
		case MemberDefine:
			e.Role, e.Detail = ev.body.Def.Id, ev.body.Def.Title
		case MemberInvite:
			e.Role, e.Detail = ev.body.Invite.Role, ev.body.Invite.Label
		case MemberTimeout:
			e.Until = ev.body.Until
		}
		out = append(out, e)
		return true
	})
	// messages a moderator deleted: in the views, where each delete is kept with who did it
	if entries, err := l.st.FindValues(bs(pfxView+bc+"/"), 0); err == nil {
		for _, en := range entries {
			var v View
			if json.Unmarshal(en.Value, &v) != nil || len(v.Deletes) == 0 {
				continue
			}
			for who, r := range v.Deletes {
				if r.Mod && who != v.Author {
					out = append(out, ModEntry{TS: r.TS, Actor: who, Action: "delete", Target: v.Author, Message: v.ID, Channel: v.Channel, l: r.L})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].l != out[j].l {
			return out[i].l > out[j].l
		}
		return out[i].TS > out[j].TS
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
