package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The calls of groups and of their voice rooms (T11.4). Who is in a call is not in the log: every
// member that takes part repeats a short announcement over chat/2 (an end-to-end message; the call's
// secret travels in it, as it did in v1), and a call nobody announced for callAliveFor is over. The
// announcement is a text message whose metadata starts with callPrefix; the app takes it before it is
// shown as a message (OnIncoming).

const (
	callPrefix    = "fogcall2:"
	maxCallWire   = 1 << 10
	maxCallID     = 128
	maxGroupCalls = 512
	maxInCall     = 256
	// callAliveEvery is how often a member in a call repeats the announcement; callAliveFor is how long a
	// call lasts without one.
	callAliveEvery = 45 * time.Second
	callAliveFor   = 150 * time.Second
)

// GroupCallInfo is a call going on in a group.
type GroupCallInfo struct {
	Room        string // the voice room ("" the group's own call)
	CallID      string
	Coordinator string // the peer ID that coordinates the call's media
	From        string // hex node ID of who started it
	Members     []string
	StartedAt   int64
	LastSeen    int64
}

type callWire struct {
	V int    `json:"v"`
	K string `json:"k"` // call, alive, end, leave
	G string `json:"g"`
	R string `json:"r,omitempty"`
	C string `json:"c"`
	O string `json:"o,omitempty"` // the coordinator
}

type gcall struct {
	GroupCallInfo
	in        map[string]int64 // hex node ID -> when it last announced
	aliveSent time.Time
}

// callBook is what this node knows of the calls of its groups.
type callBook struct {
	mu sync.Mutex
	m  map[string]*gcall
}

func callKey(group, room string) string { return group + "\x00" + room }

func (a *App) nowT() time.Time { return a.cfg.Clock.Now() }

// callEnv checks that this node may call in the group (and its room) and returns the group's log.
func (a *App) callEnv(group, room string) (Convs, convlog.Conv, error) {
	_, cv, c, _, err := a.grp(group)
	if err != nil {
		return nil, convlog.Conv{}, err
	}
	l := cv.Log()
	if !l.Participant(c, l.Self()) {
		return nil, convlog.Conv{}, ErrNoGroup
	}
	if room != "" {
		ch, ok := l.ChannelOf(c, room)
		if !ok || ch.Removed || !ch.Voice {
			return nil, convlog.Conv{}, ErrBadGroup
		}
	}
	if !l.RightsIn(c, l.Self(), room).Voice {
		return nil, convlog.Conv{}, ErrNoRight
	}
	return cv, c, nil
}

func (a *App) selfNodeHex() string { return hex.EncodeToString(a.cfg.Self[:]) }

// others lists the nodes of the group's members but this one.
func (a *App) others(c convlog.Conv) [][32]byte {
	if a.cfg.Groups == nil {
		return nil
	}
	var out [][32]byte
	for _, pub := range a.cfg.Groups.Members(c) {
		if n := convsync.NodeOf(pub); n != a.cfg.Self {
			out = append(out, n)
		}
	}
	return out
}

func (a *App) callSend(ctx context.Context, to [][32]byte, w callWire) {
	if a.cfg.Chat == nil || len(to) == 0 {
		return
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return
	}
	for _, n := range to {
		_, _ = a.cfg.Chat.Send(ctx, n, &chatpb.PersonalMessageProto{Metadata: callPrefix + string(raw)})
	}
}

// live returns a call that has not lapsed; a lapsed one is forgotten. Call with mu held.
func (b *callBook) live(key string, now time.Time) *gcall {
	g, ok := b.m[key]
	if !ok {
		return nil
	}
	if now.Sub(time.Unix(g.LastSeen, 0)) > callAliveFor {
		delete(b.m, key)
		return nil
	}
	return g
}

func (b *callBook) put(key string, g *gcall, now time.Time) bool {
	if b.m == nil {
		b.m = map[string]*gcall{}
	}
	if _, ok := b.m[key]; !ok && len(b.m) >= maxGroupCalls {
		for k, old := range b.m { // the lapsed go first
			if now.Sub(time.Unix(old.LastSeen, 0)) > callAliveFor {
				delete(b.m, k)
			}
		}
		if len(b.m) >= maxGroupCalls {
			return false
		}
	}
	b.m[key] = g
	return true
}

func (g *gcall) snapshot() GroupCallInfo {
	out := g.GroupCallInfo
	out.Members = make([]string, 0, len(g.in))
	for n := range g.in {
		out.Members = append(out.Members, n)
	}
	sort.Strings(out.Members)
	return out
}

