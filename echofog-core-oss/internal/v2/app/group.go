package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// ErrStillIn reports a local removal of a group this node is still a member of with others.
var ErrStillIn = errors.New("app: leave the group first")

const (
	// inviteMin and inviteMax bound the term of an invitation.
	inviteMin = time.Minute
	inviteMax = 365 * 24 * time.Hour
	// maxTimeoutMinutes is the longest timeout the log takes (28 days).
	maxTimeoutMinutes = 28 * 24 * 60
	// maxJoinRequests bounds the requests kept for one group.
	maxJoinRequests = 200
	// handOverBytes bounds the history sent to a newcomer at once; the sync brings the rest.
	handOverBytes = 1 << 20
	// handOverEvery is the least time between two hand-overs of a group's history to one key.
	handOverEvery = 10 * time.Minute
	maxHanded     = 4096
	// requestTTL is how long a join request and a pending group wait for an answer.
	requestTTL    = 7 * 24 * time.Hour
	joinPrefix    = "fogjoin2:"
	maxJoinMsg    = 2 << 10
	requestPrefix = "jr/"
)

// GroupInfo is one line of the group list.
type GroupInfo struct {
	ID, Title, Description, Kind string
	Role                         string
	Owner                        bool
	Members                      int
	Created                      int64
	Pending                      bool // asked to join, waiting for a decision or for the group's history
	Unread                       int
}

// GroupMember is one member of a group.
type GroupMember struct {
	Node, Role, Title string
	Rank              int
	Owner, Self       bool
	Alias             string
	Online            bool
	Timeout           int64 // unix seconds; 0: none
}

// GroupRole is a role of a group.
type GroupRole struct {
	ID, Title string
	Color     uint32
	Rank      int
	Perms     uint32
	Builtin   bool
}

// InviteSpec is an invitation as its maker wants it.
type InviteSpec struct {
	TTL         time.Duration
	Uses        uint32 // 0: no limit
	Role, Label string
	Approval    bool
}

// GroupInvite is an invitation of a group.
type GroupInvite struct {
	Nonce, Role, Label, Issuer string
	Exp                        int64
	Uses, Used                 uint32
	Revoked, Approval          bool
	Link                       string // only for one made on this node
}

// InvitePreview is what a link shows before joining.
type InvitePreview struct {
	GroupID, Title, Description, Kind string
	Expires                           int64
	Approval                          bool
	Member                            bool // already in
}

// JoinRequest is a newcomer waiting for a decision.
type JoinRequest struct {
	Node   string
	TS     int64
	Invite string
}

// ---- plumbing ------------------------------------------------------------------------------------------------

func (a *App) groupEnv() (*GroupBook, Convs, error) {
	if a.cfg.Groups == nil {
		return nil, nil, ErrGroupOff
	}
	cv, err := a.convs()
	if err != nil {
		return nil, nil, err
	}
	return a.cfg.Groups, cv, nil
}

// grp finds a group this node keeps.
func (a *App) grp(id string) (*GroupBook, Convs, convlog.Conv, groupRec, error) {
	b, cv, err := a.groupEnv()
	if err != nil {
		return nil, nil, convlog.Conv{}, groupRec{}, err
	}
	r, ok := b.rec(id)
	if !ok {
		return nil, nil, convlog.Conv{}, groupRec{}, ErrNoGroup
	}
	c := convlog.Group(id)
	if err := cv.Log().Open(c); err != nil {
		return nil, nil, convlog.Conv{}, groupRec{}, err
	}
	return b, cv, c, r, nil
}

// rightErr turns the log's refusal into the app's.
func rightErr(err error) error {
	if errors.Is(err, convlog.ErrForbidden) || errors.Is(err, convlog.ErrNotParticipant) {
		return ErrNoRight
	}
	return err
}

func (a *App) selfPubBytes() []byte { p := a.cfg.Groups.cfg.Pub; return p[:] }

// publishGroup sends an event of ours to the group. An unreachable member is not a failure of the author:
// the event is in the log and the sync brings it.
func (a *App) publishGroup(ctx context.Context, cv Convs, c convlog.Conv, raw []byte) {
	_ = cv.Publish(ctx, c, raw)
}

func (a *App) memberEvent(cv Convs, c convlog.Conv, b *convlog.MemberBody) ([]byte, error) {
	body, err := proto.Marshal(b)
	if err != nil {
		return nil, err
	}
	raw, _, err := cv.Log().Append(c, convlog.Draft{Kind: convlog.KindMember, Body: body})
	return raw, rightErr(err)
}

func validText(s string, max int, needed, nl bool) bool {
	if needed && strings.TrimSpace(s) == "" {
		return false
	}
	n := 0
	for _, r := range s {
		n++
		if unicode.IsControl(r) && !(nl && r == '\n') {
			return false
		}
	}
	return n <= max
}

func groupKind(t groups.GroupType) string {
	switch t {
	case groups.GroupTypePublicChannel:
		return GroupChannel
	case groups.GroupTypeOpenSwarm:
		return GroupOpen
	}
	return GroupPrivate
}

