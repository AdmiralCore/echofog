package app

import (
	"context"
	"encoding/hex"
	"sort"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Receiver is the receiving side of the conversation engine (*convsync.Engine).
type Receiver interface {
	Receive(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool
}

// OnIncoming takes every message chat/2 delivers from an authenticated account and gives it to the
// part that owns it: a file offer or a voice message to the files, a log event or a sync message to the
// conversation engine, a join request to the groups. It reports whether the message was one of these,
// so that nothing is shown twice; every refusal is silence.
//
// A blocked contact is heard in nothing that is its own with us: its direct messages, files, voice
// messages, join requests and syncs are dropped. The log of a group is shared with the others, so its
// events are taken from a blocked member as from anybody (dropping them would only make the logs differ);
// what the member may do in the group is the group's business.
func (a *App) OnIncoming(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	if pm == nil {
		return false
	}
	blocked := a.blockedNode(from)
	switch pm.GetType() {
	case chatpb.PersonalMessageType_MSG_TYPE_FILE:
		if !blocked {
			_ = a.OnFileOffer(from, a.groupOfSender(ctx, from), pm)
		}
		return true
	case chatpb.PersonalMessageType_MSG_TYPE_VOICE:
		if !blocked {
			_, _ = a.OnVoice(from, pm)
		}
		return true
	}
	if a.isCallWire(pm) {
		return a.OnGroupCall(ctx, from, pm)
	}
	if blocked {
		switch raw := pm.GetConvEvent(); {
		case len(pm.GetConvSync()) > 0:
			return true
		case len(raw) > 0:
			if cid, ok := convlog.PeekConv(raw); !ok || !a.isGroupConv(cid) {
				return true
			}
		default:
			return a.isJoin(pm) // a join request is the blocked contact's own business with us
		}
	}
	if r, ok := a.cfg.Convs.(Receiver); ok && r.Receive(ctx, from, pm) {
		return true
	}
	return a.OnGroupMessage(ctx, from, pm)
}

func (a *App) isJoin(pm *chatpb.PersonalMessageProto) bool {
	return len(pm.GetMetadata()) >= len(joinPrefix) && pm.GetMetadata()[:len(joinPrefix)] == joinPrefix
}

func (a *App) isGroupConv(cid convlog.ConvID) bool {
	if a.cfg.Groups == nil {
		return false
	}
	_, ok := a.cfg.Groups.Conv(cid)
	return ok
}

// blockedNode reports whether node is a contact that is blocked.
func (a *App) blockedNode(node [32]byte) bool {
	return a.contactOK(node) == ErrBlocked
}

// groupOfSender names the group a file offer from node is taken in: "" for a contact (a direct offer),
// otherwise a group this node and the sender are both in, found in this node's own roster. The sender
// never names it (the offer has no such field), so it cannot claim a group it is not in. Nobody known
// gives "", which OnFileOffer refuses.
func (a *App) groupOfSender(ctx context.Context, node [32]byte) string {
	if a.contactOK(node) == nil {
		return ""
	}
	if a.cfg.Groups == nil || a.cfg.PubOf == nil {
		return ""
	}
	pub, err := a.cfg.PubOf(ctx, node)
	if err != nil {
		return ""
	}
	shared := a.cfg.Groups.SharedGroups(pub)
	if len(shared) == 0 {
		return ""
	}
	ids := make([]string, 0, len(shared))
	for _, c := range shared {
		ids = append(ids, c.Group)
	}
	sort.Strings(ids)
	return ids[0]
}

// ChatEvents is what the runtime hands to chat.ServiceConfig.Events: messages go to OnIncoming, the
// status of an own message to the tracker st (it may be nil), knocks and their answers to OnControl.
func (a *App) ChatEvents(st *ChatStates) chat.Events {
	return chat.Events{
		Message: func(in chat.Incoming) { a.OnIncoming(context.Background(), in.From, in.Message) },
		Status: func(id [16]byte, s chat.State) {
			if st != nil {
				st.OnStatus(id, s)
			}
		},
		Control: a.OnControl,
	}
}

// NodeOfPeer turns the peer id a message names its author by (the v1 id of the account key) into the
// node ID the contacts and the members are known by. It knows the accounts the engine has met, the keys
// saved from earlier runs and the keys the groups' logs name; nobody else.
func (a *App) NodeOfPeer(peer string) (string, bool) {
	raw, err := hex.DecodeString(peer)
	if err != nil || len(raw) != 32 {
		return "", false
	}
	p := types.PeerID(raw)
	cv, err := a.convs()
	if err != nil {
		return "", false
	}
	if p == cv.Log().Self() {
		return hex.EncodeToString(a.cfg.Self[:]), true
	}
	pub, ok := cv.PubOfPeer(p)
	if !ok {
		pub, ok = a.savedPub(p)
	}
	if !ok && a.cfg.Groups != nil {
		pub, ok = a.cfg.Groups.pubOfPeer(p)
	}
	if !ok {
		return "", false
	}
	n := convsync.NodeOf(pub)
	return hex.EncodeToString(n[:]), true
}
