package node

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/services/groups"
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
	PeerID   string `json:"peerId"`
	Role     string `json:"role"`
	JoinedAt int64  `json:"joinedAt"`
	IsMe     bool   `json:"isMe"`
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
}

// GroupFileDTO is the file of a group message; its transfer (GetFileTransfer with ID) has the state.
type GroupFileDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime,omitempty"`
}

type groupsHolder struct {
	mu   sync.Mutex
	f    *groupsFacade
	subs []func(GroupEvent)
}

type groupRecord struct {
	Genesis *groups.GroupGenesisManifest `json:"genesis"`
	Members []groups.MemberProfile       `json:"members"`
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
	histSent   map[string]time.Time // when history was last handed to whom
	aliveSent  map[string]int64     // group -> when we last announced our call there (unix s)

	async   bool // handle() sends its follow-ups from a goroutine (set for the real node)
	nudgeMu sync.Mutex
	connect func(types.PeerID) // asks the node to connect to a member (keys are needed to write to them)

	// files: an offer that came in a group message, and the offer for a file we post
	onFile      func(origin types.PeerID, groupID string, wf wireFile)
	prepareFile func(groupID string, members []types.PeerID, path string) (filexfer.Offer, error)
	withdraw    func(id string)
	nudged      map[types.PeerID]time.Time
}