// info builds a group's line.
func (a *App) info(cv Convs, c convlog.Conv, r groupRec) GroupInfo {
	g := r.Genesis
	out := GroupInfo{ID: g.GroupID, Title: g.Title, Description: g.Description, Kind: groupKind(g.Type), Created: g.CreatedAt, Pending: true}
	self := cv.Log().Self()
	members := cv.Log().Members(c)
	for _, m := range members {
		if m.Peer == self {
			out.Pending = false
			out.Role = m.Role.Name
			out.Owner = m.Role.Rank >= 100
		}
	}
	if !out.Pending {
		out.Members = len(members)
		if meta, ok := cv.Log().Meta(c); ok {
			out.Unread = meta.UnreadAll()
		}
	}
	return out
}

// ---- groups --------------------------------------------------------------------------------------------------

// CreateGroup makes a group; the caller is its owner. kind is GroupPrivate, GroupChannel or GroupOpen.
func (a *App) CreateGroup(ctx context.Context, title, desc, kind string) (GroupInfo, error) {
	b, cv, err := a.groupEnv()
	if err != nil {
		return GroupInfo{}, err
	}
	var typ groups.GroupType
	switch kind {
	case GroupPrivate:
		typ = groups.GroupTypePrivateGroup
	case GroupChannel:
		typ = groups.GroupTypePublicChannel
	case GroupOpen:
		typ = groups.GroupTypeOpenSwarm
	default:
		return GroupInfo{}, ErrBadGroup
	}
	title = strings.TrimSpace(title)
	if !validText(title, maxGroupTitle, true, false) || !validText(desc, maxGroupDesc, false, true) {
		return GroupInfo{}, ErrBadGroup
	}
	pub := b.cfg.Pub
	creator := types.PeerPubKeyToID(pub)
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return GroupInfo{}, err
	}
	gen := &groups.GroupGenesisManifest{
		GroupID: fmt.Sprintf("fog_grp_%s_%s", hex.EncodeToString(creator[:6]), hex.EncodeToString(salt[:4])),
		Title:   title, Description: desc, Type: typ, Creator: creator, CreatorPub: types.IdentityPublicKey(pub),
		CreatedAt: a.cfg.Clock.Now().UnixMilli(), Salt: salt,
	}
	sig, err := b.cfg.Sign(gen.CanonicalHash())
	if err != nil || len(sig) != len(gen.Signature) {
		return GroupInfo{}, errors.New("app: could not sign the group")
	}
	copy(gen.Signature[:], sig)
	if err := groups.VerifyGenesis(gen); err != nil {
		return GroupInfo{}, err
	}
	rec := groupRec{Genesis: gen, Joined: gen.CreatedAt}
	if err := b.put(rec); err != nil {
		return GroupInfo{}, err
	}
	c := convlog.Group(gen.GroupID)
	if err := cv.Log().Open(c); err != nil {
		b.forget(gen.GroupID)
		return GroupInfo{}, err
	}
	body, err := proto.Marshal(&convlog.MemberBody{Action: convlog.MemberRoster,
		Roster: []*convlog.RosterEntry{{Member: pub[:], Role: groups.RoleOwner.Name}}})
	if err == nil {
		_, _, err = cv.Log().AppendKept(c, convlog.Draft{Kind: convlog.KindMember, Body: body})
	}
	if err != nil {
		b.forget(gen.GroupID)
		_ = cv.Log().Drop(c)
		return GroupInfo{}, err
	}
	return a.info(cv, c, rec), nil
}

