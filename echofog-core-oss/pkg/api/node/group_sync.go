package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/google/uuid"
)

// Groups have no server: every change is a message to each member, carried like any other chat
// message (end-to-end encrypted, confirmed, queued while the member is offline) but marked with
// wirePrefix so that it is never shown as a chat line. The sender of a message is the peer whose
// encrypted session it came through, so a member cannot speak for another; messages of version 2
// are sealed and signed on top of that (group_keys.go), so they can be handed on by any member.
const wirePrefix = "fogrp2:"

type wireMember struct {
	ID   string `json:"id"`
	Pub  string `json:"p"`
	Role string `json:"r"`
	V    int    `json:"v,omitempty"` // the member's version, as far as the sender knows
}

type groupWire struct {
	Kind    string                       `json:"k"` // msg join snap add role rm call callalive callend rcall rcallalive
	Group   string                       `json:"g"`
	ID      string                       `json:"i,omitempty"`
	Text    string                       `json:"t,omitempty"`
	TS      int64                        `json:"ts,omitempty"`
	Pub     string                       `json:"p,omitempty"`
	Target  string                       `json:"to,omitempty"`
	Role    string                       `json:"r,omitempty"`
	Ban     bool                         `json:"b,omitempty"`
	Genesis *groups.GroupGenesisManifest `json:"gen,omitempty"`
	Members []wireMember                 `json:"m,omitempty"`
	CallID  string                       `json:"c,omitempty"`
	Room    string                       `json:"rm,omitempty"` // a call: the voice room it is in ("" the group's own call)
	Coord   string                       `json:"co,omitempty"`
	History []wireHistory                `json:"h,omitempty"`
	File    *wireFile                    `json:"f,omitempty"`   // msg: a file posted in the group
	Inv     *inviteToken                 `json:"inv,omitempty"` // invite, join: what lets the newcomer into a private group
	Ap      bool                         `json:"ap,omitempty"`  // invite: someone accepts each newcomer (shown to the newcomer only)

	V      int                     `json:"v,omitempty"`    // version of the sender (2: sealed messages)
	Env    *senderkeys.Envelope    `json:"env,omitempty"`  // smsg: a sealed message
	Envs   []*senderkeys.Envelope  `json:"envs,omitempty"` // envs: sealed messages handed on
	Epoch  *senderkeys.Epoch       `json:"ep,omitempty"`   // key: an epoch
	Have   []senderkeys.ChainState `json:"have,omitempty"` // sync: where the asker stands
	Epochs []senderkeys.EpochID    `json:"eps,omitempty"`  // sync: the epochs the asker holds
}

// wireFile is a file offer in a group message: one transfer ID and key for all members, each of
// whom pulls the file from the sender (see filexfer.PrepareGroup). The key stays in the transfer
// record and never in the stored message.
type wireFile struct {
	ID   string `json:"i"`
	Name string `json:"n"`
	Size int64  `json:"s"`
	Mime string `json:"m,omitempty"`
	Root string `json:"r"`
	Key  string `json:"k"`
	// a picture's thumbnail (base64 JPEG) and its size, signed with the offer
	Thumb string `json:"t,omitempty"`
	TW    int    `json:"tw,omitempty"`
	TH    int    `json:"th,omitempty"`
	// where in a space it was posted: the channel ("" the main one) and the thread's root
	Channel string `json:"ch,omitempty"`
	Thread  string `json:"rt,omitempty"`
}

// wireHistory is one old message that an admin hands to a newcomer, with its author's signature
// (absent for messages of members of an older version), so the admin cannot change it.
type wireHistory struct {
	ID     string `json:"i"`
	Sender string `json:"s"`
	Text   string `json:"t"`
	TS     int64  `json:"ts"`
	Sig    []byte `json:"sig,omitempty"`
	FD     string `json:"fd,omitempty"`
}

const (
	historyLimit     = 100       // messages an admin hands to a newcomer
	historyChunk     = 20        // messages per network message
	historyChunkSize = 24 * 1024 // and bytes
	historyPause     = 5 * time.Minute
)

// GroupEvent tells the application that something changed in a group.
type GroupEvent struct {
	Kind        string `json:"kind"` // message members call callactive callend added removed
	GroupID     string `json:"groupId"`
	Title       string `json:"title,omitempty"`
	From        string `json:"from,omitempty"`
	MessageID   string `json:"messageId,omitempty"`
	CallID      string `json:"callId,omitempty"`
	Coordinator string `json:"coordinator,omitempty"`
	Reason      string `json:"reason,omitempty"` // removed: "deleted" when the owner deleted the group
	Room        string `json:"room,omitempty"`   // a call: the voice room it is in ("" the group's own call)
}

// GroupCall is a call that a member started in a group.
type GroupCall struct {
	Room string `json:"room,omitempty"` // the voice room ("" the group's own call)
	// Members are who is in the call: those who announced it lately (everybody in a call repeats
	// the announcement every callAliveEvery) and have not said they left.
	Members     []string `json:"members,omitempty"`
	CallID      string   `json:"callId"`
	Coordinator string   `json:"coordinator"`
	From        string   `json:"from"`
	StartedAt   int64    `json:"startedAt"`
	LastSeen    int64    `json:"lastSeen"` // when somebody in the call last said it is still going
}

// A group call is live while its members keep announcing it: everyone in the call repeats the
// announcement every callAliveEvery (KeepCallAlive), and a call nobody announced for callAliveFor
// is over, even if its last member vanished without saying goodbye.
const (
	callAliveEvery = 45 * time.Second
	callAliveFor   = 150 * time.Second
	// an announcement that arrives this late (queued while the peer was unreachable) no longer rings
	callAnnounceStale = 2 * time.Minute
)

func encodeWire(w groupWire) string {
	raw, _ := json.Marshal(w)
	return wirePrefix + string(raw)
}

func parsePeer(s string) (types.PeerID, bool) {
	raw, err := hex.DecodeString(s)
	var id types.PeerID
	if err != nil || len(raw) != len(id) {
		return id, false
	}
	copy(id[:], raw)
	return id, true
}

