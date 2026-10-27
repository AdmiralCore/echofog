package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Group messages are sealed once for the whole group (senderkeys) and signed by their author. The
// envelope still travels inside every member's own encrypted session, so nothing is weaker than
// before; what it adds is that the same bytes go to everybody, so a member who has a message can
// hand it to one who missed it (sync), and that the author's signature can be checked wherever a
// message comes from, history included.
//
// Epochs are made by admins: the group's creator makes the first, and whoever removes a member
// makes the next one for those who stay. A member who leaves cannot do that, so the admin who made
// the epoch in use, or the owner, replaces an epoch as soon as one of the members it was made for
// is gone; two admins doing it at once is fine, everybody settles on the same one (EpochID.After).
// Members of an older version get the message the old way, unsealed inside their session.

const (
	// wireVersion is the version of the group messages this node sends: 2 = sealed messages, 3 =
	// the conversation log (events sealed the same way). A member is sealedVersion-capable when it
	// speaks 2 or more.
	wireVersion     = 3
	sealedVersion   = 2
	groupKeysPrefix = "groups/k/"
	groupEnvPrefix  = "groups/e/"
	groupCapKey     = "groups/k/_cap"  // peers known to understand version 2
	groupLogCapKey  = "groups/k/_cap3" // peers known to speak the conversation log (version 3)

	envKeepFor  = 7 * 24 * time.Hour // envelopes kept for members who missed them
	envKeepMax  = 3000               // per group
	syncBatch   = 48 * 1024          // bytes of envelopes per message
	syncMaxEnvs = 300                // envelopes per answer
	syncPause   = 10 * time.Second   // between requests (and answers) per group and peer
	keyReqPause = 30 * time.Second
	syncEvery   = 10 * time.Minute
	syncFanout  = 3                // members asked by a periodic sync
	heardRecent = 15 * time.Minute // a member heard from this recently is likely online

	// Replacing the epoch is paced, so somebody leaving and joining over and over (or being
	// kicked and let back in) cannot make the admins mint key after key: after an epoch, the next
	// waits rekeyGap, and departures in between are covered by one new epoch. A departed member
	// who is back by then needs no new epoch at all. With more than rekeyBurst epochs in the last
	// hour the gap doubles, up to rekeyGapMax.
	rekeyGap    = 2 * time.Minute
	rekeyGapMax = 30 * time.Minute
	rekeyBurst  = 3
	joinPause   = time.Minute // between answers (members, history) to the same newcomer
)

var (
	syncFirstDelay = 20 * time.Second // tests shorten it
	maxEnvsInMsg   = 64
)

// cachedEnv is an envelope this node holds: to hand on, or (Pending) to open once its epoch comes.
type cachedEnv struct {
	Env     *senderkeys.Envelope `json:"env"`
	From    string               `json:"from"`
	At      int64                `json:"at"`
	Pending bool                 `json:"p,omitempty"`
}

func envKey(groupID string, v *senderkeys.Envelope) []byte {
	// the sender's ID would be readable in a database key; a hash of where the envelope sits is not
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%d", v.Epoch, v.Tag, v.Sender, v.N)))
	return []byte(groupEnvPrefix + groupID + "/" + hex.EncodeToString(sum[:16]))
}

// keyring returns the group's keyring, loading it on first use. Call with f.mu held.
func (f *groupsFacade) keyring(groupID string) *senderkeys.Keyring {
	if kr, ok := f.keyrings[groupID]; ok {
		return kr
	}
	kr := senderkeys.NewKeyring(groupID, f.selfID.String())
	if raw, err := f.store.Get([]byte(groupKeysPrefix + groupID)); err == nil && len(raw) > 0 {
		var saved senderkeys.Keyring
		if json.Unmarshal(raw, &saved) == nil && saved.Group == groupID {
			saved.Self = f.selfID.String()
			kr = &saved
		}
	}
	f.keyrings[groupID] = kr
	return kr
}

// saveKeyring writes the keyring; a sealed envelope must not leave before its chain is saved.
func (f *groupsFacade) saveKeyring(groupID string) error {
	kr, ok := f.keyrings[groupID]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(kr)
	if err != nil {
		return err
	}
	return f.store.Set([]byte(groupKeysPrefix+groupID), raw)
}