// Groups lists the groups this node keeps, the newest first.
func (a *App) Groups() ([]GroupInfo, error) {
	b, cv, err := a.groupEnv()
	if err != nil {
		return nil, err
	}
	var out []GroupInfo
	for _, id := range b.ids() {
		r, ok := b.rec(id)
		if !ok {
			continue
		}
		c := convlog.Group(id)
		if cv.Log().Open(c) != nil {
			continue
		}
		gi := a.info(cv, c, r)
		if gi.Pending && a.cfg.Clock.Now().Sub(time.UnixMilli(r.Joined)) >= requestTTL {
			// nobody answered in a week: the entry goes (a refusal is never told)
			b.forget(id)
			a.dropRequests(id)
			_ = cv.Log().Drop(c)
			continue
		}
		out = append(out, gi)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GroupInfoOf is one group's line.
func (a *App) GroupInfoOf(id string) (GroupInfo, error) {
	_, cv, c, r, err := a.grp(id)
	if err != nil {
		return GroupInfo{}, err
	}
	return a.info(cv, c, r), nil
}

// LeaveGroup leaves a group. The owner hands the group over first (GroupTransfer) unless they are
// alone in it.
func (a *App) LeaveGroup(ctx context.Context, id string) error {
	b, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	self := cv.Log().Self()
	members := cv.Log().Members(c)
	in := false
	for _, m := range members {
		if m.Peer != self {
			continue
		}
		in = true
		if m.Role.Rank >= 100 && len(members) > 1 {
			return ErrOwnerStays
		}
	}
	if in && len(members) > 1 {
		raw, err := a.memberEvent(cv, c, &convlog.MemberBody{Action: convlog.MemberRemove, Member: a.selfPubBytes()})
		if err != nil {
			return err
		}
		a.publishGroup(ctx, cv, c, raw)
	}
	b.forget(id)
	a.dropRequests(id)
	return cv.Log().Drop(c)
}

// ForgetGroup removes a group from this node: for an owner alone in it, a removed or banned member,
// or a newcomer who was never let in. A member leaves (LeaveGroup).
func (a *App) ForgetGroup(id string) error {
	b, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	if cv.Log().Participant(c, cv.Log().Self()) && len(cv.Log().Members(c)) > 1 {
		return ErrStillIn
	}
	b.forget(id)
	a.dropRequests(id)
	return cv.Log().Drop(c)
}

// ---- members -------------------------------------------------------------------------------------------------

func (a *App) memberOf(cv Convs, c convlog.Conv, node string) (convlog.GroupMember, error) {
	for _, m := range cv.Log().Members(c) {
		if m.Pub != ([32]byte{}) && nodeHexOfPub(m.Pub) == node {
			return m, nil
		}
	}
	return convlog.GroupMember{}, ErrNoMember
}

// GroupMembers lists a group's members, the owner first.
func (a *App) GroupMembers(id string) ([]GroupMember, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	self := cv.Log().Self()
	var out []GroupMember
	for _, m := range cv.Log().Members(c) {
		gm := GroupMember{Role: m.Role.Name, Title: m.Role.Title, Rank: m.Role.Rank, Owner: m.Role.Rank >= 100, Self: m.Peer == self,
			Timeout: cv.Log().TimeoutOf(c, m.Peer)}
		if gm.Title == "" {
			gm.Title = m.Role.Name
		}
		if m.Pub == ([32]byte{}) {
			gm.Node = hex.EncodeToString(m.Peer[:])
		} else {
			gm.Node = nodeHexOfPub(m.Pub)
			a.cmu.Lock()
			rec, lerr := a.loadContact(gm.Node)
			a.cmu.Unlock()
			if lerr == nil {
				gm.Alias = rec.Alias
			}
			if n, ok := nodeBytes(gm.Node); ok {
				gm.Online = a.Connected(types.PeerID(n))
			}
		}
		out = append(out, gm)
	}
	return out, nil
}

// GroupSetRole gives a member a role.
func (a *App) GroupSetRole(ctx context.Context, id, node, role string) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	m, err := a.memberOf(cv, c, node)
	if err != nil {
		return err
	}
	raw, err := a.memberEvent(cv, c, &convlog.MemberBody{Action: convlog.MemberRole, Member: m.Pub[:], Role: role})
	if err != nil {
		return err
	}
	a.publishGroup(ctx, cv, c, raw)
	return nil
}

// GroupTransfer hands the group over to a member; the owner becomes an admin.
func (a *App) GroupTransfer(ctx context.Context, id, node string) error {
	return a.GroupSetRole(ctx, id, node, groups.RoleOwner.Name)
}

// GroupRemove removes a member, or bans them.
func (a *App) GroupRemove(ctx context.Context, id, node string, ban bool) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	m, err := a.memberOf(cv, c, node)
	if err != nil {
		return err
	}
	if m.Peer == cv.Log().Self() {
		return a.LeaveGroup(ctx, id)
	}
	act := convlog.MemberRemove
	if ban {
		act = convlog.MemberBan
	}
	raw, err := a.memberEvent(cv, c, &convlog.MemberBody{Action: act, Member: m.Pub[:], UptoSeq: cv.Log().AuthorSeq(c, m.Peer)})
	if err != nil {
		return err
	}
	a.publishGroup(ctx, cv, c, raw)
	// the member list no longer holds them: they hear of it from us
	a.sendEvent(ctx, m.Pub, raw)
	return nil
}

// sendEvent hands one log event to one account.
func (a *App) sendEvent(ctx context.Context, pub [32]byte, raw []byte) {
	if a.cfg.Chat != nil {
		_, _ = a.cfg.Chat.Send(ctx, convsync.NodeOf(pub), &chatpb.PersonalMessageProto{ConvEvent: raw})
	}
}

// GroupTimeout keeps a member from writing for the minutes (0 lifts a timeout).
func (a *App) GroupTimeout(ctx context.Context, id, node string, minutes int) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	if minutes < 0 || minutes > maxTimeoutMinutes {
		return ErrRequest
	}
	m, err := a.memberOf(cv, c, node)
	if err != nil {
		return err
	}
	until := a.cfg.Clock.Now().Add(time.Duration(minutes) * time.Minute).Unix()
	if minutes == 0 {
		until = 1 // a time long past lifts it
	}
	raw, _, err := cv.Log().AppendTimeout(c, m.Pub[:], until, cv.Log().AuthorSeq(c, m.Peer))
	if err != nil {
		return rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	return nil
}

// ---- roles ---------------------------------------------------------------------------------------------------

var builtinRoles = []string{"owner", "admin", "moderator", "member", "subscriber", "restricted"}