func parsePub(s string) (types.IdentityPublicKey, bool) {
	raw, err := hex.DecodeString(s)
	var k types.IdentityPublicKey
	if err != nil || len(raw) != len(k) {
		return k, false
	}
	copy(k[:], raw)
	return k, true
}

func (f *groupsFacade) wireMembers(groupID string) []wireMember {
	list, err := f.mgr.ListMembers(groupID)
	if err != nil {
		return nil
	}
	out := make([]wireMember, 0, len(list))
	for _, m := range list {
		w := wireMember{ID: m.PeerID.String(), Pub: hex.EncodeToString(m.PubKey[:]), Role: m.Role.Name,
			V: f.memberVersion(m.PeerID)}
		out = append(out, w)
	}
	return out
}

// inviteMembers are the members an invitation names: the inviter and those who let newcomers in
// (they hand the newcomer the whole list), so the invitation stays short enough for a messenger
// whatever the group's size.
func (f *groupsFacade) inviteMembers(groupID string) []wireMember {
	const most = 16
	all := f.wireMembers(groupID)
	out := make([]wireMember, 0, most)
	for _, w := range all {
		id, ok := parsePeer(w.ID)
		if !ok {
			continue
		}
		if r, err := f.mgr.GetMemberRole(groupID, id); id == f.selfID || err == nil && groups.HasPermission(r, groups.PermManageRoles) {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID == f.selfID.String() || out[j].ID == f.selfID.String() {
			return out[i].ID == f.selfID.String()
		}
		ri, _ := groups.ResolveRole(out[i].Role)
		rj, _ := groups.ResolveRole(out[j].Role)
		return ri.Rank > rj.Rank
	})
	if len(out) > most {
		out = out[:most]
	}
	return out
}

// noteVersions takes the versions of the members in a list from an admin or an inviter, so a
// member seals for those it has not heard from itself yet. Call with f.mu held.
func (f *groupsFacade) noteVersions(list []wireMember) {
	for _, w := range list {
		// second hand: a member the lister has not heard from is listed as old, so a list only
		// ever tells us that somebody is newer
		if id, ok := parsePeer(w.ID); ok && id != f.selfID && w.V >= sealedVersion {
			f.noteVersion(id, w.V)
		}
	}
}

func memberProfiles(list []wireMember) []groups.MemberProfile {
	out := make([]groups.MemberProfile, 0, len(list))
	for _, w := range list {
		id, ok1 := parsePeer(w.ID)
		pub, ok2 := parsePub(w.Pub)
		role, err := groups.ResolveRole(w.Role)
		if err != nil {
			role = groups.Role{Name: w.Role} // a group's own role: MergeMembers resolves it
		}
		if ok1 && ok2 && w.Role != "" {
			out = append(out, groups.MemberProfile{PeerID: id, PubKey: pub, Role: role})
		}
	}
	return out
}

// sendTo sends one group message to a member and says how it went.
func (f *groupsFacade) sendTo(target types.PeerID, w groupWire) (queued bool, err error) {
	if f.sendSilent == nil {
		return false, errors.New("messaging is not available")
	}
	w.V = wireVersion
	return f.sendSilent(target, encodeWire(w))
}

// broadcastMsg sends a group message: sealed (env) to the members that understand it, the old way
// (legacy) to the others and when there is no envelope.
func (f *groupsFacade) broadcastMsg(groupID string, legacy groupWire, env *senderkeys.Envelope) (delivered, queued, failed int) {
	members, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return 0, 0, 0
	}
	f.mu.Lock()
	f.loadCapable()
	sealed := map[types.PeerID]bool{}
	for _, m := range members {
		sealed[m.PeerID] = env != nil && f.capable[m.PeerID]
	}
	f.mu.Unlock()
	for _, m := range members {
		if m.PeerID == f.selfID {
			continue
		}
		w := legacy
		if sealed[m.PeerID] {
			w = groupWire{Kind: "smsg", Group: groupID, Env: env}
		}
		q, err := f.sendTo(m.PeerID, w)
		switch {
		case err != nil:
			failed++
			f.nudge(m.PeerID)
		case q:
			queued++
			f.nudge(m.PeerID)
		default:
			delivered++
		}
	}
	return
}

// broadcast sends w to every member except the node itself and the given ones.
func (f *groupsFacade) broadcast(groupID string, w groupWire, except ...types.PeerID) (delivered, queued, failed int) {
	members, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return 0, 0, 0
	}
outer:
	for _, m := range members {
		if m.PeerID == f.selfID {
			continue
		}
		for _, x := range except {
			if m.PeerID == x {
				continue outer
			}
		}
		q, err := f.sendTo(m.PeerID, w)
		switch {
		case err != nil:
			failed++
			f.nudge(m.PeerID)
		case q:
			queued++
			f.nudge(m.PeerID)
		default:
			delivered++
		}
	}
	return
}

const nudgePause = 30 * time.Second
const meshLimit = 50 // members of bigger groups are not asked to connect to each other

// nudge asks the node to connect to a member, at most once in a while per member. Writing to a
// member needs their keys, which come with a connection.
func (f *groupsFacade) nudge(id types.PeerID) {
	if f.connect == nil || id == f.selfID {
		return
	}
	f.nudgeMu.Lock()
	if last, ok := f.nudged[id]; ok && time.Since(last) < nudgePause {
		f.nudgeMu.Unlock()
		return
	}
	if len(f.nudged) > 4096 {
		f.nudged = make(map[types.PeerID]time.Time)
	}
	f.nudged[id] = time.Now()
	f.nudgeMu.Unlock()
	go f.connect(id)
}

// nudgeAll asks for connections to every member of a smallish group.
func (f *groupsFacade) nudgeAll(groupID string) {
	members, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil || len(members) > meshLimit {
		return
	}
	for _, m := range members {
		f.nudge(m.PeerID)
	}
}

func (f *groupsFacade) emitEvent(ev GroupEvent) {
	if f.notify != nil {
		f.notify(ev)
	}
}

func (f *groupsFacade) titleOf(groupID string) string {
	if g, err := f.mgr.GetGroup(groupID); err == nil {
		return g.Title
	}
	return ""
}