// loadCapable reads which peers understand sealed messages. Call with f.mu held.
func (f *groupsFacade) loadCapable() {
	if f.capLoaded {
		return
	}
	f.capLoaded = true
	if f.logCapable == nil {
		f.logCapable = map[types.PeerID]bool{}
	}
	for key, set := range map[string]map[types.PeerID]bool{groupCapKey: f.capable, groupLogCapKey: f.logCapable} {
		raw, err := f.store.Get([]byte(key))
		if err != nil {
			continue
		}
		var ids []string
		if json.Unmarshal(raw, &ids) == nil {
			for _, s := range ids {
				if id, ok := parsePeer(s); ok {
					set[id] = true
				}
			}
		}
	}
}

// noteVersion remembers that peer speaks version v. Call with f.mu held.
func (f *groupsFacade) noteVersion(peer types.PeerID, v int) {
	if v < sealedVersion {
		return
	}
	f.loadCapable()
	if v >= wireVersion && !f.logCapable[peer] {
		f.logCapable[peer] = true
		saveIDs(f.store, groupLogCapKey, f.logCapable)
	}
	if f.capable[peer] {
		return
	}
	f.capable[peer] = true
	saveIDs(f.store, groupCapKey, f.capable)
}

func saveIDs(st storage.Storage, key string, set map[types.PeerID]bool) {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id.String())
	}
	raw, _ := json.Marshal(ids)
	_ = st.Set([]byte(key), raw)
}

// memberVersion is the version we know a member speaks, as told to others in member lists. Call
// with f.mu held.
func (f *groupsFacade) memberVersion(id types.PeerID) int {
	f.loadCapable()
	switch {
	case id == f.selfID || f.logCapable[id]:
		return wireVersion
	case f.capable[id]:
		return sealedVersion
	}
	return 0
}

// member returns a current (not banned) member. Call with f.mu held.
func (f *groupsFacade) member(groupID string, id types.PeerID) (groups.MemberProfile, bool) {
	list, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return groups.MemberProfile{}, false
	}
	for _, m := range list {
		if m.PeerID == id {
			return m, true
		}
	}
	return groups.MemberProfile{}, false
}

func (f *groupsFacade) canManage(groupID string, id types.PeerID) bool {
	m, ok := f.member(groupID, id)
	return ok && m.Role.Permissions&groups.PermManageRoles != 0
}

// newEpoch makes the next epoch for the members there are now and keeps it. Call with f.mu held.
func (f *groupsFacade) newEpoch(groupID string) (*senderkeys.Epoch, error) {
	if f.sign == nil {
		return nil, errors.New("no identity to sign the epoch")
	}
	list, err := f.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.PeerID.String())
	}
	kr := f.keyring(groupID)
	n := uint64(1)
	if cur := kr.Current(); cur != nil {
		n = cur.N + 1
	}
	e, err := senderkeys.NewEpoch(groupID, n, f.selfID.String(), ids, f.clock(), f.sign)
	if err != nil {
		return nil, err
	}
	kr.Add(e)
	kr.Prune(f.clock())
	if err := f.saveKeyring(groupID); err != nil {
		return nil, err
	}
	return e, nil
}

// rotate makes a new epoch and returns the sends that hand it to every member. Call with f.mu held.
func (f *groupsFacade) rotate(groupID string) []func() {
	e, err := f.newEpoch(groupID)
	if err != nil {
		return nil
	}
	w := groupWire{Kind: "key", Group: groupID, Epoch: e}
	return []func(){func() { f.broadcast(groupID, w) }}
}

// departed is a member who just left or was removed, with when we learned they were a member.
type departed struct {
	id       string
	joinedAt int64 // unix ms
}

// clock is the time the key logic goes by (tests set f.now).
func (f *groupsFacade) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// pendingRekey is a replacement of the epoch that waits for the gap since the last one.
type pendingRekey struct {
	due   time.Time
	gone  []departed
	timer *time.Timer
}

// rekeyGapFor is how long after the last epoch the next may come: rekeyGap, doubled for every
// epoch past rekeyBurst made in the last hour.
func (f *groupsFacade) rekeyGapFor(groupID string) time.Duration {
	hour := f.clock().Add(-time.Hour).UnixMilli()
	n := 0
	for _, e := range f.keyring(groupID).List() {
		if e.CreatedAt > hour {
			n++
		}
	}
	gap := rekeyGap
	for i := rekeyBurst; i < n && gap < rekeyGapMax; i++ {
		gap *= 2
	}
	return min(gap, rekeyGapMax)
}