// GroupRoles lists the roles of a group: the built-in ones as the log changed them, and the custom ones.
func (a *App) GroupRoles(id string) ([]GroupRole, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	byID := map[string]GroupRole{}
	for _, n := range builtinRoles {
		r, _ := groups.ResolveRole(n)
		byID[n] = GroupRole{ID: n, Title: n, Rank: r.Rank, Perms: uint32(r.Permissions), Builtin: true}
	}
	for _, r := range cv.Log().Roles(c) {
		_, builtin := byID[r.Name]
		t := r.Title
		if t == "" {
			t = r.Name
		}
		byID[r.Name] = GroupRole{ID: r.Name, Title: t, Color: r.Color, Rank: r.Rank, Perms: r.Perms, Builtin: builtin}
	}
	out := make([]GroupRole, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank > out[j].Rank
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GroupRoleSave makes or changes a role.
func (a *App) GroupRoleSave(ctx context.Context, id string, r GroupRole) (GroupRole, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return GroupRole{}, err
	}
	raw, _, err := cv.Log().AppendRole(c, &convlog.RoleDef{Id: r.ID, Title: r.Title, Color: r.Color, Rank: int32(r.Rank), Perms: r.Perms})
	if err != nil {
		if errors.Is(err, convlog.ErrBadBody) {
			return GroupRole{}, ErrRequest
		}
		return GroupRole{}, rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	roles, _ := a.GroupRoles(id)
	for _, x := range roles {
		if x.ID == r.ID {
			return x, nil
		}
	}
	return r, nil
}

// GroupRoleRemove removes a custom role; its members get the group's default role.
func (a *App) GroupRoleRemove(ctx context.Context, id, role string) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	raw, _, err := cv.Log().AppendRole(c, &convlog.RoleDef{Id: role, Removed: true})
	if err != nil {
		if errors.Is(err, convlog.ErrBadBody) {
			return ErrRequest
		}
		return rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	return nil
}

// GroupRights is what this node's account may do in a channel ("" the main one).
func (a *App) GroupRights(id, channel string) (convlog.Rights, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return convlog.Rights{}, err
	}
	l := cv.Log()
	r := l.RightsIn(c, l.Self(), channel)
	// a read-only channel and a slow mode narrow what the role allows
	r.Send = r.Send && l.MayPost(c, l.Self(), channel, false, false)
	return r, nil
}

// GroupModerationLog is the moderation log of a group, the newest first.
func (a *App) GroupModerationLog(id string) ([]convlog.ModEntry, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	return cv.Log().ModLog(c, 500), nil
}

// ---- channels ------------------------------------------------------------------------------------------------

func specOf(ch convlog.Channel) convlog.ChannelSpec {
	return convlog.ChannelSpec{ID: ch.ID, Name: ch.Name, Topic: ch.Topic, Parent: ch.Parent, Open: ch.Open, Voice: ch.Voice,
		ReadOnly: ch.ReadOnly, Category: ch.Category, Position: ch.Position, SlowSecs: ch.SlowSecs, Overrides: ch.Overrides}
}

// GroupChannels lists the channels and categories of a group (not the main channel, not removed ones).
func (a *App) GroupChannels(id string) ([]convlog.Channel, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	var out []convlog.Channel
	for _, ch := range cv.Log().Channels(c) {
		if !ch.Removed && ch.ID != "" {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (a *App) saveChannel(ctx context.Context, cv Convs, c convlog.Conv, s convlog.ChannelSpec) (convlog.Channel, error) {
	raw, _, err := cv.Log().AppendChannel(c, s)
	if err != nil {
		if errors.Is(err, convlog.ErrBadBody) {
			return convlog.Channel{}, ErrRequest
		}
		return convlog.Channel{}, rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	ch, _ := cv.Log().ChannelOf(c, s.ID)
	return ch, nil
}

// GroupChannelSave makes or changes a channel or a category.
func (a *App) GroupChannelSave(ctx context.Context, id string, s convlog.ChannelSpec) (convlog.Channel, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return convlog.Channel{}, err
	}
	if s.ID == "" {
		return convlog.Channel{}, ErrRequest // the main channel has its own call
	}
	return a.saveChannel(ctx, cv, c, s)
}

// GroupChannelRemove removes a channel or a category.
func (a *App) GroupChannelRemove(ctx context.Context, id, ch string) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	cur, ok := cv.Log().ChannelOf(c, ch)
	if !ok || cur.Removed || ch == "" {
		return ErrRequest
	}
	s := specOf(cur)
	s.Remove = true
	_, err = a.saveChannel(ctx, cv, c, s)
	return err
}

func sameKind(x, y convlog.Channel) bool { return x.Voice == y.Voice && x.Category == y.Category }

// GroupChannelMove puts a channel at index among the channels of its kind.
func (a *App) GroupChannelMove(ctx context.Context, id, ch string, index int) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	cur, ok := cv.Log().ChannelOf(c, ch)
	if !ok || cur.Removed || ch == "" || index < 0 {
		return ErrRequest
	}
	var sibs []convlog.Channel
	for _, x := range cv.Log().Channels(c) {
		if x.ID != "" && x.ID != ch && !x.Removed && sameKind(x, cur) {
			sibs = append(sibs, x)
		}
	}
	sort.SliceStable(sibs, func(i, j int) bool {
		if sibs[i].Position != sibs[j].Position {
			return sibs[i].Position < sibs[j].Position
		}
		return sibs[i].Created < sibs[j].Created
	})
	if index > len(sibs) {
		index = len(sibs)
	}
	order := append(append(append([]convlog.Channel{}, sibs[:index]...), cur), sibs[index:]...)
	for i, x := range order {
		if int(x.Position) == i && x.ID != ch {
			continue
		}
		s := specOf(x)
		s.Position = int32(i)
		if _, err := a.saveChannel(ctx, cv, c, s); err != nil {
			return err
		}
	}
	return nil
}

// GroupChannelPlace puts a channel in a category ("" takes it out of any).
func (a *App) GroupChannelPlace(ctx context.Context, id, ch, parent string) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	cur, ok := cv.Log().ChannelOf(c, ch)
	if !ok || cur.Removed || cur.Category {
		return ErrRequest
	}
	if parent != "" {
		p, ok := cv.Log().ChannelOf(c, parent)
		if !ok || p.Removed || !p.Category {
			return ErrRequest
		}
	}
	s := specOf(cur)
	s.Parent = parent
	_, err = a.saveChannel(ctx, cv, c, s)
	return err
}

// GroupCategoriesSetup makes a text and a voice category (once) and puts the channels that sit in
// none under the one of their kind. It returns the ids of the two categories.
func (a *App) GroupCategoriesSetup(ctx context.Context, id, text, voice string) ([]string, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	ids := []string{"cat-text", "cat-voice"}
	for i, name := range []string{text, voice} {
		if cur, ok := cv.Log().ChannelOf(c, ids[i]); ok && !cur.Removed {
			continue
		}
		if _, err := a.saveChannel(ctx, cv, c, convlog.ChannelSpec{ID: ids[i], Name: name, Category: true, Position: int32(i)}); err != nil {
			return nil, err
		}
	}
	for _, x := range cv.Log().Channels(c) {
		if x.ID == "" || x.Removed || x.Category || x.Parent != "" {
			continue
		}
		parent := ids[0]
		if x.Voice {
			parent = ids[1]
		}
		s := specOf(x)
		s.Parent = parent
		if _, err := a.saveChannel(ctx, cv, c, s); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// GroupMainChannel is the main channel's settings.
func (a *App) GroupMainChannel(id string) (convlog.Channel, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return convlog.Channel{}, err
	}
	return cv.Log().MainChannel(c), nil
}

// GroupMainChannelSave changes the main channel's settings (name, topic, slow mode, overrides).
func (a *App) GroupMainChannelSave(ctx context.Context, id string, s convlog.ChannelSpec) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	s.ID, s.Voice, s.Remove, s.Category = "", false, false, false
	_, err = a.saveChannel(ctx, cv, c, s)
	return err
}

// GroupPhoto sets the group's picture.
func (a *App) GroupPhoto(ctx context.Context, id string, photo []byte) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	raw, _, err := cv.Log().AppendGroupPhoto(c, photo)
	if err != nil {
		if errors.Is(err, convlog.ErrBadBody) {
			return ErrRequest
		}
		return rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	return nil
}

// ChatThreads lists the messages of a chat that have comments, the most recently active first.
func (a *App) ChatThreads(ctx context.Context, ref ChatRef) ([]convlog.Message, error) {
	c, err := a.resolve(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	cv, _ := a.convs()
	return a.dress(cv.Log().Threads(c, ref.Channel, 0)), nil
}

// ChatThreadTitle names the thread under root.
func (a *App) ChatThreadTitle(ctx context.Context, ref ChatRef, root, title string) error {
	return a.chatOp(ctx, ref, root, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, error) {
		raw, _, err := l.AppendThreadTitle(c, t, title)
		return raw, err
	})
}

// ---- invitations ---------------------------------------------------------------------------------------------

func inviteInfo(i convlog.Invite) GroupInvite {
	return GroupInvite{Nonce: i.N, Role: i.Role, Label: i.Label, Issuer: hex.EncodeToString(i.Issuer[:]), Exp: i.Exp, Uses: i.Uses,
		Used: i.Used, Revoked: i.Revoked, Approval: i.Approval}
}

// GroupInviteCreate makes an invitation and its link.
func (a *App) GroupInviteCreate(ctx context.Context, id string, s InviteSpec) (GroupInvite, error) {
	b, cv, c, r, err := a.grp(id)
	if err != nil {
		return GroupInvite{}, err
	}
	if s.TTL < inviteMin || s.TTL > inviteMax {
		return GroupInvite{}, ErrRequest
	}
	nonce, err := newNonce(rand.Read)
	if err != nil {
		return GroupInvite{}, err
	}
	exp := a.cfg.Clock.Now().Add(s.TTL).Unix()
	raw, _, err := cv.Log().AppendInvite(c, &convlog.InviteDef{N: nonce, Exp: exp, Uses: s.Uses, Role: s.Role, Label: s.Label, Approval: s.Approval})
	if err != nil {
		if errors.Is(err, convlog.ErrBadBody) {
			return GroupInvite{}, ErrRequest
		}
		return GroupInvite{}, rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	link, err := b.signInvite(r.Genesis, nonce, exp, s.Approval)
	if err != nil {
		return GroupInvite{}, err
	}
	inv, _ := cv.Log().InviteOf(c, nonce)
	out := inviteInfo(inv)
	out.Link = link
	return out, nil
}

// GroupInvites lists the invitations of a group, the newest first.
func (a *App) GroupInvites(id string) ([]GroupInvite, error) {
	b, cv, c, r, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	self := cv.Log().Self()
	var out []GroupInvite
	for _, i := range cv.Log().Invites(c) {
		gi := inviteInfo(i)
		if i.Issuer == self && !i.Spent() {
			gi.Link, _ = b.signInvite(r.Genesis, i.N, i.Exp, i.Approval)
		}
		out = append(out, gi)
	}
	return out, nil
}

// GroupInviteRevoke ends an invitation.
func (a *App) GroupInviteRevoke(ctx context.Context, id, nonce string) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	inv, ok := cv.Log().InviteOf(c, nonce)
	if !ok {
		return ErrRequest
	}
	raw, _, err := cv.Log().AppendInvite(c, &convlog.InviteDef{N: nonce, Exp: inv.Exp, Revoked: true})
	if err != nil {
		return rightErr(err)
	}
	a.publishGroup(ctx, cv, c, raw)
	return nil
}

// GroupInviteContacts sends the contacts an invitation to the group, as a message in their chat. It
// returns how many were sent.
func (a *App) GroupInviteContacts(ctx context.Context, id string, nodes []string, role string) (int, error) {
	if _, _, _, _, err := a.grp(id); err != nil {
		return 0, err
	}
	var targets []string
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] {
			continue
		}
		seen[n] = true
		a.cmu.Lock()
		rec, err := a.loadContact(n)
		a.cmu.Unlock()
		if err == nil && !rec.Blocked {
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}
	inv, err := a.GroupInviteCreate(ctx, id, InviteSpec{TTL: 7 * 24 * time.Hour, Uses: uint32(len(targets)), Role: role})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, n := range targets {
		if _, err := a.ChatSend(ctx, ChatRef{Kind: KindDM, ID: n}, SendReq{Text: inv.Link}); err == nil {
			sent++
		}
	}
	return sent, nil
}

