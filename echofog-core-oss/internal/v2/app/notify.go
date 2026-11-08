package app

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Notification modes of a channel as the UI names them.
const (
	NotifyAll      = "all"
	NotifyMentions = "mentions"
	NotifyNone     = "none"
)

// How loud a new message is for this user.
const (
	AlertNone  = "none"
	AlertQuiet = "quiet"
	AlertLoud  = "loud"
)

// ErrBadMode reports a notification mode that is not one of the three.
var ErrBadMode = errors.New("app: unknown notification mode")

func (a *App) prefs() (*convsync.Prefs, error) {
	if a.cfg.Prefs == nil {
		return nil, ErrChatOff
	}
	return a.cfg.Prefs, nil
}

// ChatNotify tells what a chat's channel tells the user about. The setting is local to this device.
func (a *App) ChatNotify(ref ChatRef) (string, error) {
	p, err := a.prefs()
	if err != nil {
		return "", err
	}
	c, err := a.resolve(context.Background(), ref, false)
	if err != nil {
		return "", err
	}
	switch p.Mode(c.ID, ref.Channel) {
	case convsync.ModeMentions:
		return NotifyMentions, nil
	case convsync.ModeNone:
		return NotifyNone, nil
	}
	return NotifyAll, nil
}

// SetChatNotify sets what a chat's channel tells the user about.
func (a *App) SetChatNotify(ref ChatRef, mode string) error {
	p, err := a.prefs()
	if err != nil {
		return err
	}
	var m convsync.Mode
	switch mode {
	case NotifyAll:
		m = convsync.ModeAll
	case NotifyMentions:
		m = convsync.ModeMentions
	case NotifyNone:
		m = convsync.ModeNone
	default:
		return ErrBadMode
	}
	c, err := a.resolve(context.Background(), ref, false)
	if err != nil {
		return err
	}
	return p.SetMode(c.ID, ref.Channel, m)
}

// ChatAlert says how loud a message of a chat is for this user: a message that names the user, and a
// reply in a followed thread, is loud whatever the mode; the rest follows the channel's mode; the
// user's own messages are none.
func (a *App) ChatAlert(m convlog.Message) string {
	p, err := a.prefs()
	if err != nil {
		return AlertQuiet
	}
	switch p.Alert(m) {
	case convsync.AlertLoud:
		return AlertLoud
	case convsync.AlertQuiet:
		// a silence for a term works like the mode "nothing": being named still counts, the rest is quiet no more
		if id, err := convlog.ParseConvID(m.Conv); err == nil && a.mutedUntil(p, id, m.Channel) > 0 {
			return AlertNone
		}
		return AlertQuiet
	}
	return AlertNone
}

// ChatFollow follows the thread under message root of a chat, or leaves it.
func (a *App) ChatFollow(ref ChatRef, root string, on bool) error {
	p, err := a.prefs()
	if err != nil {
		return err
	}
	id, err := requireEvent(root)
	if err != nil {
		return err
	}
	c, err := a.resolve(context.Background(), ref, false)
	if err != nil {
		return err
	}
	return p.Follow(c.ID, id, on)
}

// ChatFollowing lists the roots of the threads the user follows in a chat (a thread can be followed
// before it has a reply).
func (a *App) ChatFollowing(ref ChatRef) ([]string, error) {
	p, err := a.prefs()
	if err != nil {
		return nil, err
	}
	c, err := a.resolve(context.Background(), ref, false)
	if err != nil {
		return nil, err
	}
	return p.Followed(c.ID), nil
}

// mentionsOf resolves the node IDs a message names into the log's peer IDs. A node whose key the
// directory does not know cannot be named.
func (a *App) mentionsOf(ctx context.Context, req SendReq) (convlog.Mentions, error) {
	out := convlog.Mentions{All: req.MentionAll}
	if len(req.Mentions) > convlog.MaxMentions {
		return out, ErrBadMessage
	}
	for _, h := range req.Mentions {
		node, ok := nodeBytes(h)
		if !ok || a.cfg.PubOf == nil {
			return out, ErrBadMessage
		}
		pub, err := a.cfg.PubOf(ctx, node)
		if err != nil || convsync.NodeOf(pub) != node {
			return out, ErrBadMessage
		}
		out.Who = append(out.Who, types.PeerPubKeyToID(pub))
	}
	return out, nil
}

// NewVoiceRooms turns the live roster of the voice rooms into the app's VoiceRooms: who is in a room
// comes from the sessions of the group's members, never from the log.
func NewVoiceRooms(v *convsync.VoiceRoster, b *GroupBook) VoiceRooms { return liveRooms{v, b} }

type liveRooms struct {
	v *convsync.VoiceRoster
	b *GroupBook
}

func (r liveRooms) Roster(group, channel string) []VoiceMember {
	if r.v == nil || r.b == nil {
		return nil
	}
	if _, ok := r.b.rec(group); !ok {
		return nil
	}
	list, _ := r.v.Who(convlog.Group(group), channel)
	out := make([]VoiceMember, 0, len(list))
	for _, p := range list {
		n := convsync.NodeOf(p.Pub)
		out = append(out, VoiceMember{Node: hex.EncodeToString(n[:]), Muted: !p.State.Mic, Camera: p.State.Cam, Stream: p.State.Stream})
	}
	return out
}

// GroupEvent is something that happened in a group that the screen has to hear of. Kind is
// "requests" (a request to join waits or was decided; From names a new one), "added" (this node was let
// in), "removed" (this node was removed or banned; Reason says which) or "history" (earlier messages
// came in at once).
type GroupEvent struct {
	Kind, Group, Title, From, Reason string
	// a call: its secret, who coordinates it, and the voice room it is in ("" the group's own call)
	CallID, Coordinator, Room string
}

// groupEvents holds the subscribers of group events.
type groupEvents struct {
	mu   sync.Mutex
	subs map[int]func(GroupEvent)
	next int
	// addedAt: when this node was let into a group, for the window in which its history arrives
	addedAt map[string]time.Time
}

// historyWindow is how long after being let in a group the messages of others count as its earlier history.
const historyWindow = 30 * time.Second

func (a *App) catchingUp(group string) bool {
	a.gev.mu.Lock()
	defer a.gev.mu.Unlock()
	t, ok := a.gev.addedAt[group]
	return ok && a.cfg.Clock.Now().Sub(t) < historyWindow
}

// SubscribeGroupEvents calls fn for every such event until cancel is called.
func (a *App) SubscribeGroupEvents(fn func(GroupEvent)) (cancel func()) {
	a.gev.mu.Lock()
	defer a.gev.mu.Unlock()
	if a.gev.subs == nil {
		a.gev.subs = map[int]func(GroupEvent){}
	}
	a.gev.next++
	id := a.gev.next
	a.gev.subs[id] = fn
	return func() {
		a.gev.mu.Lock()
		delete(a.gev.subs, id)
		a.gev.mu.Unlock()
	}
}

func (a *App) emitGroup(ev GroupEvent) {
	if ev.Title == "" {
		if g, err := a.GroupInfoOf(ev.Group); err == nil {
			ev.Title = g.Title
		}
	}
	a.gev.mu.Lock()
	subs := make([]func(GroupEvent), 0, len(a.gev.subs))
	for _, f := range a.gev.subs {
		subs = append(subs, f)
	}
	a.gev.mu.Unlock()
	for _, f := range subs {
		f(ev)
	}
}