// handle processes a group message from origin. It reports whether text was one (such messages
// are never shown as chat lines, valid or not).
func (f *groupsFacade) handle(origin types.PeerID, text string) bool {
	raw, ok := strings.CutPrefix(text, wirePrefix)
	if !ok {
		return false
	}
	var w groupWire
	if json.Unmarshal([]byte(raw), &w) != nil || w.Group == "" {
		return true
	}
	var after []func()
	f.mu.Lock()
	if f.load() == nil {
		f.noteVersion(origin, w.V)
		if len(f.heard) > 4096 {
			f.heard = make(map[types.PeerID]time.Time)
		}
		f.heard[origin] = time.Now()
		after = f.apply(origin, w)
	}
	f.mu.Unlock()
	if len(after) > 0 {
		run := func() {
			for _, fn := range after {
				fn()
			}
		}
		if f.async {
			// the caller is a dispatcher worker; sending (which may wait for a peer's key bundle, a
			// request that the same workers must answer) from it could block the whole node
			go run()
		} else {
			run()
		}
	}
	return true
}

// Changes to a group travel separately from each author and can arrive out of order: a member
// who was offline may hear the second owner hand the group on before hearing the first owner
// make them owner. Such a change is not lost: it waits (deferred) and is tried again after every
// change that is applied, until it fits or a week has passed.
const (
	maxDeferred = 64
	deferredFor = 7 * 24 * time.Hour
)

type deferredOp struct {
	origin types.PeerID
	w      groupWire
	at     time.Time
}

// governs tells whether a kind changes who is in the group or what they may do.
func governs(kind string) bool {
	switch kind {
	case "own", "role", "rm", "snap", "join", "add":
		return true
	}
	return false
}

// deferOp keeps a change that does not fit yet, when the reason may go away (the author or the
// target is not known as such yet). Call with f.mu held.
func (f *groupsFacade) deferOp(origin types.PeerID, w groupWire, err error) {
	if !errors.Is(err, groups.ErrInsufficientPermissions) && !errors.Is(err, groups.ErrInsufficientRank) &&
		!errors.Is(err, groups.ErrMemberNotFound) {
		return
	}
	list := f.deferred[w.Group]
	for _, d := range list {
		if d.origin == origin && d.w.Kind == w.Kind && d.w.Target == w.Target && d.w.Role == w.Role && d.w.Ban == w.Ban {
			return
		}
	}
	if len(list) >= maxDeferred {
		list = list[1:]
	}
	f.deferred[w.Group] = append(list, deferredOp{origin, w, time.Now()})
}

// retryDeferred applies the waiting changes that fit now, over and over while some do. Call with
// f.mu held.
func (f *groupsFacade) retryDeferred(groupID string) []func() {
	var after []func()
	f.retrying = true
	defer func() { f.retrying = false }()
	for progress := true; progress; {
		progress = false
		list := f.deferred[groupID]
		delete(f.deferred, groupID)
		for _, d := range list {
			if time.Since(d.at) > deferredFor {
				continue
			}
			if out := f.applyOne(d.origin, d.w); out != nil {
				after = append(after, out...)
				progress = true
			}
		}
	}
	return after
}

// apply changes the local state for one message and returns what is to be done once the lock is
// released (network sends, events).
func (f *groupsFacade) apply(origin types.PeerID, w groupWire) (after []func()) {
	after = f.applyOne(origin, w)
	if after != nil && governs(w.Kind) && !f.retrying && len(f.deferred[w.Group]) > 0 {
		after = append(after, f.retryDeferred(w.Group)...)
	}
	return after
}