// StartGroupCall starts the call of a group, or returns the one going on.
func (a *App) StartGroupCall(ctx context.Context, group string) (GroupCallInfo, error) {
	return a.startCall(ctx, group, "")
}

// StartRoomCall starts the call of a voice room, or returns the one going on. A room's call never rings.
func (a *App) StartRoomCall(ctx context.Context, group, room string) (GroupCallInfo, error) {
	if room == "" {
		return GroupCallInfo{}, ErrBadGroup
	}
	return a.startCall(ctx, group, room)
}

func (a *App) startCall(ctx context.Context, group, room string) (GroupCallInfo, error) {
	cv, c, err := a.callEnv(group, room)
	if err != nil {
		return GroupCallInfo{}, err
	}
	now := a.nowT()
	key := callKey(group, room)
	a.gc.mu.Lock()
	if g := a.gc.live(key, now); g != nil {
		out := g.snapshot()
		a.gc.mu.Unlock()
		return out, nil
	}
	self := a.selfNodeHex()
	coord := cv.Log().Self().String()
	g := &gcall{GroupCallInfo: GroupCallInfo{Room: room, CallID: call.NewInviteSecret(), Coordinator: coord, From: self,
		StartedAt: now.Unix(), LastSeen: now.Unix()}, in: map[string]int64{self: now.Unix()}, aliveSent: now}
	if !a.gc.put(key, g, now) {
		a.gc.mu.Unlock()
		return GroupCallInfo{}, ErrTooManyTransfers
	}
	out := g.snapshot()
	a.gc.mu.Unlock()
	a.callSend(ctx, a.others(c), callWire{V: 1, K: "call", G: group, R: room, C: out.CallID, O: out.Coordinator})
	return out, nil
}

// ActiveGroupCall is the call going on in a group or one of its rooms.
func (a *App) ActiveGroupCall(group, room string) (GroupCallInfo, bool) {
	a.gc.mu.Lock()
	defer a.gc.mu.Unlock()
	g := a.gc.live(callKey(group, room), a.nowT())
	if g == nil {
		return GroupCallInfo{}, false
	}
	return g.snapshot(), true
}

// KeepGroupCallAlive tells the others that the call goes on and that this node is in it. The UI calls
// it every callAliveEvery while the user is in the call; announcements closer together are skipped.
func (a *App) KeepGroupCallAlive(ctx context.Context, group, room, callID, coordinator string) error {
	if callID == "" || coordinator == "" || len(callID) > maxCallID {
		return ErrBadGroup
	}
	_, c, err := a.callEnv(group, room)
	if err != nil {
		return err
	}
	now := a.nowT()
	key := callKey(group, room)
	self := a.selfNodeHex()
	a.gc.mu.Lock()
	g := a.gc.live(key, now)
	if g == nil || g.CallID != callID {
		g = &gcall{GroupCallInfo: GroupCallInfo{Room: room, CallID: callID, Coordinator: coordinator, From: self, StartedAt: now.Unix()},
			in: map[string]int64{}}
		if !a.gc.put(key, g, now) {
			a.gc.mu.Unlock()
			return ErrTooManyTransfers
		}
	}
	g.LastSeen = now.Unix()
	g.in[self] = now.Unix()
	if !g.aliveSent.IsZero() && now.Sub(g.aliveSent) >= 0 && now.Sub(g.aliveSent) < callAliveEvery/2 {
		a.gc.mu.Unlock()
		return nil
	}
	g.aliveSent = now
	a.gc.mu.Unlock()
	a.callSend(ctx, a.others(c), callWire{V: 1, K: "alive", G: group, R: room, C: callID, O: coordinator})
	return nil
}

// EndGroupCall ends the call of a group or a room for everybody.
func (a *App) EndGroupCall(ctx context.Context, group, room, callID string) error {
	_, c, err := a.callEnv(group, room)
	if err != nil {
		return err
	}
	key := callKey(group, room)
	a.gc.mu.Lock()
	g := a.gc.live(key, a.nowT())
	if g == nil || callID != "" && g.CallID != callID {
		a.gc.mu.Unlock()
		return ErrNoCall
	}
	id := g.CallID
	delete(a.gc.m, key)
	a.gc.mu.Unlock()
	a.callSend(ctx, a.others(c), callWire{V: 1, K: "end", G: group, R: room, C: id})
	return nil
}