// scheduleRekey replaces the epoch now if the gap since the last one has passed, or remembers
// to do it when it has (departures meanwhile join the same replacement). Call with f.mu held.
func (f *groupsFacade) scheduleRekey(groupID string, gone []departed) []func() {
	p := f.rekeys[groupID]
	if p == nil {
		p = &pendingRekey{}
	}
	p.gone = append(p.gone, gone...)
	due := f.clock()
	if cur := f.keyring(groupID).Current(); cur != nil {
		if at := time.UnixMilli(cur.CreatedAt).Add(f.rekeyGapFor(groupID)); at.After(due) {
			due = at
		}
	}
	if !due.After(f.clock()) {
		delete(f.rekeys, groupID)
		if p.timer != nil {
			p.timer.Stop()
		}
		return f.rekeyNow(groupID, p.gone)
	}
	p.due = due
	f.rekeys[groupID] = p
	if f.async && p.timer == nil {
		p.timer = time.AfterFunc(due.Sub(f.clock()), f.runDueRekeys)
	}
	return nil
}

// rekeyNow makes the new epoch unless nothing calls for it any more: every departed member is
// back, no member the epoch was made for is gone, and it is not too old. Call with f.mu held.
func (f *groupsFacade) rekeyNow(groupID string, gone []departed) []func() {
	still := gone[:0:0]
	for _, d := range gone {
		id, ok := parsePeer(d.id)
		if !ok {
			continue
		}
		if _, back := f.member(groupID, id); !back {
			still = append(still, d)
		}
	}
	if len(still) == 0 && !f.tainted(groupID) && !f.keyring(groupID).Stale(f.clock()) {
		return nil
	}
	return f.rotate(groupID)
}

// runDueRekeys makes the replacements whose time has come and sends them out. The node's timers
// call it; so does SyncGroups, for a timer lost to a restart.
func (f *groupsFacade) runDueRekeys() {
	var after []func()
	f.mu.Lock()
	for g, p := range f.rekeys {
		if p.due.After(f.clock()) {
			continue
		}
		delete(f.rekeys, g)
		after = append(after, f.rekeyNow(g, p.gone)...)
	}
	f.mu.Unlock()
	for _, fn := range after {
		fn()
	}
}

// tainted tells whether the epoch in use may be held by somebody who is no longer a member: it
// was made for them, or for somebody else gone since, or we learned of them as a member after we
// got the epoch (then it may have been handed to them). Both times of that last check are this
// node's own clock, so a creator whose clock runs ahead cannot hide a holder.
func (f *groupsFacade) tainted(groupID string, gone ...departed) bool {
	kr := f.keyring(groupID)
	cur := kr.Current()
	if cur == nil {
		return false
	}
	for _, d := range gone {
		if cur.Has(d.id) || d.joinedAt >= kr.LearnedAt(cur) {
			return true
		}
	}
	for _, s := range cur.Members {
		id, ok := parsePeer(s)
		if !ok {
			continue
		}
		if _, ok := f.member(groupID, id); !ok {
			return true
		}
	}
	return false
}

// maybeRotate replaces the epoch in use when it must go and this node is the one to do it: the
// admin who removed somebody (actor), or the admin who made it, or the owner, once a member it
// was made for is gone or it is older than senderkeys.MaxEpochAge. Call with f.mu held.
func (f *groupsFacade) maybeRotate(groupID string, actor bool, gone ...departed) []func() {
	if !f.canManage(groupID, f.selfID) {
		return nil
	}
	kr := f.keyring(groupID)
	cur := kr.Current()
	if cur == nil {
		if !f.mgr.IsOwner(groupID, f.selfID) {
			return nil // the owner starts the first epoch of an older group
		}
		return f.rotate(groupID)
	}
	responsible := actor || f.mgr.IsOwner(groupID, f.selfID) || cur.Creator == f.selfID.String()
	if !responsible || !(actor || f.tainted(groupID, gone...) || kr.Stale(f.clock())) {
		return nil
	}
	return f.scheduleRekey(groupID, gone)
}