func (f *groupsFacade) applyOne(origin types.PeerID, w groupWire) (after []func()) {
	if f.blocked != nil && f.blocked(origin) {
		switch w.Kind {
		case "msg", "call", "callalive", "rcall", "rcallalive":
			return nil // what a blocked member says or starts stays out of sight; the group goes on
		}
	}
	switch w.Kind {
	case "msg":
		// the old way: unsealed inside the sender's session, which is what proves who wrote it
		return f.receiveMessage(origin, w.Group, w.ID, w.TS, w.Text, w.File, nil, "", true)

	case "smsg":
		return f.takeEnvelope(origin, w.Group, w.Env)

	case "envs":
		var after []func()
		for i, v := range w.Envs {
			if i >= maxEnvsInMsg {
				break
			}
			after = append(after, f.takeEnvelope(origin, w.Group, v)...)
		}
		return after

	case "key":
		if !f.acceptEpoch(w.Group, w.Epoch) {
			return nil
		}
		f.admitted(w.Group) // the group's keys came: we are in
		// a newer epoch may have been made for somebody who is gone by now (two admins at once)
		after := f.maybeRotate(w.Group, false)
		return append(after, f.openPending(w.Group)...)

	case "keyreq":
		if _, ok := f.member(w.Group, origin); !ok {
			return nil
		}
		k := "keyans/" + w.Group + "/" + origin.String()
		if last, ok := f.paced[k]; ok && time.Since(last) < syncPause {
			return nil
		}
		f.paced[k] = time.Now()
		return f.sendEpochs(w.Group, origin)

	case "sync":
		return f.answerSync(w.Group, origin, w.Have, w.Epochs)

	case "join":
		pub, ok := parsePub(w.Pub)
		if !ok {
			return nil
		}
		if _, err := f.mgr.GetGroup(w.Group); err != nil {
			return nil
		}
		_, already := f.member(w.Group, origin)
		if !already && f.needsInvite(w.Group) && !f.inviteValid(w.Group, w.Inv) {
			slog.Info("Refused a join without a valid invitation", "group_id", w.Group, "peer", origin.String()[:8])
			return nil
		}
		via := ""
		if w.Inv != nil && f.inviteState != nil {
			if st, ok := f.inviteState(w.Group, w.Inv.N); ok {
				via = st.N
				if st.Approval && !already {
					// the invitation asks for someone to accept the newcomer: a request waits
					return f.noteRequestLocked(w.Group, origin, w.Pub, st)
				}
			}
		}
		return f.admitLocked(w.Group, origin, w.Pub, pub, via)

	case "snap":
		role, err := f.mgr.GetMemberRole(w.Group, origin)
		if err != nil || role.Permissions&groups.PermManageRoles == 0 {
			return nil
		}
		if f.mgr.MergeMembers(w.Group, memberProfiles(w.Members), f.mgr.IsOwner(w.Group, origin)) != nil {
			return nil
		}
		f.noteVersions(w.Members)
		_ = f.persist(w.Group)
		f.admitted(w.Group) // a member who lets members in told us who is in: we are let in
		title := f.titleOf(w.Group)
		after := []func(){func() { f.nudgeAll(w.Group); f.emitEvent(GroupEvent{Kind: "members", GroupID: w.Group, Title: title}) }}
		if f.keyring(w.Group).Current() == nil {
			after = append(after, f.requestKeys(w.Group, origin)...) // a newcomer still without keys
			if f.onJoined != nil && f.logModeLocked(w.Group) {
				hook, g := f.onJoined, w.Group
				after = append(after, func() { hook(g, origin) }) // and without the group's history
			}
		}
		return after

	case "add":
		// an admin added us to a group; only people we know can do that without our consent
		if w.Genesis == nil || w.Genesis.GroupID != w.Group || !f.knows(origin) {
			return nil
		}
		if _, err := f.mgr.GetGroup(w.Group); err == nil {
			return nil // already in
		}
		if f.mgr.ImportGroup(w.Genesis) != nil {
			return nil
		}
		_ = f.mgr.MergeMembers(w.Group, memberProfiles(w.Members), false)
		f.noteVersions(w.Members)
		if _, err := f.mgr.GetMemberRole(w.Group, f.selfID); err != nil {
			_ = f.mgr.SelfJoin(w.Group, f.selfID, f.selfPub)
		}
		_ = f.persist(w.Group)
		title := w.Genesis.Title
		after := []func(){func() {
			f.nudgeAll(w.Group)
			_, _ = f.sendTo(origin, groupWire{Kind: "histreq", Group: w.Group}) // what was said before us
			f.emitEvent(GroupEvent{Kind: "added", GroupID: w.Group, Title: title, From: origin.String()})
		}}
		if f.keyring(w.Group).Current() == nil {
			after = append(after, f.requestKeys(w.Group, origin)...)
		}
		return after

	case "role":
		target, ok := parsePeer(w.Target)
		if !ok {
			return nil
		}
		if err := f.mgr.ApplyRole(w.Group, origin, target, w.Role); err != nil {
			f.deferOp(origin, w, err)
			return nil
		}
		_ = f.persist(w.Group)
		title := f.titleOf(w.Group)
		return []func(){func() {
			f.emitEvent(GroupEvent{Kind: "members", GroupID: w.Group, Title: title, From: origin.String()})
		}}

	case "rm":
		target, ok := parsePeer(w.Target)
		if !ok {
			return nil
		}
		title := f.titleOf(w.Group)
		was, _ := f.member(w.Group, target)
		if err := f.mgr.ApplyRemove(w.Group, origin, target, w.Ban); err != nil {
			f.deferOp(origin, w, err)
			return nil
		}
		if target == f.selfID {
			f.forget(w.Group)
			return []func(){func() {
				f.emitEvent(GroupEvent{Kind: "removed", GroupID: w.Group, Title: title, From: origin.String()})
			}}
		}
		_ = f.persist(w.Group)
		// somebody left or was removed: the epoch they had is replaced by whoever is to do it
		return append(f.maybeRotate(w.Group, false, departed{target.String(), was.JoinedAt}), func() {
			f.emitEvent(GroupEvent{Kind: "members", GroupID: w.Group, Title: title, From: origin.String()})
		})

	case "own":
		target, ok := parsePeer(w.Target)
		if !ok {
			return nil
		}
		if err := f.mgr.ApplyTransfer(w.Group, origin, target); err != nil {
			f.deferOp(origin, w, err)
			return nil
		}
		_ = f.persist(w.Group)
		title := f.titleOf(w.Group)
		return []func(){func() {
			f.emitEvent(GroupEvent{Kind: "members", GroupID: w.Group, Title: title, From: origin.String()})
		}}

	case "del":
		if !f.mgr.IsOwner(w.Group, origin) {
			return nil
		}
		title := f.titleOf(w.Group)
		f.forget(w.Group)
		return []func(){func() {
			f.emitEvent(GroupEvent{Kind: "removed", GroupID: w.Group, Title: title, From: origin.String(), Reason: "deleted"})
		}}

	case "callend":
		if _, err := f.mgr.GetMemberRole(w.Group, origin); err != nil {
			return nil
		}
		if c, ok := f.calls[callKey(w.Group, w.Room)]; ok && c.CallID == w.CallID {
			delete(f.calls, callKey(w.Group, w.Room))
			delete(f.inCall, callKey(w.Group, w.Room))
		}
		ev := GroupEvent{Kind: "callend", GroupID: w.Group, Title: f.titleOf(w.Group), From: origin.String(), CallID: w.CallID, Room: w.Room}
		return []func(){func() { f.emitEvent(ev) }}

	case "histreq":
		// a new member asks for what was said before they came; admins answer, not too often
		if _, err := f.mgr.GetMemberRole(w.Group, origin); err != nil {
			return nil
		}
		if role, err := f.mgr.GetMemberRole(w.Group, f.selfID); err != nil || role.Permissions&groups.PermManageRoles == 0 {
			return nil
		}
		key := w.Group + "/" + origin.String()
		if last, ok := f.histSent[key]; ok && time.Since(last) < historyPause {
			return nil
		}
		f.histSent[key] = time.Now()
		if chunks := f.historyChunks(w.Group); len(chunks) > 0 {
			return []func(){func() { f.sendHistory(origin, w.Group, chunks) }}
		}
		return nil

	case "hist":
		// old messages handed over by an admin; only what is older than our own joining is taken
		role, err := f.mgr.GetMemberRole(w.Group, origin)
		if err != nil || role.Permissions&groups.PermManageRoles == 0 {
			return nil
		}
		return f.takeHistory(w)

	case "callleave":
		// a member left a voice room's call without ending it
		key := callKey(w.Group, w.Room)
		if c, ok := f.calls[key]; ok && c.CallID == w.CallID {
			delete(f.inCall[key], origin)
			ev := GroupEvent{Kind: "callactive", GroupID: w.Group, Title: f.titleOf(w.Group), From: origin.String(), CallID: w.CallID, Coordinator: c.Coordinator, Room: w.Room}
			return []func(){func() { f.emitEvent(ev) }}
		}
		return nil

	case "call", "callalive", "rcall", "rcallalive":
		if (w.Kind == "rcall" || w.Kind == "rcallalive") && w.Room == "" {
			return nil
		}
		role, err := f.mgr.GetMemberRole(w.Group, origin)
		if err != nil || role.Permissions&groups.PermJoinVoice == 0 || w.CallID == "" || w.Coord == "" {
			return nil
		}
		now := time.Now()
		// an announcement held back while the sender was unreachable says nothing about now
		if w.TS != 0 && now.Sub(time.Unix(w.TS, 0)) > callAnnounceStale {
			return nil
		}
		prev, known := f.calls[callKey(w.Group, w.Room)]
		sameCall := known && prev.CallID == w.CallID
		c := GroupCall{Room: w.Room, CallID: w.CallID, Coordinator: w.Coord, From: origin.String(), StartedAt: now.Unix()}
		if sameCall {
			c.From, c.StartedAt = prev.From, prev.StartedAt
		}
		c.LastSeen = now.Unix()
		f.calls[callKey(w.Group, w.Room)] = c
		f.noteInCall(callKey(w.Group, w.Room), origin, sameCall)
		if w.Room != "" {
			// a voice room shows who is in it: every announcement may change that
			ev := GroupEvent{Kind: "callactive", GroupID: w.Group, Title: f.titleOf(w.Group), From: origin.String(), CallID: w.CallID, Coordinator: w.Coord, Room: w.Room}
			return []func(){func() { f.emitEvent(ev) }}
		}
		if sameCall && time.Since(time.Unix(prev.LastSeen, 0)) <= callAliveFor {
			return nil // a call we already know about, still going: nothing new to show
		}
		kind := "callactive" // learned of a call already under way: show it, do not ring
		if w.Kind == "call" && w.Room == "" {
			kind = "call" // somebody just started the group's call: ring (a voice room never rings)
		}
		ev := GroupEvent{Kind: kind, GroupID: w.Group, Title: f.titleOf(w.Group), From: origin.String(), CallID: w.CallID, Coordinator: w.Coord, Room: w.Room}
		return []func(){func() { f.emitEvent(ev) }}
	}
	return nil
}

