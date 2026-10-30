package node

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/events"
)

const (
	groupKeyPrefix = "groups/g/"
	groupMsgPrefix = "groups/m/"
	inviteScheme   = "fogroup1:" // genesis only (older invites)
	inviteScheme2  = "fogroup2:" // genesis and the members known when it was made
	maxGroupTitle  = 64
	maxGroupText   = 8192
)

type GroupDTO struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	ReadOnly    bool   `json:"readOnly"`
	Creator     string `json:"creator"`
	CreatedAt   int64  `json:"createdAt"`
	Members     int    `json:"members"`
	MyRole      string `json:"myRole"`
}

type GroupMemberDTO struct {
	PeerID    string `json:"peerId"`
	Role      string `json:"role"`
	RoleTitle string `json:"roleTitle,omitempty"` // a custom (or renamed) role's name
	RoleColor uint32 `json:"roleColor,omitempty"` // 0xRRGGBB
	JoinedAt  int64  `json:"joinedAt"`
	IsMe      bool   `json:"isMe"`
	Online    bool   `json:"online"` // the node has a link to them now (oneself: always)
	// what I may do to this member: remove, ban, give one of Roles (its present role included),
	// hand the group over to them
	CanKick     bool     `json:"canKick"`
	CanBan      bool     `json:"canBan"`
	Roles       []string `json:"roles,omitempty"`
	CanTransfer bool     `json:"canTransfer"`
	// Timeout: until when they may not write (unix seconds, 0 none); CanTimeout: I may give or
	// lift one (a member below my rank, and I may remove members)
	Timeout    int64 `json:"timeout,omitempty"`
	CanTimeout bool  `json:"canTimeout"`
}

type GroupMessageDTO struct {
	ID        string        `json:"id"`
	GroupID   string        `json:"groupId"`
	Sender    string        `json:"sender"`
	Text      string        `json:"text"`
	Timestamp int64         `json:"timestamp"`
	IsMine    bool          `json:"isMine"`
	Delivered int           `json:"delivered"` // members whose node took it at once
	Queued    int           `json:"queued"`    // members who are offline: it is sent when they are back
	Failed    int           `json:"failed"`
	File      *GroupFileDTO `json:"file,omitempty"` // a file posted in the group (Text is its name)
	// Verified: the author is certain, by their signature or because the message came over their
	// own session. Only history from a member of an older version can be unverified.
	Verified bool `json:"verified"`
}

// storedGroupMsg is a message as kept: with its author's signature, so it can be handed to a
// newcomer as history that cannot be altered on the way.
type storedGroupMsg struct {
	GroupMessageDTO
	Sig []byte `json:"sig,omitempty"`
	FD  string `json:"fd,omitempty"` // digest of the file offer the signature covers
}

