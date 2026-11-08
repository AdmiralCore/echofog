package app

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	// ErrGroupOff reports a group call on an app built without a group book.
	ErrGroupOff = errors.New("app: groups are not available")
	// ErrInvite is the one refusal of an invitation: a bad link, a forged one, one that ran out, was
	// revoked or never existed, the group's ban, all look the same (AD20).
	ErrInvite = errors.New("app: the invitation does not work")
	// ErrNoGroup reports a group this node is not in. A group that does not exist and one this
	// node never joined look the same.
	ErrNoGroup = errors.New("app: unknown group")
	// ErrBadGroup reports a title, description or kind that is not acceptable.
	ErrBadGroup = errors.New("app: bad group data")
	// ErrNoRight reports a member who may not do this in the group.
	ErrNoRight = errors.New("app: not allowed in this group")
	// ErrOwnerStays reports an owner who tries to leave a group that still has members.
	ErrOwnerStays = errors.New("app: hand the group over before leaving it")
	// ErrNoMember reports an account that is not in the group.
	ErrNoMember = errors.New("app: no such member")
)

// Group kinds the app offers, the v1 words.
const (
	GroupPrivate = "private" // closed: people come in by invitations
	GroupChannel = "channel" // broadcast: few write, everyone reads
	GroupOpen    = "open"    // an open community room
)

const (
	maxGroupTitle = 64
	maxGroupDesc  = 256
	maxGroups     = 500
	groupPrefix   = "gr/"
	linkPrefix    = "echofog-group:2:"
	maxInviteLink = 4 << 10
)

// BookConfig configures a GroupBook.
type BookConfig struct {
	// Pub is the account's Ed25519 key, Sign signs with its private key.
	Pub  [32]byte
	Sign func([]byte) ([]byte, error)
	// Store keeps the group records.
	Store store.KV
	Clock Clock
}

// groupRec is what this node keeps about a group it is in.
type groupRec struct {
	Genesis *groups.GroupGenesisManifest `json:"genesis"`
	Joined  int64                        `json:"joined"`
	Via     string                       `json:"via,omitempty"`
	Issuer  string                       `json:"issuer,omitempty"` // hex key of who let us in
	// JoinSent: our own join event was written for the invitation we hold; it is written once, so that a
	// member who was removed does not come back by themselves.
	JoinSent bool `json:"joinSent,omitempty"`
	// Added: this node was let in and the screen was told of it, once.
	Added bool `json:"added,omitempty"`
}

// GroupBook is the register of the groups this node is in and the group rules the conversation
// log asks about. It exists before the conversation engine (the engine is built with its rules,
// its resolver and its member list) and learns the engine's log later, when the App is built.
type GroupBook struct {
	cfg BookConfig
	log atomic.Pointer[convlog.Log]

	mu     sync.Mutex
	loaded bool
	recs   map[string]*groupRec
	byConv map[convlog.ConvID]string
}

// NewGroupBook returns a book that keeps its records in cfg.Store.
func NewGroupBook(cfg BookConfig) (*GroupBook, error) {
	if cfg.Store == nil || cfg.Sign == nil || cfg.Clock == nil {
		return nil, errors.New("app: a group book needs a store, a signer and a clock")
	}
	return &GroupBook{cfg: cfg, recs: map[string]*groupRec{}, byConv: map[convlog.ConvID]string{}}, nil
}

func (b *GroupBook) attach(l *convlog.Log) { b.log.Store(l) }

func (b *GroupBook) load() {
	if b.loaded {
		return
	}
	b.loaded = true
	es, err := b.cfg.Store.Scan([]byte(groupPrefix), store.MaxScanLimit)
	if err != nil {
		return
	}
	for _, e := range es {
		var r groupRec
		if json.Unmarshal(e.Value, &r) != nil || r.Genesis == nil || groups.VerifyGenesis(r.Genesis) != nil {
			continue
		}
		b.recs[r.Genesis.GroupID] = &r
		b.byConv[convlog.GroupConv(r.Genesis.GroupID)] = r.Genesis.GroupID
	}
}

// rec returns a copy of a group's record.
func (b *GroupBook) rec(id string) (groupRec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	r, ok := b.recs[id]
	if !ok {
		return groupRec{}, false
	}
	return *r, true
}