func role0(f *groupsFacade, groupID string, id types.PeerID) string {
	if r, err := f.mgr.GetMemberRole(groupID, id); err == nil {
		return r.Name
	}
	return "member"
}

// receiveMessage keeps a message of sender (checked already: by its signature sig, or by the
// session it came through) and returns the event to show it. Call with f.mu held.
func (f *groupsFacade) receiveMessage(sender types.PeerID, groupID, id string, ts int64, text string, file *wireFile, sig []byte, fd string, verified bool) []func() {
	role, err := f.mgr.GetMemberRole(groupID, sender)
	if err != nil || role.Permissions&groups.PermSendMessages == 0 || id == "" || text == "" || len(text) > maxGroupText {
		return nil
	}
	if f.blocked != nil && f.blocked(sender) {
		return nil
	}
	if f.store.Exists([]byte(groupSeenPrefix + id)) {
		return nil
	}
	_ = f.store.Set([]byte(groupSeenPrefix+id), []byte{1})
	now := time.Now().UnixMilli()
	if ts <= 0 || ts > now+24*3600*1000 {
		ts = now
	}
	f.seq++
	msg := storedGroupMsg{GroupMessageDTO: GroupMessageDTO{ID: id, GroupID: groupID, Sender: sender.String(), Text: text, Timestamp: ts, Verified: verified}, Sig: sig, FD: fd}
	var offer func()
	if file != nil {
		wf := *file
		if f.onFile == nil || wf.ID == "" || wf.Root == "" || wf.Key == "" || wf.Size < 0 {
			return nil
		}
		msg.File = &GroupFileDTO{ID: wf.ID, Name: wf.Name, Size: wf.Size, Mime: wf.Mime, Thumb: thumbOK(wf.Thumb), TW: wf.TW, TH: wf.TH,
			Channel: wf.Channel, Thread: wf.Thread}
		offer = func() { f.onFile(sender, groupID, wf) }
	}
	raw, _ := json.Marshal(msg)
	if f.store.Set(msgKey(groupID, ts, f.seq), raw) != nil {
		return nil
	}
	ev := GroupEvent{Kind: "message", GroupID: groupID, Title: f.titleOf(groupID), From: sender.String(), MessageID: id}
	mirror := func() {
		if f.onLegacy != nil {
			f.onLegacy(groupID, msg.GroupMessageDTO) // the chat shows it with the log's messages
		}
	}
	if offer != nil {
		// the transfer is known before the message shows, so it can be accepted at once
		return []func(){offer, mirror, func() { f.emitEvent(ev) }}
	}
	return []func(){mirror, func() { f.emitEvent(ev) }}
}

// forget drops a group and what was said in it from this node.
func (f *groupsFacade) forget(groupID string) {
	f.forgetKeys(groupID)
	delete(f.deferred, groupID)
	f.mgr.DropGroup(groupID)
	_ = f.store.Delete([]byte(groupKeyPrefix + groupID))
	if entries, err := f.store.FindValues([]byte(groupMsgPrefix+groupID+"/"), 100000); err == nil {
		for _, e := range entries {
			_ = f.store.Delete(e.Key)
		}
	}
	delete(f.calls, groupID)
	if f.onForget != nil {
		go f.onForget(groupID) // its conversation log goes too
	}
}

func (f *groupsFacade) knows(id types.PeerID) bool {
	return f.isContact != nil && f.isContact(id)
}