// InspectGroupInvite shows what a link leads to. A link that is bad, forged or past its term is
// refused with the one ErrInvite; what the group's members decided later (a revocation, a ban) is
// not known here and not shown.
func (a *App) InspectGroupInvite(link string) (InvitePreview, error) {
	b, cv, err := a.groupEnv()
	if err != nil {
		return InvitePreview{}, err
	}
	p, err := b.parseInvite(link)
	if err != nil || p.Exp <= a.cfg.Clock.Now().Unix() {
		return InvitePreview{}, ErrInvite
	}
	g := p.Genesis
	out := InvitePreview{GroupID: g.GroupID, Title: g.Title, Description: g.Description, Kind: groupKind(g.Type), Expires: p.Exp, Approval: p.Approval}
	if _, ok := b.rec(g.GroupID); ok {
		c := convlog.Group(g.GroupID)
		out.Member = cv.Log().Participant(c, cv.Log().Self())
	}
	return out, nil
}

// joinMsg is what a newcomer sends the maker of an invitation.
type joinMsg struct {
	V     int    `json:"v"`
	Group string `json:"g"`
	Nonce string `json:"n"`
	Pub   string `json:"p"`
}

func (a *App) sendJoin(ctx context.Context, issuer [32]byte, m joinMsg) error {
	if a.cfg.Chat == nil {
		return errors.New("app: no chat service")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = a.cfg.Chat.Send(ctx, convsync.NodeOf(issuer), &chatpb.PersonalMessageProto{Metadata: joinPrefix + string(raw)})
	return err
}

// JoinGroup joins a group by an invitation link. The link is checked here (form, signature, term:
// ErrInvite, one error for all); the group's members decide the rest and say nothing back when they
// refuse, so a revoked invitation, one used up and a ban look the same to the newcomer: the group
// is listed as pending and nothing comes (AD20).
func (a *App) JoinGroup(ctx context.Context, link string) (GroupInfo, error) {
	b, cv, err := a.groupEnv()
	if err != nil {
		return GroupInfo{}, err
	}
	p, err := b.parseInvite(link)
	if err != nil || p.Exp <= a.cfg.Clock.Now().Unix() {
		return GroupInfo{}, ErrInvite
	}
	id := p.Genesis.GroupID
	c := convlog.Group(id)
	if old, ok := b.rec(id); ok && cv.Log().Participant(c, cv.Log().Self()) {
		return a.info(cv, c, old), nil
	}
	rec := groupRec{Genesis: p.Genesis, Joined: a.cfg.Clock.Now().UnixMilli(), Via: p.Nonce, Issuer: p.Issuer}
	if err := b.put(rec); err != nil {
		return GroupInfo{}, err
	}
	if err := cv.Log().Open(c); err != nil {
		return GroupInfo{}, err
	}
	if _, known := cv.Log().InviteOf(c, p.Nonce); !p.Approval && cv.Log().HasRoster(c) && known {
		// the history and the invitation are here already (we were in before): write the join now,
		// the members weigh it
		a.joinNow(ctx, b, cv, c)
	} else {
		issuer, _ := keyOfHex(p.Issuer)
		_ = a.sendJoin(ctx, issuer, joinMsg{V: 1, Group: p.Genesis.GroupID, Nonce: p.Nonce, Pub: hex.EncodeToString(a.selfPubBytes())})
	}
	rec, _ = b.rec(id)
	return a.info(cv, c, rec), nil
}

// joinNow writes our own join event, once per invitation we took, and sends it to the members. A
// refusal is silent.
func (a *App) joinNow(ctx context.Context, b *GroupBook, cv Convs, c convlog.Conv) {
	r, ok := b.claimJoin(c.Group)
	if !ok {
		return
	}
	raw, err := a.memberEvent(cv, c, &convlog.MemberBody{Action: convlog.MemberJoin, Member: a.selfPubBytes(), Via: r.Via})
	if err != nil {
		return
	}
	a.publishGroup(ctx, cv, c, raw)
	// our own list may not name the one who let us in (we were removed once): they get it directly
	if p, ok := keyOfHex(r.Issuer); ok {
		a.sendEvent(ctx, p, raw)
	}
}

// hello opens a sync with a peer when the engine can.
func (a *App) hello(ctx context.Context, cv Convs, pub [32]byte) {
	if h, ok := cv.(interface {
		Hello(context.Context, [32]byte) error
	}); ok {
		_ = h.Hello(ctx, pub)
	}
}

// handOver sends a newcomer what the group's log holds, so that it knows who is in and what they may do.
func (a *App) handOver(ctx context.Context, cv Convs, c convlog.Conv, pub [32]byte) {
	if a.cfg.Chat == nil {
		return
	}
	for _, raw := range cv.Log().Export(c, handOverBytes) {
		_, _ = a.cfg.Chat.Send(ctx, convsync.NodeOf(pub), &chatpb.PersonalMessageProto{ConvEvent: raw})
	}
	a.hello(ctx, cv, pub)
}

// OnGroupMessage takes a join message of chat/2 (a newcomer's request to the maker of an invitation);
// it reports whether the message was one, so that it is not shown as a chat message. Wire it where
// the runtime takes chat/2 messages, after the conversation engine. Every refusal is silence.
func (a *App) OnGroupMessage(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	md := pm.GetMetadata()
	if !strings.HasPrefix(md, joinPrefix) {
		return false
	}
	b, cv, err := a.groupEnv()
	if err != nil || len(md) > maxJoinMsg {
		return true
	}
	var m joinMsg
	if json.Unmarshal([]byte(md[len(joinPrefix):]), &m) != nil || m.V != 1 {
		return true
	}
	pub, ok := keyOfHex(m.Pub)
	if !ok || convsync.NodeOf(pub) != from {
		return true
	}
	if _, ok := b.rec(m.Group); !ok {
		return true
	}
	c := convlog.Group(m.Group)
	l := cv.Log()
	if l.Open(c) != nil || !l.Participant(c, l.Self()) {
		return true
	}
	inv, ok := l.InviteOf(c, m.Nonce)
	if !ok || inv.Spent() || inv.Exp <= a.cfg.Clock.Now().Unix() {
		return true
	}
	peer := types.PeerPubKeyToID(pub)
	for _, x := range l.Banned(c) {
		if x.Peer == peer {
			return true
		}
	}
	switch {
	case l.Participant(c, peer):
		if a.mayHandOver(m.Group, pub) {
			a.handOver(ctx, cv, c, pub) // already in: a device that lost its copy
		}
	case inv.Approval:
		a.noteRequest(c, pub, inv)
	default:
		if a.mayHandOver(m.Group, pub) {
			a.handOver(ctx, cv, c, pub)
		}
	}
	return true
}

func (a *App) forgetHanded(group string, pub [32]byte) {
	a.hmu.Lock()
	delete(a.handed, group+"/"+hex.EncodeToString(pub[:]))
	a.hmu.Unlock()
}

// mayHandOver tells whether the history of a group may go to a key now: once per handOverEvery, so
// that a repeated message costs nothing. The table is bounded.
func (a *App) mayHandOver(group string, pub [32]byte) bool {
	now := a.cfg.Clock.Now()
	k := group + "/" + hex.EncodeToString(pub[:])
	a.hmu.Lock()
	defer a.hmu.Unlock()
	if a.handed == nil || len(a.handed) >= maxHanded {
		for key, t := range a.handed {
			if now.Sub(t) >= handOverEvery {
				delete(a.handed, key)
			}
		}
		if a.handed == nil || len(a.handed) >= maxHanded {
			a.handed = map[string]time.Time{}
		}
	}
	if t, ok := a.handed[k]; ok && now.Sub(t) < handOverEvery {
		return false
	}
	a.handed[k] = now
	return true
}

// ---- join requests -------------------------------------------------------------------------------------------

type requestRec struct {
	Pub   string `json:"pub"`
	TS    int64  `json:"ts"`
	Via   string `json:"via"`
	Label string `json:"label,omitempty"`
}

func requestKey(group, node string) []byte { return []byte(requestPrefix + group + "/" + node) }

// noteRequest keeps a newcomer's request when we may decide on it.
func (a *App) noteRequest(c convlog.Conv, pub [32]byte, inv convlog.Invite) {
	l := a.cfg.Convs.Log()
	if !l.RightsOf(c, l.Self()).Roles {
		return
	}
	prefix := []byte(requestPrefix + c.Group + "/")
	key := requestKey(c.Group, nodeHexOfPub(pub))
	if es, err := a.cfg.Store.Scan(prefix, maxJoinRequests); err != nil || len(es) >= maxJoinRequests {
		return
	}
	if old, err := a.cfg.Store.Get(key); err == nil {
		var r requestRec
		if json.Unmarshal(old, &r) == nil && !a.expired(r.TS) {
			return
		}
	}
	raw, err := json.Marshal(requestRec{Pub: hex.EncodeToString(pub[:]), TS: a.cfg.Clock.Now().Unix(), Via: inv.N, Label: inv.Label})
	if err == nil {
		_ = a.cfg.Store.PutTTL(key, raw, requestTTL)
	}
}

// expired tells whether a request made at ts (unix seconds) has waited too long.
func (a *App) expired(ts int64) bool {
	return a.cfg.Clock.Now().Sub(time.Unix(ts, 0)) >= requestTTL
}

func (a *App) dropRequests(group string) {
	es, err := a.cfg.Store.Scan([]byte(requestPrefix+group+"/"), maxJoinRequests)
	if err != nil {
		return
	}
	for _, e := range es {
		_ = a.cfg.Store.Delete(e.Key)
	}
}

// GroupJoinRequests lists the newcomers waiting for a decision, the oldest first.
func (a *App) GroupJoinRequests(id string) ([]JoinRequest, error) {
	if _, _, _, _, err := a.grp(id); err != nil {
		return nil, err
	}
	es, err := a.cfg.Store.Scan([]byte(requestPrefix+id+"/"), maxJoinRequests)
	if err != nil {
		return nil, err
	}
	var out []JoinRequest
	for _, e := range es {
		var r requestRec
		if json.Unmarshal(e.Value, &r) != nil {
			continue
		}
		if a.expired(r.TS) {
			_ = a.cfg.Store.Delete(e.Key)
			continue
		}
		pub, ok := keyOfHex(r.Pub)
		if !ok {
			continue
		}
		out = append(out, JoinRequest{Node: nodeHexOfPub(pub), TS: r.TS, Invite: r.Label})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

// GroupJoinDecide accepts or declines a request. A declined newcomer is told nothing.
func (a *App) GroupJoinDecide(ctx context.Context, id, node string, accept bool) error {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return err
	}
	key := requestKey(id, node)
	raw, err := a.cfg.Store.Get(key)
	if err != nil {
		return ErrRequest
	}
	var r requestRec
	if json.Unmarshal(raw, &r) != nil {
		_ = a.cfg.Store.Delete(key)
		return ErrRequest
	}
	pub, ok := keyOfHex(r.Pub)
	if !ok || a.expired(r.TS) {
		_ = a.cfg.Store.Delete(key)
		return ErrRequest
	}
	if !accept {
		return a.cfg.Store.Delete(key)
	}
	ev, err := a.memberEvent(cv, c, &convlog.MemberBody{Action: convlog.MemberJoin, Member: pub[:], Via: r.Via})
	if err != nil {
		return err
	}
	_ = a.cfg.Store.Delete(key)
	a.publishGroup(ctx, cv, c, ev)
	a.handOver(ctx, cv, c, pub)
	return nil
}

// ---- following the log ---------------------------------------------------------------------------------------

// onGroupChange runs when the log of a group changes: a newcomer whose history arrived writes their
// join, once.
func (a *App) onGroupChange(ch convlog.Change) {
	if ch.Conv.Kind != convlog.KindGroup {
		return
	}
	b := a.cfg.Groups
	cv := a.cfg.Convs
	l := cv.Log()
	for _, m := range ch.Members {
		if (m.Action == convlog.MemberRemove || m.Action == convlog.MemberBan) && len(m.Member) == 32 {
			a.forgetHanded(ch.Conv.Group, [32]byte(m.Member)) // who left may come back and be given the history again
		}
	}
	r, ok := b.rec(ch.Conv.Group)
	if !ok || r.JoinSent || l.Participant(ch.Conv, l.Self()) || !l.HasRoster(ch.Conv) {
		return
	}
	// the join comes after the invitation in the log's order: wait until the invitation is here
	if _, ok := l.InviteOf(ch.Conv, r.Via); !ok {
		return
	}
	a.joinNow(context.Background(), b, cv, ch.Conv)
}