// put saves a record.
func (b *GroupBook) put(r groupRec) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	id := r.Genesis.GroupID
	if _, known := b.recs[id]; !known && len(b.recs) >= maxGroups {
		return ErrBadGroup
	}
	if err := b.cfg.Store.Put([]byte(groupPrefix+id), raw); err != nil {
		return err
	}
	cp := r
	b.recs[id] = &cp
	b.byConv[convlog.GroupConv(id)] = id
	return nil
}

// claimJoin marks the join of a group as written and returns its record; false when it was written
// already or the group is not kept.
func (b *GroupBook) claimJoin(id string) (groupRec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	r, ok := b.recs[id]
	if !ok || r.JoinSent || r.Via == "" {
		return groupRec{}, false
	}
	r.JoinSent = true
	if raw, err := json.Marshal(r); err == nil {
		_ = b.cfg.Store.Put([]byte(groupPrefix+id), raw)
	}
	return *r, true
}

// forget removes a record.
func (b *GroupBook) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	delete(b.recs, id)
	delete(b.byConv, convlog.GroupConv(id))
	_ = b.cfg.Store.Delete([]byte(groupPrefix + id))
}

// ids lists the groups, in the order of the records.
func (b *GroupBook) ids() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	out := make([]string, 0, len(b.recs))
	for id := range b.recs {
		out = append(out, id)
	}
	return out
}

// Conv is the group conversation a conversation id names (convsync.Config.Groups).
func (b *GroupBook) Conv(id convlog.ConvID) (convlog.Conv, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	g, ok := b.byConv[id]
	if !ok {
		return convlog.Conv{}, false
	}
	return convlog.Group(g), true
}