// SetRole changes the role of a member (admin, moderator, member, restricted) and tells the group.
func (f *groupsFacade) SetRole(groupID, targetID, role string) error {
	target, ok := parsePeer(targetID)
	if !ok {
		return errors.New("bad member id")
	}
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	if err := f.mgr.ApplyRole(groupID, f.selfID, target, role); err != nil {
		f.mu.Unlock()
		return explain(err)
	}
	was, _ := f.member(groupID, target)
	_ = f.persist(groupID)
	f.mu.Unlock()
	f.broadcast(groupID, groupWire{Kind: "role", Group: groupID, Target: targetID, Role: role})
	f.memberChange(groupID, memberBody(convlog.MemberRole, was.PubKey, role))
	return nil
}

// Remove kicks (or bans) a member. The member is told too, so that the group disappears for them.
func (f *groupsFacade) Remove(groupID, targetID string, ban bool) error {
	target, ok := parsePeer(targetID)
	if !ok || target == f.selfID {
		return errors.New("bad member id")
	}
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	members, _ := f.mgr.GetAuthorizedRecipients(groupID)
	was, _ := f.member(groupID, target)
	if err := f.mgr.ApplyRemove(groupID, f.selfID, target, ban); err != nil {
		f.mu.Unlock()
		return explain(err)
	}
	_ = f.persist(groupID)
	// those who stay get a new epoch: the removed member cannot read what comes next (paced, so
	// kicking somebody who keeps coming back does not mint a key each time)
	rekey := f.maybeRotate(groupID, true, departed{targetID, was.JoinedAt})
	f.mu.Unlock()
	w := groupWire{Kind: "rm", Group: groupID, Target: targetID, Ban: ban}
	for _, m := range members {
		if m.PeerID != f.selfID {
			_, _ = f.sendTo(m.PeerID, w) // includes the removed member
		}
	}
	for _, fn := range rekey {
		fn()
	}
	action := convlog.MemberRemove
	if ban {
		action = convlog.MemberBan
	}
	f.memberChange(groupID, memberBody(action, was.PubKey, ""))
	return nil
}

// Leave tells the group and forgets it. The owner cannot leave.
func (f *groupsFacade) Leave(groupID string) error {
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	role, err := f.mgr.GetMemberRole(groupID, f.selfID)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	if role.Rank >= groups.RoleOwner.Rank {
		f.mu.Unlock()
		return errors.New("the owner cannot leave the group")
	}
	f.mu.Unlock()
	f.memberChange(groupID, memberBody(convlog.MemberRemove, f.selfPub, ""))
	f.broadcast(groupID, groupWire{Kind: "rm", Group: groupID, Target: f.selfID.String()})
	f.mu.Lock()
	f.forget(groupID)
	f.mu.Unlock()
	return nil
}

// StartCall opens a call for the group: everybody who may speak is told, and the caller joins the
// call with themselves as coordinator.
func (f *groupsFacade) StartCall(groupID, room string) (GroupCall, error) {
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return GroupCall{}, err
	}
	role, err := f.mgr.GetMemberRole(groupID, f.selfID)
	if err != nil || role.Permissions&groups.PermJoinVoice == 0 {
		f.mu.Unlock()
		return GroupCall{}, errors.New("you cannot call in this group")
	}
	now := time.Now().Unix()
	c := GroupCall{Room: room, CallID: call.NewInviteSecret(), Coordinator: f.selfID.String(), From: f.selfID.String(), StartedAt: now, LastSeen: now}
	f.calls[callKey(groupID, room)] = c
	f.inCall[callKey(groupID, room)] = map[types.PeerID]int64{f.selfID: now}
	f.mu.Unlock()
	f.broadcast(groupID, groupWire{Kind: callKind(room, "call"), Group: groupID, Room: room, CallID: c.CallID, Coord: c.Coordinator, TS: now})
	return c, nil
}

// KeepCallAlive tells the group that the call is still going. The app calls it every
// callAliveEvery while this member is in the call; announcements closer together are skipped.
func (f *groupsFacade) KeepCallAlive(groupID, room, callID, coordinator string) error {
	if callID == "" || coordinator == "" {
		return errors.New("no call")
	}
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	if _, err := f.mgr.GetMemberRole(groupID, f.selfID); err != nil {
		f.mu.Unlock()
		return err
	}
	now := time.Now().Unix()
	key := callKey(groupID, room)
	c, ok := f.calls[key]
	if ok && c.CallID == callID && f.aliveSent[key] != 0 && now-f.aliveSent[key] < int64(callAliveEvery/time.Second)/2 {
		f.mu.Unlock()
		return nil
	}
	if !ok || c.CallID != callID {
		c = GroupCall{Room: room, CallID: callID, Coordinator: coordinator, From: f.selfID.String(), StartedAt: now}
		delete(f.inCall, key) // another call in this room: its own list of who is in it
	}
	c.LastSeen = now
	f.calls[key] = c
	f.noteInCall(key, f.selfID, true)
	f.aliveSent[key] = now
	f.mu.Unlock()
	f.broadcast(groupID, groupWire{Kind: callKind(room, "callalive"), Group: groupID, Room: room, CallID: callID, Coord: coordinator, TS: now})
	return nil
}

// callKind is the wire kind that announces a call. A voice room's call has kinds of its own: a
// build from before voice rooms does not know the room field and would take "call" for the
// group's own call and ring every member; a kind it does not know it ignores.
func callKind(room, kind string) string {
	if room != "" {
		return "r" + kind
	}
	return kind
}

// ActiveCall is the call that a member recently started in the group, if any.
func (f *groupsFacade) ActiveCall(groupID, room string) (GroupCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.calls[callKey(groupID, room)]
	if !ok || time.Since(time.Unix(c.LastSeen, 0)) > callAliveFor {
		return GroupCall{}, false
	}
	c.Members = f.membersInLocked(callKey(groupID, room))
	return c, true
}

func explain(err error) error {
	switch {
	case errors.Is(err, groups.ErrInsufficientPermissions):
		return errors.New("you are not allowed to do that in this group")
	case errors.Is(err, groups.ErrInsufficientRank):
		return errors.New("you cannot change a member of the same or a higher rank")
	case errors.Is(err, groups.ErrCannotTargetOwner):
		return errors.New("the owner cannot be removed")
	case errors.Is(err, groups.ErrMemberNotFound):
		return errors.New("this person is not in the group")
	}
	return err
}

const groupSeenPrefix = "groups/seen/"