// acceptEpoch keeps an epoch handed over by origin when its creator is an admin of the group and
// signed it. Reports whether it was new. Call with f.mu held.
func (f *groupsFacade) acceptEpoch(groupID string, e *senderkeys.Epoch) bool {
	if e == nil || e.Group != groupID {
		return false
	}
	creator, ok := parsePeer(e.Creator)
	if !ok {
		return false
	}
	m, ok := f.member(groupID, creator)
	if !ok || m.Role.Permissions&groups.PermManageRoles == 0 {
		return false
	}
	if e.Verify(ed25519.PublicKey(m.PubKey[:])) != nil {
		return false
	}
	kr := f.keyring(groupID)
	if !kr.Add(e) {
		return false
	}
	kr.Prune(f.clock())
	_ = f.saveKeyring(groupID)
	return true
}

// sendEpochs hands the epochs we hold (newest first) to a member who needs them.
func (f *groupsFacade) sendEpochs(groupID string, to types.PeerID) []func() {
	list := f.keyring(groupID).List()
	if len(list) == 0 {
		return nil
	}
	var ws []groupWire
	for _, e := range list {
		ws = append(ws, groupWire{Kind: "key", Group: groupID, Epoch: e})
	}
	return []func(){func() {
		for _, w := range ws {
			if _, err := f.sendTo(to, w); err != nil {
				return
			}
		}
	}}
}

// requestKeys asks peer for the group's epochs, at most every keyReqPause. Call with f.mu held.
func (f *groupsFacade) requestKeys(groupID string, peer types.PeerID) []func() {
	if peer == f.selfID {
		return nil
	}
	k := "key/" + groupID + "/" + peer.String()
	if last, ok := f.paced[k]; ok && f.clock().Sub(last) < keyReqPause {
		return nil
	}
	f.paced[k] = f.clock()
	w := groupWire{Kind: "keyreq", Group: groupID}
	return []func(){func() { _, _ = f.sendTo(peer, w) }}
}

// requestSync asks peer for the messages we are missing, at most every syncPause. Call with f.mu held.
func (f *groupsFacade) requestSync(groupID string, peer types.PeerID) []func() {
	if peer == f.selfID {
		return nil
	}
	k := "sync/" + groupID + "/" + peer.String()
	if last, ok := f.paced[k]; ok && f.clock().Sub(last) < syncPause {
		return nil
	}
	f.paced[k] = f.clock()
	kr := f.keyring(groupID)
	var ids []senderkeys.EpochID
	for _, e := range kr.List() {
		ids = append(ids, e.ID())
	}
	if len(ids) == 0 {
		return f.requestKeys(groupID, peer)
	}
	w := groupWire{Kind: "sync", Group: groupID, Have: kr.Have(), Epochs: ids}
	return []func(){func() { _, _ = f.sendTo(peer, w) }}
}

// seal seals a signed message in the group's current epoch, making or replacing the epoch first
// when this node is the one to. Returns nil when the group has no epoch (members get the message
// the old way). Call with f.mu held; the returned sends hand a new epoch out, before the message.
func (f *groupsFacade) seal(groupID string, c *senderkeys.Content) (*senderkeys.Envelope, []func()) {
	before := f.maybeRotate(groupID, false)
	if f.tainted(groupID) {
		// somebody no longer a member holds the epoch in use and the next one is not made yet
		// (rekeys are paced): nothing new is sealed with it, members get it in their own sessions
		return nil, before
	}
	kr := f.keyring(groupID)
	if kr.Current() == nil {
		return nil, before
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, before
	}
	v, err := kr.Seal(plain)
	if err != nil || f.saveKeyring(groupID) != nil {
		return nil, before
	}
	f.cacheEnv(groupID, v, f.selfID, false)
	return v, before
}

// cacheEnv keeps an envelope for members who missed it. Call with f.mu held.
func (f *groupsFacade) cacheEnv(groupID string, v *senderkeys.Envelope, from types.PeerID, pending bool) {
	raw, err := json.Marshal(cachedEnv{Env: v, From: from.String(), At: f.clock().UnixMilli(), Pending: pending})
	if err != nil {
		return
	}
	_ = f.store.Set(envKey(groupID, v), raw)
	f.envWrites++
	if f.envWrites%100 == 0 {
		f.pruneEnvs(groupID)
	}
}