// Members lists the keys of a group's members (convsync.Config.Members): what its log says, the
// founder, and who let us in while the roster has not come.
func (b *GroupBook) Members(c convlog.Conv) [][32]byte {
	r, ok := b.rec(c.Group)
	if !ok {
		return nil
	}
	seen := map[[32]byte]bool{}
	var out [][32]byte
	add := func(p [32]byte) {
		if p != ([32]byte{}) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if l := b.log.Load(); l != nil {
		for _, m := range l.Members(c) {
			add(m.Pub)
		}
	}
	// until the log holds the roster, the founder and who let us in are the ones to ask; after it
	// the log alone says who is a member (a banned founder gets nothing)
	if l := b.log.Load(); l == nil || !l.HasRoster(c) {
		add([32]byte(r.Genesis.CreatorPub))
		if p, ok := keyOfHex(r.Issuer); ok {
			add(p)
		}
	}
	return out
}

// SharedGroups lists the groups we and the account behind pub are both in
// (convsync.Config.SharedGroups).
func (b *GroupBook) SharedGroups(pub [32]byte) []convlog.Conv {
	var out []convlog.Conv
	for _, id := range b.ids() {
		c := convlog.Group(id)
		for _, m := range b.Members(c) {
			if m == pub {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func keyOfHex(s string) ([32]byte, bool) {
	var k [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return k, false
	}
	copy(k[:], raw)
	return k, true
}

// ---- rules ---------------------------------------------------------------------------------------------------

// Rules is the group rules the conversation log wants (convsync.Config.Rules).
func (b *GroupBook) Rules() convlog.GroupRules { return bookRules{b} }

type bookRules struct{ b *GroupBook }

// Current: until a group's log holds its roster only the founder is known, as the owner.
func (r bookRules) Current(c convlog.Conv, a types.PeerID) convlog.Rights {
	g, ok := r.b.rec(c.Group)
	if !ok || g.Genesis.Creator != a {
		return convlog.Rights{}
	}
	return convlog.PermRights(uint32(groups.PermAll))
}

func (r bookRules) Founder(c convlog.Conv) (types.PeerID, bool) {
	g, ok := r.b.rec(c.Group)
	if !ok {
		return types.PeerID{}, false
	}
	return g.Genesis.Creator, true
}

func (bookRules) Role(name string) (convlog.Role, bool) {
	r, err := groups.ResolveRole(name)
	if err != nil {
		return convlog.Role{}, false
	}
	return convlog.Role{Name: r.Name, Rank: r.Rank, Perms: uint32(r.Permissions)}, true
}

// NeedsInvite: a private group counts a member's own join only with an invitation.
func (r bookRules) NeedsInvite(c convlog.Conv) bool {
	g, ok := r.b.rec(c.Group)
	return !ok || g.Genesis.Type == groups.GroupTypePrivateGroup
}

func (r bookRules) DefaultRole(c convlog.Conv) string {
	if g, ok := r.b.rec(c.Group); ok && g.Genesis.Type == groups.GroupTypePublicChannel {
		return groups.RoleChannelSubscriber.Name
	}
	return groups.RoleMember.Name
}

// ---- invitation links ----------------------------------------------------------------------------------------

// invitePayload is what an invitation link carries. The maker signs it, so a link cannot be
// changed on the way (a longer term, no approval); the log still decides whether it lets anybody in.
type invitePayload struct {
	Genesis  *groups.GroupGenesisManifest `json:"g"`
	Nonce    string                       `json:"n"`
	Exp      int64                        `json:"e"`
	Approval bool                         `json:"a,omitempty"`
	Issuer   string                       `json:"i"` // hex key of the maker
	Sig      []byte                       `json:"s"`
}

func (p *invitePayload) digest() []byte {
	h := p.Genesis.CanonicalHash()
	b := append([]byte("echofog/v2/group-invite\x00"), h...)
	b = append(b, p.Nonce...)
	var n [9]byte
	binary.BigEndian.PutUint64(n[:8], uint64(p.Exp))
	if p.Approval {
		n[8] = 1
	}
	b = append(b, n[:]...)
	return append(b, p.Issuer...)
}

func (b *GroupBook) signInvite(g *groups.GroupGenesisManifest, nonce string, exp int64, approval bool) (string, error) {
	p := &invitePayload{Genesis: g, Nonce: nonce, Exp: exp, Approval: approval, Issuer: hex.EncodeToString(b.cfg.Pub[:])}
	sig, err := b.cfg.Sign(p.digest())
	if err != nil {
		return "", err
	}
	p.Sig = sig
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return linkPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// parseInvite checks a link: its form, the group's genesis and the maker's signature. Whether it
// has run out is for the caller; every failure is the same ErrInvite.
func (b *GroupBook) parseInvite(link string) (*invitePayload, error) {
	link = unwebInvite(strings.TrimSpace(link))
	if len(link) > maxInviteLink || !strings.HasPrefix(link, linkPrefix) {
		return nil, ErrInvite
	}
	raw, err := base64.RawURLEncoding.DecodeString(link[len(linkPrefix):])
	if err != nil {
		return nil, ErrInvite
	}
	var p invitePayload
	if json.Unmarshal(raw, &p) != nil || p.Genesis == nil || groups.VerifyGenesis(p.Genesis) != nil {
		return nil, ErrInvite
	}
	key, ok := keyOfHex(p.Issuer)
	if !ok || len(p.Nonce) != 32 || !ed25519.Verify(key[:], p.digest(), p.Sig) {
		return nil, ErrInvite
	}
	return &p, nil
}

// groupWebMarker is where the data of the web form of a group invitation starts: the join page, then a fragment.
// The data sits after the "#", so it never reaches the page's server (the web form of a contact link is the same).
const groupWebMarker = "/join/#group/"

// webInvite is the https form of an invitation link: https://<join host>/join/#group/<data>. Empty when
// the node has no join page or the link is none.
func (a *App) webInvite(link string) string {
	host := a.cfg.JoinHost
	if host == "" || strings.ContainsAny(host, "/?#@\\ ") || !strings.HasPrefix(link, linkPrefix) {
		return ""
	}
	return "https://" + host + groupWebMarker + link[len(linkPrefix):]
}

// unwebInvite turns the web form back into the app's: anything else comes back as it is. The host is
// any host without a path, query, user part or fragment; the data must be in the fragment alone.
func unwebInvite(s string) string {
	if !strings.HasPrefix(s, "https://") {
		return s
	}
	rest := s[len("https://"):]
	i := strings.Index(rest, groupWebMarker)
	if i <= 0 || strings.ContainsAny(rest[:i], "/?#@\\ ") {
		return s
	}
	return linkPrefix + rest[i+len(groupWebMarker):]
}

func newNonce(rnd func([]byte) (int, error)) (string, error) {
	var n [16]byte
	if _, err := rnd(n[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(n[:]), nil
}

func nodeHexOfPub(pub [32]byte) string {
	n := convsync.NodeOf(pub)
	return hex.EncodeToString(n[:])
}

// pubOfPeer finds the account key the logs of the groups name behind a peer id.
func (b *GroupBook) pubOfPeer(p types.PeerID) ([32]byte, bool) {
	for _, id := range b.ids() {
		for _, pub := range b.Members(convlog.Group(id)) {
			if types.PeerPubKeyToID(types.PeerPublicKey(pub)) == p {
				return pub, true
			}
		}
	}
	return [32]byte{}, false
}