func newGroupMessageID() string { return uuid.NewString() }

// peerOfPubKey is the peer ID of a hex identity key.
func peerOfPubKey(hexKey string) (types.PeerID, types.IdentityPublicKey, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(hexKey))
	var id types.PeerID
	var ident types.IdentityPublicKey
	if err != nil || len(pub) != 32 {
		return id, ident, errors.New("member key must be 32 bytes of hex")
	}
	id = sha256.Sum256(pub)
	copy(ident[:], pub)
	return id, ident, nil
}

// historyChunks packs the most recent messages of a group for a newcomer. Call with f.mu held.
func (f *groupsFacade) historyChunks(groupID string) []groupWire {
	entries, err := f.store.FindValues([]byte(groupMsgPrefix+groupID+"/"), 100000)
	if err != nil || len(entries) == 0 {
		return nil
	}
	if len(entries) > historyLimit {
		entries = entries[len(entries)-historyLimit:]
	}
	var chunks []groupWire
	cur := groupWire{Kind: "hist", Group: groupID}
	size := 0
	for _, e := range entries {
		var m storedGroupMsg
		if json.Unmarshal(e.Value, &m) != nil || m.ID == "" {
			continue
		}
		if len(cur.History) >= historyChunk || size+len(m.Text) > historyChunkSize {
			chunks = append(chunks, cur)
			cur, size = groupWire{Kind: "hist", Group: groupID}, 0
		}
		cur.History = append(cur.History, wireHistory{ID: m.ID, Sender: m.Sender, Text: m.Text, TS: m.Timestamp, Sig: m.Sig, FD: m.FD})
		size += len(m.Text)
	}
	if len(cur.History) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

func (f *groupsFacade) sendHistory(to types.PeerID, groupID string, chunks []groupWire) {
	for _, c := range chunks {
		if _, err := f.sendTo(to, c); err != nil {
			return
		}
	}
}

// takeHistory stores the old messages of a "hist" message. Call with f.mu held.
func (f *groupsFacade) takeHistory(w groupWire) []func() {
	members, err := f.mgr.ListMembers(w.Group)
	if err != nil {
		return nil
	}
	var joined int64
	for _, m := range members {
		if m.PeerID == f.selfID {
			joined = m.JoinedAt
		}
	}
	stored := 0
	for _, h := range w.History {
		if h.ID == "" || h.Text == "" || len(h.Text) > maxGroupText || h.TS <= 0 || h.TS >= joined {
			continue
		}
		sender, ok := parsePeer(h.Sender)
		if !ok {
			continue
		}
		author, ok := f.member(w.Group, sender)
		if !ok {
			continue // somebody who is not in the group (any more): not shown
		}
		// a signed message must still be what its author wrote; an unsigned one (from a member of an
		// older version) is taken on the admin's word and shown as unverified
		verified := false
		if len(h.Sig) > 0 {
			c := senderkeys.Content{Group: w.Group, ID: h.ID, TS: h.TS, Sender: h.Sender, Text: h.Text, FileDigest: h.FD, Sig: h.Sig}
			if c.Verify(author.PubKey[:]) != nil {
				continue
			}
			verified = true
		}
		if f.store.Exists([]byte(groupSeenPrefix + h.ID)) {
			continue
		}
		_ = f.store.Set([]byte(groupSeenPrefix+h.ID), []byte{1})
		f.seq++
		msg := storedGroupMsg{GroupMessageDTO: GroupMessageDTO{ID: h.ID, GroupID: w.Group, Sender: sender.String(), Text: h.Text, Timestamp: h.TS,
			IsMine: sender == f.selfID, Verified: verified}, Sig: h.Sig, FD: h.FD}
		raw, _ := json.Marshal(msg)
		if f.store.Set(msgKey(w.Group, h.TS, f.seq), raw) == nil {
			stored++
		}
	}
	if stored == 0 {
		return nil
	}
	ev := GroupEvent{Kind: "history", GroupID: w.Group, Title: f.titleOf(w.Group)}
	return []func(){func() { f.emitEvent(ev) }}
}

// TransferOwnership hands the group to another member; the owner becomes an admin.
func (f *groupsFacade) TransferOwnership(groupID, targetID string) error {
	target, ok := parsePeer(targetID)
	if !ok || target == f.selfID {
		return errors.New("bad member id")
	}
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	if err := f.mgr.ApplyTransfer(groupID, f.selfID, target); err != nil {
		f.mu.Unlock()
		return explain(err)
	}
	was, _ := f.member(groupID, target)
	_ = f.persist(groupID)
	f.mu.Unlock()
	f.broadcast(groupID, groupWire{Kind: "own", Group: groupID, Target: targetID})
	f.memberChange(groupID, memberBody(convlog.MemberRole, was.PubKey, groups.RoleOwner.Name))
	return nil
}

// Delete removes the group for everybody; only the owner may.
func (f *groupsFacade) Delete(groupID string) error {
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	if !f.mgr.IsOwner(groupID, f.selfID) {
		f.mu.Unlock()
		return errors.New("only the owner can delete the group")
	}
	f.mu.Unlock()
	f.broadcast(groupID, groupWire{Kind: "del", Group: groupID})
	f.mu.Lock()
	f.forget(groupID)
	f.mu.Unlock()
	return nil
}

// EndCall tells the group that the call is over (the last person left); it stops ringing for
// those who have not answered and disappears from "join the call".
func (f *groupsFacade) EndCall(groupID, room, callID string) error {
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	if _, err := f.mgr.GetMemberRole(groupID, f.selfID); err != nil {
		f.mu.Unlock()
		return err
	}
	if c, ok := f.calls[callKey(groupID, room)]; ok && (callID == "" || c.CallID == callID) {
		callID = c.CallID
		delete(f.calls, callKey(groupID, room))
		delete(f.inCall, callKey(groupID, room))
	}
	f.mu.Unlock()
	if callID == "" {
		return errors.New("no call to end")
	}
	f.broadcast(groupID, groupWire{Kind: "callend", Group: groupID, Room: room, CallID: callID})
	return nil
}

// callKey names a call among a group's: its own call (room "") or a voice room's.
func callKey(groupID, room string) string {
	if room == "" {
		return groupID
	}
	return groupID + "\x00" + room
}

// ActiveRoomCalls are the calls going on in a group's voice rooms, by room.
func (f *groupsFacade) ActiveRoomCalls(groupID string) map[string]GroupCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]GroupCall{}
	for k, c := range f.calls {
		if c.Room != "" && k == callKey(groupID, c.Room) && time.Since(time.Unix(c.LastSeen, 0)) <= callAliveFor {
			c.Members = f.membersInLocked(k)
			out[c.Room] = c
		}
	}
	return out
}