func (f *groupsFacade) cachedEnvs(groupID string) []cachedEnv {
	entries, err := f.store.FindValues([]byte(groupEnvPrefix+groupID+"/"), 0)
	if err != nil {
		return nil
	}
	out := make([]cachedEnv, 0, len(entries))
	for _, e := range entries {
		var c cachedEnv
		if json.Unmarshal(e.Value, &c) == nil && c.Env != nil {
			out = append(out, c)
		}
	}
	return out
}

// pruneEnvs drops envelopes older than envKeepFor, and the oldest past envKeepMax.
func (f *groupsFacade) pruneEnvs(groupID string) {
	entries, err := f.store.FindValues([]byte(groupEnvPrefix+groupID+"/"), 0)
	if err != nil {
		return
	}
	cut := f.clock().Add(-envKeepFor).UnixMilli()
	type aged struct {
		key []byte
		at  int64
	}
	var keep []aged
	for _, e := range entries {
		var c cachedEnv
		if json.Unmarshal(e.Value, &c) != nil || c.At < cut {
			_ = f.store.Delete(e.Key)
			continue
		}
		keep = append(keep, aged{e.Key, c.At})
	}
	if len(keep) <= envKeepMax {
		return
	}
	// oldest first
	for i := 1; i < len(keep); i++ {
		for j := i; j > 0 && keep[j].at < keep[j-1].at; j-- {
			keep[j], keep[j-1] = keep[j-1], keep[j]
		}
	}
	for _, a := range keep[:len(keep)-envKeepMax] {
		_ = f.store.Delete(a.key)
	}
}

// takeEnvelope opens an envelope that origin handed us (its author or another member), checks
// the author's signature and keeps the message. Call with f.mu held.
func (f *groupsFacade) takeEnvelope(origin types.PeerID, groupID string, v *senderkeys.Envelope) []func() {
	if v == nil || v.Group != groupID {
		return nil
	}
	if _, err := f.mgr.GetGroup(groupID); err != nil {
		return nil
	}
	sender, ok := parsePeer(v.Sender)
	if !ok {
		return nil
	}
	if _, ok := f.member(groupID, origin); !ok && origin != f.selfID {
		return nil // only members hand messages on
	}
	author, ok := f.member(groupID, sender)
	if !ok || author.Role.Permissions&groups.PermSendMessages == 0 {
		return nil
	}
	key := envKey(groupID, v)
	if raw, err := f.store.Get(key); err == nil && len(raw) > 0 {
		var c cachedEnv
		if json.Unmarshal(raw, &c) == nil && !c.Pending {
			return nil // had it already
		}
	}
	kr := f.keyring(groupID)
	plain, err := kr.Open(v)
	if errors.Is(err, senderkeys.ErrUnknownEpoch) {
		// its epoch has not reached us yet: keep it, and ask for the epoch
		f.cacheEnv(groupID, v, origin, true)
		after := f.requestKeys(groupID, origin)
		if sender != origin {
			after = append(after, f.requestKeys(groupID, sender)...)
		}
		return after
	}
	if err != nil {
		return nil
	}
	if bytes.HasPrefix(plain, cevMagic) {
		return f.takeEvent(origin, groupID, sender, v, plain[len(cevMagic):])
	}
	var c senderkeys.Content
	if json.Unmarshal(plain, &c) != nil || c.Group != groupID || c.Sender != v.Sender ||
		c.Verify(ed25519.PublicKey(author.PubKey[:])) != nil {
		return nil
	}
	_ = f.saveKeyring(groupID)
	f.cacheEnv(groupID, v, origin, false)

	var file *wireFile
	if len(c.Extra) > 0 {
		var wf wireFile
		if json.Unmarshal(c.Extra, &wf) != nil {
			return nil
		}
		file = &wf
	}
	after := f.receiveMessage(sender, groupID, c.ID, c.TS, c.Text, file, c.Sig, c.FileDigest, true)
	// a gap in the author's chain: somebody has the messages in between
	for _, st := range kr.Have() {
		if st.Sender == v.Sender && len(st.Missing) > 0 {
			after = append(after, f.requestSync(groupID, origin)...)
			break
		}
	}
	return after
}

// openPending tries the envelopes that waited for an epoch. Call with f.mu held.
func (f *groupsFacade) openPending(groupID string) []func() {
	var after []func()
	for _, c := range f.cachedEnvs(groupID) {
		if !c.Pending || f.keyring(groupID).Epoch(c.Env.EpochID()) == nil {
			continue
		}
		from, ok := parsePeer(c.From)
		if !ok {
			continue
		}
		after = append(after, f.takeEnvelope(from, groupID, c.Env)...)
	}
	return after
}