// GroupFileDTO is the file of a group message; its transfer (GetFileTransfer with ID) has the state.
type GroupFileDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime,omitempty"`
	// a picture's thumbnail (base64 JPEG) and its size
	Thumb string `json:"thumb,omitempty"`
	TW    int    `json:"tw,omitempty"`
	TH    int    `json:"th,omitempty"`
	// Channel and Thread are where in a space it was posted ("" the main channel, no thread)
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`
}

// maxThumb bounds a thumbnail somebody else sent (ours are about 6 KB).
const maxThumb = 16 << 10

func thumbOK(s string) string {
	if len(s) > maxThumb {
		return ""
	}
	return s
}

type groupsHolder struct {
	mu   sync.Mutex
	f    *groupsFacade
	subs []func(GroupEvent)
}

type groupRecord struct {
	Genesis *groups.GroupGenesisManifest `json:"genesis"`
	Members []groups.MemberProfile       `json:"members"`
	Bans    map[string]string            `json:"bans,omitempty"` // peer id hex -> reason
}

type groupsFacade struct {
	mu      sync.Mutex
	mgr     *groups.GroupManager
	store   storage.Storage
	selfID  types.PeerID
	selfPub types.IdentityPublicKey
	sign    groups.SignFunc
	loaded  bool
	seq     uint32

	sendSilent func(target types.PeerID, text string) (queued bool, err error)
	isContact  func(types.PeerID) bool
	blocked    func(types.PeerID) bool // peers the user blocked: their messages and calls are ignored
	notify     func(GroupEvent)
	calls      map[string]GroupCall
	histSent   map[string]time.Time              // when history was last handed to whom
	aliveSent  map[string]int64                  // group -> when we last announced our call there (unix s)
	inCall     map[string]map[types.PeerID]int64 // call key -> who announced being in it, when (unix s)

	async   bool // handle() sends its follow-ups from a goroutine (set for the real node)
	nudgeMu sync.Mutex
	connect func(types.PeerID)      // asks the node to connect to a member (keys are needed to write to them)
	online  func(types.PeerID) bool // whether the node has a link to a member now

	// files: an offer that came in a group message, and the offer for a file we post
	onFile      func(origin types.PeerID, groupID string, wf wireFile)
	prepareFile func(groupID string, members []types.PeerID, path string) (filexfer.Offer, error)
	withdraw    func(id string)
	nudged      map[types.PeerID]time.Time

	// sealed messages (group_keys.go)
	keyrings   map[string]*senderkeys.Keyring
	capable    map[types.PeerID]bool // peers that understand sealed messages
	logCapable map[types.PeerID]bool // peers that speak the conversation log (version 3)
	oldPeers   map[types.PeerID]bool // peers last heard speaking a version before the log
	capLoaded  bool

	// the conversation log (conversations.go): a group's events, and its old-style messages so
	// the chat shows everything in one place
	onEvent   func(groupID string, origin types.PeerID, raw []byte)
	onMember  func(groupID string, b *convlog.MemberBody) // a membership change of ours, for the log
	onLegacy  func(groupID string, m GroupMessageDTO)
	sendEvent func(target types.PeerID, text string, raw []byte, track bool) error
	connected func(types.PeerID) bool
	onJoined  func(groupID string, peer types.PeerID) // a newcomer and the admin who let it in: sync the log now
	paced     map[string]time.Time                    // when a key request, sync request or sync answer last went
	envWrites int
	heard     map[types.PeerID]time.Time // when a group message last came from a peer

	deferred map[string][]deferredOp // changes that came before what they build on
	retrying bool

	rekeys map[string]*pendingRekey // epoch replacements waiting for the gap since the last one

	// the groups' role definitions on their logs, by group and role (group_roles.go)
	rolesMu  sync.Mutex
	roleDefs map[string]map[string]groups.Role

	// onForget: a group we left or that was deleted, for its conversation log to go
	onForget func(groupID string)

	// inviteState is an invitation as the group's log has it (its uses, a revocation)
	inviteState func(groupID, n string) (convlog.Invite, bool)
	now         func() time.Time // the clock of the key logic (tests move it)
}

func newGroupsFacade(store storage.Storage, selfID types.PeerID, selfPub types.IdentityPublicKey, sign groups.SignFunc,
	sendSilent func(types.PeerID, string) (bool, error), isContact func(types.PeerID) bool, notify func(GroupEvent), em events.Emitter) *groupsFacade {
	mgr := groups.NewGroupManager()
	if em != nil {
		mgr.SetEmitter(em)
	}
	return &groupsFacade{mgr: mgr, store: store, selfID: selfID, selfPub: selfPub, sign: sign,
		sendSilent: sendSilent, isContact: isContact, notify: notify, calls: make(map[string]GroupCall), histSent: make(map[string]time.Time), aliveSent: make(map[string]int64),
		inCall: make(map[string]map[types.PeerID]int64),
		nudged: make(map[types.PeerID]time.Time), keyrings: make(map[string]*senderkeys.Keyring),
		capable: make(map[types.PeerID]bool), paced: make(map[string]time.Time), heard: make(map[types.PeerID]time.Time),
		deferred: make(map[string][]deferredOp), rekeys: make(map[string]*pendingRekey)}
}

func kindName(t groups.GroupType) string {
	switch t {
	case groups.GroupTypePublicChannel:
		return "channel"
	case groups.GroupTypeOpenSwarm:
		return "swarm"
	default:
		return "group"
	}
}

func parseKind(kind string) (groups.GroupType, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "group", "private":
		return groups.GroupTypePrivateGroup, nil
	case "channel":
		return groups.GroupTypePublicChannel, nil
	case "swarm", "open":
		return groups.GroupTypeOpenSwarm, nil
	}
	return 0, groups.ErrInvalidType
}

func (f *groupsFacade) load() error {
	if f.loaded {
		return nil
	}
	entries, err := f.store.FindValues([]byte(groupKeyPrefix), 10000)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var rec groupRecord
		if json.Unmarshal(e.Value, &rec) != nil || rec.Genesis == nil {
			continue
		}
		if f.mgr.RestoreGroup(rec.Genesis, rec.Members) == nil && len(rec.Bans) > 0 {
			bans := make(map[types.PeerID]string, len(rec.Bans))
			for s, reason := range rec.Bans {
				if id, ok := parsePeer(s); ok {
					bans[id] = reason
				}
			}
			f.mgr.RestoreBans(rec.Genesis.GroupID, bans)
		}
	}
	f.loaded = true
	return nil
}

func (f *groupsFacade) persist(groupID string) error {
	g, err := f.mgr.GetGroup(groupID)
	if err != nil {
		return err
	}
	members, err := f.mgr.ListMembers(groupID)
	if err != nil {
		return err
	}
	rec := groupRecord{Genesis: g, Members: members}
	if bans := f.mgr.Bans(groupID); len(bans) > 0 {
		rec.Bans = make(map[string]string, len(bans))
		for id, reason := range bans {
			rec.Bans[id.String()] = reason
		}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return f.store.Set([]byte(groupKeyPrefix+groupID), raw)
}

func (f *groupsFacade) toDTO(g *groups.GroupGenesisManifest, memberCount int) GroupDTO {
	dto := GroupDTO{
		ID: g.GroupID, Title: g.Title, Description: g.Description, Kind: kindName(g.Type),
		ReadOnly: true, Creator: g.Creator.String(), CreatedAt: g.CreatedAt, Members: memberCount,
	}
	if r, err := f.mgr.GetMemberRole(g.GroupID, f.selfID); err == nil {
		dto.MyRole = r.Name
		dto.ReadOnly = r.Permissions&groups.PermSendMessages == 0
	}
	return dto
}

func (f *groupsFacade) Create(title, description, kind string) (GroupDTO, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > maxGroupTitle {
		return GroupDTO{}, errors.New("title must be 1-64 characters")
	}
	gt, err := parseKind(kind)
	if err != nil {
		return GroupDTO{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return GroupDTO{}, err
	}
	g, err := f.mgr.CreateGroupSigned(title, strings.TrimSpace(description), gt, f.selfID, f.selfPub, f.sign)
	if err != nil {
		return GroupDTO{}, err
	}
	if err := f.persist(g.GroupID); err != nil {
		return GroupDTO{}, err
	}
	// the first epoch; without it the group still works, the old way
	_, _ = f.newEpoch(g.GroupID)
	roster := f.rosterLocked(g.GroupID)
	dto := f.toDTO(g, 1)
	f.mu.Unlock()
	f.memberChange(g.GroupID, roster) // the membership starts on the log
	f.mu.Lock()
	return dto, nil
}

func (f *groupsFacade) List() ([]GroupDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return nil, err
	}
	summaries := f.mgr.ListGroups()
	out := make([]GroupDTO, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, f.toDTO(s.Genesis, s.MemberCount))
	}
	return out, nil
}

func (f *groupsFacade) Invite(groupID string) (string, error) {
	link, _, err := f.inviteFor(groupID, inviteTTL)
	return link, err
}

// inviteFor makes an invitation that lets people in for ttl, and its token (nil: the group needs
// none, an open room or a channel).
func (f *groupsFacade) inviteFor(groupID string, ttl time.Duration) (string, *inviteToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return "", nil, err
	}
	g, err := f.mgr.GetGroup(groupID)
	if err != nil {
		return "", nil, err
	}
	var tok *inviteToken
	if f.needsInvite(groupID) {
		role, err := f.mgr.GetMemberRole(groupID, f.selfID)
		if err != nil || !mayInvite(role) {
			return "", nil, errors.New("your role may not invite to this group")
		}
		if tok, err = f.newInviteTokenFor(groupID, ttl); err != nil {
			return "", nil, err
		}
	}
	raw, err := json.Marshal(groupWire{Kind: "invite", Group: groupID, Genesis: g, Members: f.inviteMembers(groupID), Inv: tok})
	if err != nil {
		return "", nil, err
	}
	return inviteScheme2 + base64.RawURLEncoding.EncodeToString(raw), tok, nil
}

// groupInviteText is a group invitation in its own form (fogroup2:…), from that form, from
// echofog://group/… (what the invitation page opens) or from its https form.
func groupInviteText(text string) string {
	t := strings.TrimSpace(text)
	// the invitation among other words (a messenger's message around it, a dot after it)
	for _, w := range strings.Fields(t) {
		lw := strings.ToLower(w)
		if strings.HasPrefix(lw, "https://") || strings.HasPrefix(lw, "http://") || strings.HasPrefix(lw, "echofog://") ||
			strings.HasPrefix(lw, inviteScheme2) || strings.HasPrefix(lw, inviteScheme) {
			t = strings.TrimRight(w, ".,;:!?)]»\"'")
			break
		}
	}
	t = strings.TrimSpace(access.FromWebLink(t))
	const app = "echofog://group/"
	if len(t) > len(app) && strings.EqualFold(t[:len(app)], app) {
		return inviteScheme2 + strings.TrimSuffix(t[len(app):], "/")
	}
	return t
}

// GroupWebLink is the https form of a group invitation: a page messengers make tappable, which
// opens the app with it.
func GroupWebLink(invite string) string {
	if !strings.HasPrefix(invite, inviteScheme2) {
		return ""
	}
	return access.WebLinkBase + "group/" + strings.TrimPrefix(invite, inviteScheme2)
}

func (f *groupsFacade) Join(invite string) (GroupDTO, error) {
	invite = groupInviteText(invite)
	var g *groups.GroupGenesisManifest
	var members []wireMember
	var tok *inviteToken
	switch {
	case strings.HasPrefix(invite, inviteScheme2):
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, inviteScheme2))
		var w groupWire
		if err != nil || json.Unmarshal(raw, &w) != nil || w.Genesis == nil {
			return GroupDTO{}, errors.New("malformed invite")
		}
		g, members, tok = w.Genesis, w.Members, w.Inv
	case strings.HasPrefix(invite, inviteScheme):
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, inviteScheme))
		var old groups.GroupGenesisManifest
		if err != nil || json.Unmarshal(raw, &old) != nil {
			return GroupDTO{}, errors.New("malformed invite")
		}
		g = &old
	default:
		return GroupDTO{}, errors.New("not a group invite")
	}
	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return GroupDTO{}, err
	}
	if err := f.mgr.ImportGroup(g); err != nil {
		f.mu.Unlock()
		return GroupDTO{}, err
	}
	_ = f.mgr.MergeMembers(g.GroupID, memberProfiles(members), true) // the inviter is trusted
	f.noteVersions(members)
	if _, err := f.mgr.GetMemberRole(g.GroupID, f.selfID); err != nil {
		if err := f.mgr.JoinGroup(g.GroupID, f.selfID, f.selfPub); err != nil {
			f.mu.Unlock()
			return GroupDTO{}, err
		}
	}
	if err := f.persist(g.GroupID); err != nil {
		f.mu.Unlock()
		return GroupDTO{}, err
	}
	known, _ := f.mgr.GetGroup(g.GroupID)
	list, _ := f.mgr.ListMembers(g.GroupID)
	dto := f.toDTO(known, len(list))
	f.mu.Unlock()

	if tok != nil {
		// the invitation we came with: our own join on the group's log names it
		_ = f.store.Set([]byte(joinViaPrefix+g.GroupID), []byte(tok.N))
	}
	// let the members know who joined (offline ones get it when they are back)
	f.broadcast(g.GroupID, groupWire{Kind: "join", Group: g.GroupID, Pub: hex.EncodeToString(f.selfPub[:]), Inv: tok})
	return dto, nil
}

func (f *groupsFacade) AddMember(groupID, pubKeyHex string) error {
	id, ident, err := peerOfPubKey(pubKeyHex)
	if err != nil {
		return err
	}

	f.mu.Lock()
	if err := f.load(); err != nil {
		f.mu.Unlock()
		return err
	}
	if err := f.mgr.AddMemberAs(groupID, f.selfID, id, ident); err != nil {
		f.mu.Unlock()
		return explain(err)
	}
	if err := f.persist(groupID); err != nil {
		f.mu.Unlock()
		return err
	}
	g, _ := f.mgr.GetGroup(groupID)
	members := f.wireMembers(groupID)
	keys := f.sendEpochs(groupID, id)
	f.mu.Unlock()

	// the newcomer gets the group and its keys, the others get to know the newcomer
	_, _ = f.sendTo(id, groupWire{Kind: "add", Group: groupID, Genesis: g, Members: members})
	for _, fn := range keys {
		fn()
	}
	newcomer := wireMember{ID: id.String(), Pub: hex.EncodeToString(ident[:]), Role: "member"}
	f.broadcast(groupID, groupWire{Kind: "snap", Group: groupID, Members: []wireMember{newcomer}}, id)
	f.memberChange(groupID, memberBody(convlog.MemberJoin, ident, ""))
	return nil
}

func (f *groupsFacade) Members(groupID string) ([]GroupMemberDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return nil, err
	}
	members, err := f.mgr.ListMembers(groupID)
	if err != nil {
		return nil, err
	}
	me, meErr := f.mgr.GetMemberRole(groupID, f.selfID)
	var assignable []groups.Role // the roles a manager may give, highest first: not the owner's
	for _, r := range f.roles(groupID) {
		if r.Rank < groups.RoleOwner.Rank {
			assignable = append(assignable, r)
		}
	}
	out := make([]GroupMemberDTO, 0, len(members))
	for _, m := range members {
		d := GroupMemberDTO{PeerID: m.PeerID.String(), Role: m.Role.Name, RoleTitle: m.Role.Title, RoleColor: m.Role.Color,
			JoinedAt: m.JoinedAt, IsMe: m.PeerID == f.selfID}
		d.Online = d.IsMe || (f.online != nil && f.online(m.PeerID))
		if meErr == nil && !d.IsMe {
			over := groups.CanAdminister(me, m.Role) // the same rules the group service applies
			d.CanKick = over && groups.HasPermission(me, groups.PermKickMembers)
			d.CanBan = over && groups.HasPermission(me, groups.PermBanMembers)
			if over && groups.HasPermission(me, groups.PermManageRoles) {
				for _, r := range assignable {
					if groups.CanAssignRole(me, r) || r.Name == m.Role.Name {
						d.Roles = append(d.Roles, r.Name)
					}
				}
			}
			d.CanTransfer = me.Rank >= groups.RoleOwner.Rank
		}
		out = append(out, d)
	}
	return out, nil
}

func msgKey(groupID string, ts int64, seq uint32) []byte {
	var b [12]byte
	binary.BigEndian.PutUint64(b[:8], uint64(ts))
	binary.BigEndian.PutUint32(b[8:], seq)
	return []byte(groupMsgPrefix + groupID + "/" + hex.EncodeToString(b[:]))
}

// mayPost checks that we may write in a group.
func (f *groupsFacade) mayPost(groupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	if _, err := f.mgr.GetGroup(groupID); err != nil {
		return err
	}
	role, err := f.mgr.GetMemberRole(groupID, f.selfID)
	if err != nil {
		return errors.New("you are not a member of this group")
	}
	if role.Permissions&groups.PermSendMessages == 0 {
		return errors.New("you cannot post in this group")
	}
	return nil
}

// isMember tells whether peer belongs to the group now (file transfers ask it).
func (f *groupsFacade) isMember(groupID string, peer types.PeerID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.load() != nil {
		return false
	}
	_, err := f.mgr.GetMemberRole(groupID, peer)
	return err == nil
}

func (f *groupsFacade) SendMessage(groupID, text string) (GroupMessageDTO, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > maxGroupText {
		return GroupMessageDTO{}, errors.New("message must be 1-8192 bytes")
	}
	if err := f.mayPost(groupID); err != nil {
		return GroupMessageDTO{}, err
	}
	return f.post(groupID, text, nil)
}

// SendFile posts a file in a group: one message carries the offer to every member, and each
// member who accepts pulls the file from this node.
func (f *groupsFacade) SendFile(groupID, channel, thread, path string) (GroupMessageDTO, error) {
	if f.prepareFile == nil {
		return GroupMessageDTO{}, errors.New("file transfer is not available")
	}
	if err := f.mayPost(groupID); err != nil {
		return GroupMessageDTO{}, err
	}
	f.mu.Lock()
	if r, ok := f.role(groupID, f.selfID); !ok || r.Permissions&groups.PermSendMedia == 0 {
		f.mu.Unlock()
		return GroupMessageDTO{}, errors.New("your role may not send files here")
	}
	list, err := f.mgr.ListMembers(groupID)
	f.mu.Unlock()
	if err != nil {
		return GroupMessageDTO{}, err
	}
	members := make([]types.PeerID, 0, len(list))
	for _, m := range list {
		members = append(members, m.PeerID)
	}
	offer, err := f.prepareFile(groupID, members, path)
	if err != nil {
		return GroupMessageDTO{}, err
	}
	wf := &wireFile{ID: offer.ID, Name: offer.Name, Size: offer.Size, Mime: offer.Mime, Root: offer.Root, Key: offer.Key,
		Channel: channel, Thread: thread}
	if thumb, w, h, ok := filexfer.Thumbnail(path); ok {
		wf.Thumb, wf.TW, wf.TH = thumb, w, h // the members see the picture before they fetch it
	}
	msg, err := f.post(groupID, offer.Name, wf)
	if err != nil || (msg.Failed > 0 && msg.Delivered+msg.Queued == 0) {
		if f.withdraw != nil {
			f.withdraw(offer.ID)
		}
		if err == nil {
			err = errors.New("the file could not be offered to anyone in the group")
		}
		return GroupMessageDTO{}, err
	}
	return msg, nil
}

// post sends a message (with a file offer, when file is set) to the group and keeps it. It is
// signed, and sealed for the members that understand sealed messages; the others get it the old way.
func (f *groupsFacade) post(groupID, text string, file *wireFile) (GroupMessageDTO, error) {
	now := time.Now().UnixMilli()
	id := newGroupMessageID()
	c := senderkeys.Content{Group: groupID, ID: id, TS: now, Sender: f.selfID.String(), Text: text}
	if file != nil {
		c.Extra, _ = json.Marshal(file)
	}
	var env *senderkeys.Envelope
	var before []func()
	if f.sign != nil && c.Sign(f.sign) == nil {
		f.mu.Lock()
		env, before = f.seal(groupID, &c)
		f.mu.Unlock()
	}
	for _, fn := range before {
		fn() // a new epoch reaches the members before the first message sealed in it
	}
	legacy := groupWire{Kind: "msg", Group: groupID, ID: id, Text: text, TS: now, File: file}
	delivered, queued, failed := f.broadcastMsg(groupID, legacy, env)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	msg := storedGroupMsg{GroupMessageDTO: GroupMessageDTO{
		ID: id, GroupID: groupID, Sender: f.selfID.String(),
		Text: text, Timestamp: now, IsMine: true, Delivered: delivered, Queued: queued, Failed: failed, Verified: true,
	}, Sig: c.Sig, FD: c.FileDigest}
	if file != nil {
		msg.File = &GroupFileDTO{ID: file.ID, Name: file.Name, Size: file.Size, Mime: file.Mime, Thumb: file.Thumb, TW: file.TW, TH: file.TH,
			Channel: file.Channel, Thread: file.Thread}
	}
	raw, _ := json.Marshal(msg)
	if err := f.store.Set(msgKey(groupID, now, f.seq), raw); err != nil {
		return GroupMessageDTO{}, err
	}
	return msg.GroupMessageDTO, nil
}

func (f *groupsFacade) Messages(groupID string, limit int) ([]GroupMessageDTO, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return nil, err
	}
	if _, err := f.mgr.GetGroup(groupID); err != nil {
		return nil, err
	}
	entries, err := f.store.FindValues([]byte(groupMsgPrefix+groupID+"/"), 100000)
	if err != nil {
		return nil, err
	}
	out := make([]GroupMessageDTO, 0, len(entries))
	for _, e := range entries {
		var m GroupMessageDTO
		if json.Unmarshal(e.Value, &m) == nil {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// groupsFacade returns the group service, making it on first use; the conversation log comes up
// with it, so a group's log traffic finds it from the first moment on.
func (n *NodeApi) groupsFacade() (*groupsFacade, error) {
	f, made, err := n.openGroups()
	if made {
		_, _ = n.convsFacade()
	}
	return f, err
}

func (n *NodeApi) openGroups() (*groupsFacade, bool, error) {
	n.groupsH.mu.Lock()
	defer n.groupsH.mu.Unlock()
	if n.groupsH.f != nil {
		return n.groupsH.f, false, nil
	}
	if n.node == nil || n.node.Storage == nil || n.node.Messenger == nil {
		return nil, false, errors.New("node not started")
	}
	pub, err := n.node.IdentityPublicKey()
	if err != nil || len(pub) != 32 {
		return nil, false, errors.New("identity not available")
	}
	var ident types.IdentityPublicKey
	copy(ident[:], pub)
	var em events.Emitter
	if n.node.EventBus != nil {
		em = n.node.EventBus
	}
	contact := n.node.IsContact
	f := newGroupsFacade(n.node.Storage, n.node.GetSelfPeerID(), ident, n.node.SignIdentity,
		n.node.Messenger.SendSilent, contact, n.publishGroupEvent, em)
	f.async = true
	f.blocked = n.node.IsBlocked
	f.online = func(id types.PeerID) bool { return n.node.Swarm != nil && n.node.Swarm.GetPeer(id) != nil }
	f.connect = func(id types.PeerID) {
		ctx, cancel := context.WithTimeout(n.node.Ctx, 20*time.Second)
		defer cancel()
		_ = n.node.EnsureConnected(ctx, id)
	}
	if files := n.node.Files; files != nil {
		f.prepareFile = files.PrepareGroup
		f.withdraw = files.Withdraw
		f.onFile = func(origin types.PeerID, groupID string, wf wireFile) {
			o := filexfer.Offer{ID: wf.ID, Group: groupID, Name: wf.Name, Size: wf.Size, Mime: wf.Mime, Root: wf.Root, Key: wf.Key}
			if _, err := files.OnOffer(origin, o); err != nil {
				n.node.Logger.Warn("FileXfer: ignoring a malformed group file offer", "group", groupID, "err", err)
			}
		}
		n.node.SetGroupMemberCheck(f.isMember)
	}
	n.node.Messenger.SetInterceptor(f.handle)
	n.hookGroupsToLog(f)
	n.groupsH.f = f
	go f.syncLoop(n.node.Ctx) // members hand each other the messages they missed
	return f, true, nil
}

func (n *NodeApi) CreateGroup(title, description, kind string) (GroupDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupDTO{}, err
	}
	return f.Create(title, description, kind)
}

func (n *NodeApi) ListGroups() ([]GroupDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return nil, err
	}
	return f.List()
}

func (n *NodeApi) GetGroupInvite(groupID string) (string, error) {
	return n.quickInvite(groupID)
}

func (n *NodeApi) JoinGroup(invite string) (GroupDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupDTO{}, err
	}
	return f.Join(invite)
}

func (n *NodeApi) AddGroupMember(groupID, pubKeyHex string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.AddMember(groupID, pubKeyHex)
}

func (n *NodeApi) ListGroupMembers(groupID string) ([]GroupMemberDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return nil, err
	}
	list, err := f.Members(groupID)
	if err != nil {
		return list, err
	}
	if cf, err := n.convsFacade(); err == nil && cf.log.HasRoster(convlog.Group(groupID)) {
		c := convlog.Group(groupID)
		for i := range list {
			if id, ok := parsePeer(list[i].PeerID); ok {
				list[i].Timeout = cf.log.TimeoutOf(c, id)
			}
			list[i].CanTimeout = list[i].CanKick // the log's rule: who may remove, a lower rank
		}
	}
	return list, nil
}

func (n *NodeApi) SendGroupMessage(groupID, text string) (GroupMessageDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupMessageDTO{}, err
	}
	if f.LogMode(groupID) {
		if _, cerr := n.convsFacade(); cerr == nil {
			m, err := n.ChatSend(ChatRef{Kind: convlog.KindGroup, ID: groupID}, SendRequest{Text: text})
			if err != nil {
				return GroupMessageDTO{}, err
			}
			return groupDTOOf(groupID, m), nil
		}
	}
	d, err := f.SendMessage(groupID, text)
	n.mirrorOwnGroupPost(groupID, d, err)
	return d, err
}

// mirrorOwnGroupPost records a message we posted the old way in the conversation log too.
func (n *NodeApi) mirrorOwnGroupPost(groupID string, d GroupMessageDTO, err error) {
	if err != nil {
		return
	}
	if cf, cerr := n.convsFacade(); cerr == nil {
		cf.mirrorGroup(groupID, d)
	}
}

// SendGroupFile posts a file in a group: in a channel of a space ("" the main one), or as a comment
// in the thread under root ("" none). Every member gets the offer; each who accepts fetches it
// from this node (the transfer's Delivered counts them).
func (n *NodeApi) SendGroupFile(groupID, channel, thread, path string) (GroupMessageDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupMessageDTO{}, err
	}
	if thread != "" {
		if _, err := convlog.ParseEventID(thread); err != nil {
			return GroupMessageDTO{}, err
		}
	}
	d, err := f.SendFile(groupID, channel, thread, path)
	n.mirrorOwnGroupPost(groupID, d, err)
	return d, err
}

// GetGroupMessages returns a group's messages, oldest first. A group on the conversation log is
// read from the log (edits applied, deleted messages left out).
func (n *NodeApi) GetGroupMessages(groupID string, limit int) ([]GroupMessageDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return nil, err
	}
	if f.LogMode(groupID) {
		if _, cerr := n.convsFacade(); cerr == nil {
			if limit <= 0 || limit > 500 {
				limit = 500
			}
			page, err := n.ChatHistory(ChatRef{Kind: convlog.KindGroup, ID: groupID}, "", limit)
			if err == nil {
				out := make([]GroupMessageDTO, 0, len(page.Messages))
				for i := len(page.Messages) - 1; i >= 0; i-- {
					if m := page.Messages[i]; !m.Deleted {
						out = append(out, groupDTOOf(groupID, m))
					}
				}
				return out, nil
			}
		}
	}
	return f.Messages(groupID, limit)
}

// CallInvite is what InviteToCall sent: the call to join, and the invitation message with its
// state, so the caller can tell whether the other side has been reached at all (see
// MessageDTO.State; OnMessageState reports when it changes).
type CallInvite struct {
	CallID      string `json:"callId"`
	Coordinator string `json:"coordinator"`
	MessageID   string `json:"messageId"`
	State       string `json:"state"`
}

func (n *NodeApi) InviteToCall(pubKeyHex string) (CallInvite, error) {
	pub, derr := hex.DecodeString(strings.TrimSpace(pubKeyHex))
	if derr != nil || len(pub) != 32 {
		return CallInvite{}, errors.New("peer key must be 32 bytes of hex")
	}
	if n.node == nil || n.node.Messenger == nil {
		return CallInvite{}, errors.New("node not started")
	}
	self := n.GetMyID()
	if self == "" {
		return CallInvite{}, errors.New("identity not available")
	}
	hash := sha256.Sum256(pub)
	var target types.PeerID
	copy(target[:], hash[:])
	callID := call.NewInviteSecret()
	text := fmt.Sprintf("%s%s/%s", CallInvitePrefix, callID, self)
	msgID, state, err := n.node.Messenger.SendChatMessageState(target, text)
	if err != nil {
		return CallInvite{}, err
	}
	slog.Info("call invitation sent", "peer", target.String()[:8], "state", state)
	return CallInvite{CallID: callID, Coordinator: self, MessageID: msgID, State: state}, nil
}

const CallInvitePrefix = "fogcall://"

// ParseCallInvite reads the call invitation that InviteToCall sends as a chat message:
// "fogcall://<call id>/<coordinator peer id>".
func ParseCallInvite(text string) (callID, coordinator string, ok bool) {
	rest, found := strings.CutPrefix(text, CallInvitePrefix)
	if !found {
		return "", "", false
	}
	callID, coordinator, found = strings.Cut(rest, "/")
	if !found || callID == "" || coordinator == "" {
		return "", "", false
	}
	return callID, coordinator, true
}

// SubscribeGroupEvents calls cb whenever something happens in a group: a message arrived, members
// changed, somebody started a call, the node was added to or removed from a group.
func (n *NodeApi) SubscribeGroupEvents(cb func(GroupEvent)) {
	n.groupsH.mu.Lock()
	n.groupsH.subs = append(n.groupsH.subs, cb)
	n.groupsH.mu.Unlock()
}

func (n *NodeApi) publishGroupEvent(ev GroupEvent) {
	n.groupsH.mu.Lock()
	subs := append([]func(GroupEvent){}, n.groupsH.subs...)
	n.groupsH.mu.Unlock()
	for _, cb := range subs {
		go cb(ev)
	}
}

// SetGroupRole gives a member the role admin, moderator, member or restricted.
func (n *NodeApi) SetGroupRole(groupID, memberID, role string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.SetRole(groupID, memberID, role)
}

// RemoveGroupMember kicks a member out of the group, or bans them.
func (n *NodeApi) RemoveGroupMember(groupID, memberID string, ban bool) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.Remove(groupID, memberID, ban)
}

// LeaveGroup leaves the group and forgets it on this node.
func (n *NodeApi) LeaveGroup(groupID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.Leave(groupID)
}

// StartGroupCall tells the members that a call opens and returns what the caller joins with.
func (n *NodeApi) StartGroupCall(groupID string) (GroupCall, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupCall{}, err
	}
	return f.StartCall(groupID, "")
}

// KeepGroupCallAlive tells the group the call is still going; call it periodically while in it.
func (n *NodeApi) KeepGroupCallAlive(groupID, callID, coordinator string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.KeepCallAlive(groupID, "", callID, coordinator)
}

// ActiveGroupCall is the call going on in the group, if there is one.
func (n *NodeApi) ActiveGroupCall(groupID string) (GroupCall, bool) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupCall{}, false
	}
	return f.ActiveCall(groupID, "")
}

// TransferGroupOwnership hands the group to another member; the owner becomes an admin.
func (n *NodeApi) TransferGroupOwnership(groupID, memberID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.TransferOwnership(groupID, memberID)
}

// DeleteGroup removes the group for everybody. Only the owner can.
func (n *NodeApi) DeleteGroup(groupID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.Delete(groupID)
}

// EndGroupCall tells the group that its call is over. Call it when the last person leaves.
func (n *NodeApi) EndGroupCall(groupID, callID string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.EndCall(groupID, "", callID)
}