// noteInCall records that peer is in the call key; a new call (fresh false) starts its list anew.
// Call with f.mu held.
func (f *groupsFacade) noteInCall(key string, peer types.PeerID, fresh bool) {
	if !fresh || f.inCall[key] == nil {
		f.inCall[key] = map[types.PeerID]int64{}
	}
	f.inCall[key][peer] = time.Now().Unix()
}

// membersInLocked are who announced being in the call key lately, in a stable order.
func (f *groupsFacade) membersInLocked(key string) []string {
	var out []string
	for id, seen := range f.inCall[key] {
		if time.Since(time.Unix(seen, 0)) <= callAliveFor {
			out = append(out, id.String())
		}
	}
	sort.Strings(out)
	return out
}

// LeaveCall says we left a voice room's call that others stay in.
func (f *groupsFacade) LeaveCall(groupID, room, callID string) {
	f.mu.Lock()
	key := callKey(groupID, room)
	if m := f.inCall[key]; m != nil {
		delete(m, f.selfID)
	}
	f.mu.Unlock()
	f.broadcast(groupID, groupWire{Kind: "callleave", Group: groupID, Room: room, CallID: callID})
}

// removedOnLog applies a removal or a ban that came by the group's log, as an "rm" message from
// its author would: the group service checks the author may do it, the member's epoch is
// replaced, and a member removed in our absence no longer gets keys or is let back in by us.
func (f *groupsFacade) removedOnLog(groupID string, author types.PeerID, memberPub []byte, ban bool) {
	var pub types.PeerPublicKey
	copy(pub[:], memberPub)
	target := types.PeerPubKeyToID(pub)
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return
	}
	was, in := f.member(groupID, target)
	if !in {
		f.mu.Unlock()
		return // the "rm" came first, or they were never on our list
	}
	title := f.titleOf(groupID)
	if err := f.mgr.ApplyRemove(groupID, author, target, ban); err != nil {
		f.mu.Unlock()
		return
	}
	var after []func()
	if target == f.selfID {
		f.forget(groupID)
		after = append(after, func() {
			f.emitEvent(GroupEvent{Kind: "removed", GroupID: groupID, Title: title, From: author.String()})
		})
	} else {
		_ = f.persist(groupID)
		after = append(f.maybeRotate(groupID, false, departed{target.String(), was.JoinedAt}), func() {
			f.emitEvent(GroupEvent{Kind: "members", GroupID: groupID, Title: title, From: author.String()})
		})
	}
	f.mu.Unlock()
	for _, fn := range after {
		fn()
	}
}

// admitLocked lets a newcomer in (who came with the invitation via, "" none known): into the
// group's list here, and, from whoever may let members in, onto the log, with the member list,
// the keys and the history. Call with f.mu held; run what it returns after.
func (f *groupsFacade) admitLocked(groupID string, origin types.PeerID, pubHex string, pub types.IdentityPublicKey, via string) (after []func()) {
	if err := f.mgr.SelfJoin(groupID, origin, pub); err != nil {
		return nil
	}
	_ = f.persist(groupID)
	f.dropRequestLocked(groupID, origin)
	title := f.titleOf(groupID)
	after = append(after, func() {
		f.emitEvent(GroupEvent{Kind: "members", GroupID: groupID, Title: title, From: origin.String()})
	})
	// whoever may manage members tells the newcomer who is in the group: once a minute per
	// newcomer, so leaving and joining in a loop does not flood the group (keys always go)
	k := "join/" + groupID + "/" + origin.String()
	last, answered := f.paced[k]
	fresh := !answered || f.clock().Sub(last) >= joinPause
	role, err := f.mgr.GetMemberRole(groupID, f.selfID)
	if err != nil || role.Permissions&groups.PermManageRoles == 0 {
		return after
	}
	if !fresh {
		after = append(after, f.maybeRotate(groupID, false)...)
		return append(after, f.sendEpochs(groupID, origin)...)
	}
	f.paced[k] = f.clock()
	// the newcomer's membership on the log, written by whoever may let members in
	join := memberBody(convlog.MemberJoin, pub, "")
	if via != "" && f.inviteState != nil {
		if st, ok := f.inviteState(groupID, via); ok {
			join.Via = st.N // one of the invitation's uses, counted on the log
			if st.Role != "" {
				// the role the invitation gives, from whoever lets the newcomer in
				target, role := origin.String(), st.Role
				after = append(after, func() { _ = f.SetRole(groupID, target, role) })
			}
		}
	}
	after = append(after, func() { f.memberChange(groupID, join) })
	snap := groupWire{Kind: "snap", Group: groupID, Members: f.wireMembers(groupID)}
	newcomer := groupWire{Kind: "snap", Group: groupID, Members: []wireMember{{ID: origin.String(), Pub: pubHex, Role: role0(f, groupID, origin)}}}
	after = append(after, func() { _, _ = f.sendTo(origin, snap) })
	// the other members only hear of the newcomer from us: they wrote to who they knew
	after = append(after, func() { f.broadcast(groupID, newcomer, origin) })
	// and the newcomer gets the keys (an older group gets its first epoch from its owner)
	after = append(after, f.maybeRotate(groupID, false)...)
	after = append(after, f.sendEpochs(groupID, origin)...)
	if chunks := f.historyChunks(groupID); len(chunks) > 0 {
		after = append(after, func() { f.sendHistory(origin, groupID, chunks) })
	}
	// a group on the log hands its history over by syncing the log with the newcomer
	if f.onJoined != nil && f.logModeLocked(groupID) {
		hook := f.onJoined
		after = append(after, func() { hook(groupID, origin) })
	}
	return after
}