// LeaveRoomCall says this node left a room whose call goes on: it is no longer among who is in it.
func (a *App) LeaveRoomCall(ctx context.Context, group, room, callID string) error {
	if room == "" {
		return ErrBadGroup
	}
	_, c, err := a.callEnv(group, room)
	if err != nil {
		return err
	}
	a.gc.mu.Lock()
	if g := a.gc.live(callKey(group, room), a.nowT()); g != nil && g.CallID == callID {
		delete(g.in, a.selfNodeHex())
	}
	a.gc.mu.Unlock()
	a.callSend(ctx, a.others(c), callWire{V: 1, K: "leave", G: group, R: room, C: callID})
	return nil
}

// ForgetGroupCalls drops what is known of the calls of the groups.
func (a *App) ForgetGroupCalls() {
	a.gc.mu.Lock()
	a.gc.m = nil
	a.gc.mu.Unlock()
}

func (a *App) isCallWire(pm *chatpb.PersonalMessageProto) bool {
	return strings.HasPrefix(pm.GetMetadata(), callPrefix)
}

// OnGroupCall takes an announcement of a call from an authenticated account. It reports whether the
// message was one, so that it is not shown as a chat message; every refusal is silence: a sender that
// is no member (removed, banned, a stranger), a right it does not have, a group or a room that is not
// there, a call id that is none.
func (a *App) OnGroupCall(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	md := pm.GetMetadata()
	if !strings.HasPrefix(md, callPrefix) {
		return false
	}
	if len(md) > maxCallWire || a.cfg.Groups == nil || a.cfg.Convs == nil || a.cfg.PubOf == nil {
		return true
	}
	var w callWire
	if json.Unmarshal([]byte(md[len(callPrefix):]), &w) != nil || w.V != 1 || w.C == "" || len(w.C) > maxCallID || len(w.O) > maxCallID {
		return true
	}
	switch w.K {
	case "call", "alive", "end", "leave":
	default:
		return true
	}
	if _, ok := a.cfg.Groups.rec(w.G); !ok {
		return true
	}
	pub, err := a.cfg.PubOf(ctx, from)
	if err != nil || convsync.NodeOf(pub) != from {
		return true
	}
	c := convlog.Group(w.G)
	l := a.cfg.Convs.Log()
	peer := types.PeerPubKeyToID(types.PeerPublicKey(pub))
	if l.Open(c) != nil || !l.Participant(c, l.Self()) || !l.Participant(c, peer) {
		return true
	}
	if w.R != "" {
		ch, ok := l.ChannelOf(c, w.R)
		if !ok || ch.Removed || !ch.Voice {
			return true
		}
	}
	if !l.RightsIn(c, peer, w.R).Voice {
		return true
	}
	if a.blockedNode(from) {
		return true // a blocked contact does not ring, whatever group it speaks in
	}
	now := a.nowT()
	key := callKey(w.G, w.R)
	fromHex := hex.EncodeToString(from[:])
	var ev *GroupEvent
	a.gc.mu.Lock()
	g := a.gc.live(key, now)
	switch w.K {
	case "call", "alive":
		if w.O == "" || !coordinatorIn(l, c, w.O) {
			a.gc.mu.Unlock()
			return true
		}
		fresh := g == nil || g.CallID != w.C
		if fresh {
			g = &gcall{GroupCallInfo: GroupCallInfo{Room: w.R, CallID: w.C, Coordinator: w.O, From: fromHex, StartedAt: now.Unix()}, in: map[string]int64{}}
			if !a.gc.put(key, g, now) {
				a.gc.mu.Unlock()
				return true
			}
			kind := "callactive" // a call that is under way, or a room's: never rings
			if w.K == "call" && w.R == "" {
				kind = "call"
			}
			ev = &GroupEvent{Kind: kind, Group: w.G, From: fromHex, CallID: w.C, Coordinator: w.O, Room: w.R}
		}
		g.LastSeen = now.Unix()
		if len(g.in) < maxInCall || g.in[fromHex] != 0 {
			g.in[fromHex] = now.Unix()
		}
	case "leave":
		if g != nil && g.CallID == w.C {
			delete(g.in, fromHex)
		}
	case "end":
		if g != nil && g.CallID == w.C {
			delete(a.gc.m, key)
			ev = &GroupEvent{Kind: "callend", Group: w.G, From: fromHex, CallID: w.C, Room: w.R}
		}
	}
	a.gc.mu.Unlock()
	if ev != nil {
		a.emitGroup(*ev)
	}
	return true
}

// coordinatorIn reports whether the peer ID a call names as its coordinator is a member of the group.
func coordinatorIn(l *convlog.Log, c convlog.Conv, peer string) bool {
	raw, err := hex.DecodeString(peer)
	if err != nil || len(raw) != 32 {
		return false
	}
	return l.Participant(c, types.PeerID(raw))
}