func newGroupsFacade(store storage.Storage, selfID types.PeerID, selfPub types.IdentityPublicKey, sign groups.SignFunc,
	sendSilent func(types.PeerID, string) (bool, error), isContact func(types.PeerID) bool, notify func(GroupEvent), em events.Emitter) *groupsFacade {
	mgr := groups.NewGroupManager()
	if em != nil {
		mgr.SetEmitter(em)
	}
	return &groupsFacade{mgr: mgr, store: store, selfID: selfID, selfPub: selfPub, sign: sign,
		sendSilent: sendSilent, isContact: isContact, notify: notify, calls: make(map[string]GroupCall), histSent: make(map[string]time.Time), aliveSent: make(map[string]int64),
		nudged: make(map[types.PeerID]time.Time)}
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
		_ = f.mgr.RestoreGroup(rec.Genesis, rec.Members)
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
	raw, err := json.Marshal(groupRecord{Genesis: g, Members: members})
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
	return f.toDTO(g, 1), nil
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return "", err
	}
	g, err := f.mgr.GetGroup(groupID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(groupWire{Kind: "invite", Group: groupID, Genesis: g, Members: f.wireMembers(groupID)})
	if err != nil {
		return "", err
	}
	return inviteScheme2 + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (f *groupsFacade) Join(invite string) (GroupDTO, error) {
	invite = strings.TrimSpace(invite)
	var g *groups.GroupGenesisManifest
	var members []wireMember
	switch {
	case strings.HasPrefix(invite, inviteScheme2):
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, inviteScheme2))
		var w groupWire
		if err != nil || json.Unmarshal(raw, &w) != nil || w.Genesis == nil {
			return GroupDTO{}, errors.New("malformed invite")
		}
		g, members = w.Genesis, w.Members
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

	// let the members know who joined (offline ones get it when they are back)
	f.broadcast(g.GroupID, groupWire{Kind: "join", Group: g.GroupID, Pub: hex.EncodeToString(f.selfPub[:])})
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
	f.mu.Unlock()

	// the newcomer gets the group, the others get to know the newcomer
	_, _ = f.sendTo(id, groupWire{Kind: "add", Group: groupID, Genesis: g, Members: members})
	newcomer := wireMember{ID: id.String(), Pub: hex.EncodeToString(ident[:]), Role: "member"}
	f.broadcast(groupID, groupWire{Kind: "snap", Group: groupID, Members: []wireMember{newcomer}}, id)
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
	out := make([]GroupMemberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, GroupMemberDTO{PeerID: m.PeerID.String(), Role: m.Role.Name, JoinedAt: m.JoinedAt, IsMe: m.PeerID == f.selfID})
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
func (f *groupsFacade) SendFile(groupID, path string) (GroupMessageDTO, error) {
	if f.prepareFile == nil {
		return GroupMessageDTO{}, errors.New("file transfer is not available")
	}
	if err := f.mayPost(groupID); err != nil {
		return GroupMessageDTO{}, err
	}
	f.mu.Lock()
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
	msg, err := f.post(groupID, offer.Name, &wireFile{ID: offer.ID, Name: offer.Name, Size: offer.Size, Mime: offer.Mime, Root: offer.Root, Key: offer.Key})
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

// post sends a message (with a file offer, when file is set) to the group and keeps it.
func (f *groupsFacade) post(groupID, text string, file *wireFile) (GroupMessageDTO, error) {
	now := time.Now().UnixMilli()
	id := newGroupMessageID()
	delivered, queued, failed := f.broadcast(groupID, groupWire{Kind: "msg", Group: groupID, ID: id, Text: text, TS: now, File: file})

	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	msg := GroupMessageDTO{
		ID: id, GroupID: groupID, Sender: f.selfID.String(),
		Text: text, Timestamp: now, IsMine: true, Delivered: delivered, Queued: queued, Failed: failed,
	}
	if file != nil {
		msg.File = &GroupFileDTO{ID: file.ID, Name: file.Name, Size: file.Size, Mime: file.Mime}
	}
	raw, _ := json.Marshal(msg)
	if err := f.store.Set(msgKey(groupID, now, f.seq), raw); err != nil {
		return GroupMessageDTO{}, err
	}
	return msg, nil
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

func (n *NodeApi) groupsFacade() (*groupsFacade, error) {
	n.groupsH.mu.Lock()
	defer n.groupsH.mu.Unlock()
	if n.groupsH.f != nil {
		return n.groupsH.f, nil
	}
	if n.node == nil || n.node.Storage == nil || n.node.Messenger == nil {
		return nil, errors.New("node not started")
	}
	pub, err := n.node.IdentityPublicKey()
	if err != nil || len(pub) != 32 {
		return nil, errors.New("identity not available")
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
	n.groupsH.f = f
	return f, nil
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
	f, err := n.groupsFacade()
	if err != nil {
		return "", err
	}
	return f.Invite(groupID)
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
	return f.Members(groupID)
}

func (n *NodeApi) SendGroupMessage(groupID, text string) (GroupMessageDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupMessageDTO{}, err
	}
	return f.SendMessage(groupID, text)
}

// SendGroupFile posts a file in a group. Every member gets the offer; each who accepts fetches it
// from this node (the transfer's Delivered counts them).
func (n *NodeApi) SendGroupFile(groupID, path string) (GroupMessageDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupMessageDTO{}, err
	}
	return f.SendFile(groupID, path)
}

func (n *NodeApi) GetGroupMessages(groupID string, limit int) ([]GroupMessageDTO, error) {
	f, err := n.groupsFacade()
	if err != nil {
		return nil, err
	}
	return f.Messages(groupID, limit)
}

func (n *NodeApi) InviteToCall(pubKeyHex string) (callID string, coordinator string, err error) {
	pub, derr := hex.DecodeString(strings.TrimSpace(pubKeyHex))
	if derr != nil || len(pub) != 32 {
		return "", "", errors.New("peer key must be 32 bytes of hex")
	}
	if n.node == nil || n.node.Messenger == nil {
		return "", "", errors.New("node not started")
	}
	self := n.GetMyID()
	if self == "" {
		return "", "", errors.New("identity not available")
	}
	callID = call.NewInviteSecret()
	text := fmt.Sprintf("%s%s/%s", CallInvitePrefix, callID, self)
	if err := n.SendMessage(pubKeyHex, text); err != nil {
		return "", "", err
	}
	return callID, self, nil
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
	return f.StartCall(groupID)
}

// KeepGroupCallAlive tells the group the call is still going; call it periodically while in it.
func (n *NodeApi) KeepGroupCallAlive(groupID, callID, coordinator string) error {
	f, err := n.groupsFacade()
	if err != nil {
		return err
	}
	return f.KeepCallAlive(groupID, callID, coordinator)
}

// ActiveGroupCall is the call going on in the group, if there is one.
func (n *NodeApi) ActiveGroupCall(groupID string) (GroupCall, bool) {
	f, err := n.groupsFacade()
	if err != nil {
		return GroupCall{}, false
	}
	return f.ActiveCall(groupID)
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
	return f.EndCall(groupID, callID)
}