// answerSync hands requester the envelopes it said it lacks. Call with f.mu held.
func (f *groupsFacade) answerSync(groupID string, requester types.PeerID, have []senderkeys.ChainState, epochs []senderkeys.EpochID) []func() {
	if _, ok := f.member(groupID, requester); !ok || requester == f.selfID {
		return nil
	}
	k := "answer/" + groupID + "/" + requester.String()
	if last, ok := f.paced[k]; ok && f.clock().Sub(last) < syncPause {
		return nil
	}
	f.paced[k] = f.clock()
	var envs []*senderkeys.Envelope
	for _, c := range f.cachedEnvs(groupID) {
		if c.Pending || c.Env.Sender == requester.String() {
			continue
		}
		if senderkeys.Wants(have, epochs, c.Env.EpochID(), c.Env.Sender, c.Env.N) {
			envs = append(envs, c.Env)
			if len(envs) >= syncMaxEnvs {
				break
			}
		}
	}
	var after []func()
	// a requester that lacks an epoch we hold gets it too
	held := map[senderkeys.EpochID]bool{}
	for _, id := range epochs {
		held[id] = true
	}
	for _, e := range f.keyring(groupID).List() {
		if !held[e.ID()] {
			w := groupWire{Kind: "key", Group: groupID, Epoch: e}
			after = append(after, func() { _, _ = f.sendTo(requester, w) })
		}
	}
	if len(envs) == 0 {
		return after
	}
	batches := batchEnvs(envs)
	return append(after, func() {
		for _, b := range batches {
			if _, err := f.sendTo(requester, groupWire{Kind: "envs", Group: groupID, Envs: b}); err != nil {
				return
			}
		}
	})
}

func batchEnvs(envs []*senderkeys.Envelope) [][]*senderkeys.Envelope {
	var out [][]*senderkeys.Envelope
	var cur []*senderkeys.Envelope
	size := 0
	for _, v := range envs {
		n := len(v.CT) + len(v.Nonce) + 200
		if len(cur) > 0 && (size+n > syncBatch || len(cur) >= maxEnvsInMsg) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, v)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// SyncGroups asks a few members of every group for what this node missed (and hands out a new
// epoch where this node is the one to). The node calls it at start and every syncEvery.
func (f *groupsFacade) SyncGroups() {
	var after []func()
	f.mu.Lock()
	if f.load() != nil {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	f.runDueRekeys()
	f.mu.Lock()
	for _, s := range f.mgr.ListGroups() {
		g := s.Genesis.GroupID
		after = append(after, f.maybeRotate(g, false)...)
		list, err := f.mgr.GetAuthorizedRecipients(g)
		if err != nil {
			continue
		}
		var others []types.PeerID
		for _, m := range list {
			if m.PeerID != f.selfID {
				others = append(others, m.PeerID)
			}
		}
		// ask members we heard from lately first: they are likely online; a request to somebody
		// offline only waits in the outbox
		rand.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
		now := f.clock()
		recent := func(p types.PeerID) bool { t, ok := f.heard[p]; return ok && now.Sub(t) < heardRecent }
		sort.SliceStable(others, func(i, j int) bool { return recent(others[i]) && !recent(others[j]) })
		if len(others) > syncFanout {
			others = others[:syncFanout]
		}
		for _, p := range others {
			after = append(after, f.requestSync(g, p)...)
		}
	}
	f.mu.Unlock()
	for _, fn := range after {
		fn()
	}
}

// forgetKeys drops what this node held for a group it is no longer in. Call with f.mu held.
func (f *groupsFacade) forgetKeys(groupID string) {
	delete(f.keyrings, groupID)
	_ = f.store.Delete([]byte(groupKeysPrefix + groupID))
	if entries, err := f.store.FindValues([]byte(groupEnvPrefix+groupID+"/"), 0); err == nil {
		for _, e := range entries {
			_ = f.store.Delete(e.Key)
		}
	}
}

// syncLoop runs SyncGroups shortly after start and then every syncEvery, until ctx ends.
func (f *groupsFacade) syncLoop(ctx context.Context) {
	t := time.NewTimer(syncFirstDelay) // let the node find its peers first
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.SyncGroups()
			t.Reset(syncEvery)
		}
	}
}
