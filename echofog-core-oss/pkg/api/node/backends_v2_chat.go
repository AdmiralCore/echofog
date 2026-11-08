package node

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/pkg/dto"
)

// The protocol 2 chats domain (T11.3d): the methods of chatBackend over the app layer. The DTOs and
// the events are the ones the frontend already knows (BACKENDS.md): ChatMessage, ConversationDTO,
// ConvChange. Messages name people by the node ID, as the contacts and the members of a group do.

func appRef(r ChatRef) app.ChatRef { return app.ChatRef{Kind: r.Kind, ID: r.ID, Channel: r.Channel} }

func chatRef(r app.ChatRef) ChatRef { return ChatRef{Kind: r.Kind, ID: r.ID, Channel: r.Channel} }

// peerNames turns the peer ids a message names people by into node IDs, asking the app once per id.
// An id the app does not know stays as it is.
type peerNames struct {
	app  v2Messenger
	memo map[string]string
}

func (b *v2MsgBackend) names() *peerNames { return &peerNames{app: b.app, memo: map[string]string{}} }

func (p *peerNames) of(s string) string {
	if s == "" {
		return ""
	}
	if n, ok := p.memo[s]; ok {
		return n
	}
	n, ok := p.app.NodeOfPeer(s)
	if !ok {
		n = s
	}
	p.memo[s] = n
	return n
}

func (p *peerNames) list(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = p.of(s)
	}
	return out
}

// msg copies a message of the log with every person named by the node ID. The log hands out shared
// slices, so the copy never writes into the original.
func (p *peerNames) msg(m convlog.Message) ChatMessage {
	m.Author, m.DeletedBy, m.PinnedBy = p.of(m.Author), p.of(m.DeletedBy), p.of(m.PinnedBy)
	m.Mentions = p.list(m.Mentions)
	if m.Reactions != nil {
		rs := make([]convlog.Reaction, len(m.Reactions))
		for i, r := range m.Reactions {
			r.By = p.list(r.By)
			rs[i] = r
		}
		m.Reactions = rs
	}
	return m
}

func (p *peerNames) msgs(in []convlog.Message) []ChatMessage {
	out := make([]ChatMessage, 0, len(in))
	for _, m := range in {
		out = append(out, p.msg(m))
	}
	return out
}

func (b *v2MsgBackend) page(p app.ChatPage, err error) (ChatPage, error) {
	if err != nil {
		return ChatPage{}, v2Err(err)
	}
	return ChatPage{Messages: b.names().msgs(p.Messages), Next: p.Next}, nil
}

func (b *v2MsgBackend) ChatHistory(ref ChatRef, cursor string, limit int) (ChatPage, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	return b.page(b.app.ChatHistory(ctx, appRef(ref), cursor, limit))
}

func (b *v2MsgBackend) ChatThread(ref ChatRef, root, cursor string, limit int) (ChatPage, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	return b.page(b.app.ChatThread(ctx, appRef(ref), root, cursor, limit))
}

func (b *v2MsgBackend) ChatPins(ref ChatRef) ([]ChatMessage, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	ms, err := b.app.ChatPins(ctx, appRef(ref))
	if err != nil {
		return nil, v2Err(err)
	}
	return b.names().msgs(ms), nil
}

func (b *v2MsgBackend) ChatMessage(ref ChatRef, id string) (ChatMessage, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	m, err := b.app.ChatMessage(ctx, appRef(ref), id)
	if err != nil {
		return ChatMessage{}, v2Err(err)
	}
	return b.names().msg(m), nil
}

func (b *v2MsgBackend) ChatSend(ref ChatRef, req SendRequest) (ChatMessage, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	m, err := b.app.ChatSend(ctx, appRef(ref), app.SendReq{Text: req.Text, ReplyTo: req.ReplyTo, Thread: req.Thread})
	if err != nil {
		// the message is written and queued even when the send failed: the UI repeats the sending,
		// it does not write the message again (the app returns it together with the error)
		if m.ID != "" {
			return b.names().msg(m), v2Err(err)
		}
		return ChatMessage{}, v2Err(err)
	}
	return b.names().msg(m), nil
}

func (b *v2MsgBackend) ChatEdit(ref ChatRef, id, text string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.ChatEdit(ctx, appRef(ref), id, text))
}

func (b *v2MsgBackend) ChatDelete(ref ChatRef, id string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.ChatDelete(ctx, appRef(ref), id))
}

func (b *v2MsgBackend) ChatReact(ref ChatRef, id, emoji string, on bool) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.ChatReact(ctx, appRef(ref), id, emoji, on))
}

func (b *v2MsgBackend) ChatPin(ref ChatRef, id string, on bool) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.ChatPin(ctx, appRef(ref), id, on))
}

func (b *v2MsgBackend) ChatMarkRead(ref ChatRef, id string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(b.app.ChatMarkRead(ctx, appRef(ref), id))
}

// ---- the chat list ---------------------------------------------------------------------------------------------

// canOf is what I may do in a chat. A stranger's chat and a blocked contact's are read only; a group
// that has not let me in is too.
func canOf(e app.ConvEntry, rights convlog.Rights, hasRights bool) ConvCan {
	if e.Ref.Kind == app.KindGroup {
		if !hasRights {
			return ConvCan{}
		}
		return ConvCan{Send: rights.Send, Files: rights.Media, Links: rights.Links, Threads: rights.Threads, React: rights.React,
			Pin: rights.Pin, Manage: rights.Manage, Channels: rights.Manage, Roles: rights.Roles, Invite: rights.Invite,
			Call: rights.Voice, Stream: rights.Stream, SlowFree: rights.Moderate || rights.Manage}
	}
	if e.Request || e.Blocked {
		return ConvCan{}
	}
	return ConvCan{Send: true, Files: true, Links: true, Threads: true, React: true, Pin: true, Call: true, Stream: true}
}

// row builds one entry of the chat list; info is the group's line for a group.
func (b *v2MsgBackend) row(p *peerNames, e app.ConvEntry, info *app.GroupInfo) ConversationDTO {
	out := ConversationDTO{Ref: chatRef(e.Ref), Conv: e.Conv, LastTS: e.LastTS, Unread: e.Unread, MutedUntil: e.MutedUntil, Request: e.Request,
		Upgraded: e.Upgraded, Kind: e.Kind, Name: e.Name, PeerID: e.PeerID, PubKey: e.PeerID, Online: e.Online, Blocked: e.Blocked}
	if e.Last != nil {
		m := p.msg(*e.Last)
		out.Last = &m
	}
	if e.Ref.Kind != app.KindGroup {
		out.Can = canOf(e, convlog.Rights{}, false)
		return out
	}
	out.PeerID, out.PubKey = "", ""
	var rights convlog.Rights
	var have bool
	if info != nil {
		out.Title, out.Role, out.Members = info.Title, info.Role, info.Members
		if info.Pending {
			out.Pending = "admission"
		} else if r, err := b.app.GroupRights(e.Ref.ID, ""); err == nil {
			rights, have = r, true
			if reqs, err := b.app.GroupJoinRequests(e.Ref.ID); err == nil {
				out.Requests = len(reqs)
			}
		}
	}
	out.Can = canOf(e, rights, have)
	return out
}

func (b *v2MsgBackend) Conversations() ([]ConversationDTO, error) {
	list, err := b.app.Conversations()
	if err != nil {
		return nil, v2Err(err)
	}
	groups := map[string]app.GroupInfo{}
	if gs, err := b.app.Groups(); err == nil {
		for _, g := range gs {
			groups[g.ID] = g
		}
	}
	p := b.names()
	out := make([]ConversationDTO, 0, len(list))
	for _, e := range list {
		var info *app.GroupInfo
		if g, ok := groups[e.Ref.ID]; ok && e.Ref.Kind == app.KindGroup {
			info = &g
		}
		out = append(out, b.row(p, e, info))
	}
	return out, nil
}

func (b *v2MsgBackend) ChatRows() ([]ConversationDTO, error) { return b.Conversations() }

// SubscribeConversations delivers every change of a chat; a later subscription replaces the earlier one.
func (b *v2MsgBackend) SubscribeConversations(cb func(ConvChange)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancelConv != nil {
		b.cancelConv()
		b.cancelConv = nil
	}
	if cb == nil {
		return
	}
	b.cancelConv = b.app.SubscribeChats(func(c app.ConvChange) { cb(b.convChange(c)) })
}

func (b *v2MsgBackend) convChange(c app.ConvChange) ConvChange {
	p := b.names()
	var info *app.GroupInfo
	if c.Ref.Kind == app.KindGroup {
		if g, err := b.app.GroupInfoOf(c.Ref.ID); err == nil {
			info = &g
		}
	}
	return ConvChange{Ref: chatRef(c.Ref), Conversation: b.row(p, c.Conversation, info), Messages: p.msgs(c.Messages),
		Reload: c.Reload, Rev: c.Rev}
}

// ---- the calls of v1 that have no more to say ---------------------------------------------------------------

// SendMessage is the plain message call of v1: a message to a contact.
func (b *v2MsgBackend) SendMessage(pubKeyHex string, text string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(asContact(pubKeyHex, func(node string) error {
		_, err := b.app.ChatSend(ctx, app.ChatRef{Kind: app.KindDM, ID: node}, app.SendReq{Text: text})
		return err
	}))
}

// asContact runs do with the node ID arg stands for: the node ID itself, or, when the app has no such
// contact, the node of the account key of that name (the argument of v1 was the key).
func asContact(arg string, do func(node string) error) error {
	err := do(arg)
	if !errors.Is(err, app.ErrNoContact) {
		return err
	}
	raw, derr := hex.DecodeString(strings.TrimSpace(arg))
	if derr != nil || len(raw) != 32 {
		return err
	}
	n := convsync.NodeOf([32]byte(raw))
	return do(hex.EncodeToString(n[:]))
}

// GetMessages is the history call of v1, over the direct chat.
func (b *v2MsgBackend) GetMessages(contactID string, limit int, beforeTimestamp uint64) ([]dto.MessageDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	page, err := b.app.ChatHistory(ctx, app.ChatRef{Kind: app.KindDM, ID: contactID}, "", limit)
	if err != nil {
		return nil, v2Err(err)
	}
	p := b.names()
	out := make([]dto.MessageDTO, 0, len(page.Messages))
	for _, m := range page.Messages {
		if beforeTimestamp != 0 && uint64(m.TS) >= beforeTimestamp {
			continue
		}
		out = append(out, dto.MessageDTO{ID: m.ID, IsMine: m.Mine, SenderID: p.of(m.Author), TargetID: contactID, Type: dto.MsgTypeText,
			Text: m.Text, Metadata: m.Metadata, Timestamp: m.TS, ReplyToID: m.ReplyTo, State: m.State})
	}
	return out, nil
}

// MessageHandler stays closed: a new message of v2 is told by the change of its chat (conv:change).
func (b *v2MsgBackend) MessageHandler() <-chan *dto.MessageDTO {
	return closedChannel[*dto.MessageDTO]()
}

// OnMessageState has nothing to say: the state of a message is part of the chat message itself.
func (b *v2MsgBackend) OnMessageState(cb func(MessageStateDTO)) {}

// ---- attachments -----------------------------------------------------------------------------------------------

// ChatAttachments lists a chat's media, files and links (every text channel of a group), newest first.
func (b *v2MsgBackend) ChatAttachments(ref ChatRef) (ChatAttachments, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	channels := []string{ref.Channel}
	if ref.Kind == app.KindGroup {
		channels = []string{""}
		if chs, err := b.app.GroupChannels(ref.ID); err == nil {
			for _, ch := range chs {
				if !ch.Voice && !ch.Category && ch.ID != "" {
					channels = append(channels, ch.ID)
				}
			}
		}
	}
	p := b.names()
	out := ChatAttachments{Media: []ChatMessage{}, Files: []ChatMessage{}, Links: []ChatLink{}}
	seen := 0
	for _, ch := range channels {
		cursor := ""
		for seen < attachmentScan {
			r := appRef(ref)
			r.Channel = ch
			page, err := b.app.ChatHistory(ctx, r, cursor, 200)
			if err != nil {
				if seen == 0 && ch == channels[0] {
					return ChatAttachments{}, v2Err(err)
				}
				break
			}
			for _, m := range page.Messages {
				seen++
				if m.Deleted {
					continue
				}
				if m.File != nil {
					if isMedia(m.File.Mime, m.File.Name) {
						out.Media = append(out.Media, p.msg(m))
					} else {
						out.Files = append(out.Files, p.msg(m))
					}
				}
				for _, u := range linkRe.FindAllString(m.Text, 20) {
					out.Links = append(out.Links, ChatLink{URL: strings.TrimRight(u, ".,;:!?)]}"), MessageID: m.ID, Author: p.of(m.Author), Mine: m.Mine, TS: m.TS})
				}
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
	}
	return out, nil
}

// ---- saved messages (T11.3e): kept on this device by the app layer, never sent --------------------------------

func savedDTO(m app.SavedMessage) dto.SavedMessageDTO {
	return dto.SavedMessageDTO{ID: m.ID, ChatID: m.ChatID, ChatName: m.ChatName, MsgID: m.MsgID, Channel: m.Channel, Thread: m.Thread,
		Sender: m.Sender, Mine: m.Mine, Text: m.Text, FileName: m.FileName, FileSize: m.FileSize, Ts: m.TS, SavedAt: m.SavedAt}
}

func (b *v2MsgBackend) SaveMessage(m dto.SavedMessageDTO) (dto.SavedMessageDTO, error) {
	s, err := b.app.SaveMessage(app.SavedMessage{ChatID: m.ChatID, ChatName: m.ChatName, MsgID: m.MsgID, Channel: m.Channel, Thread: m.Thread,
		Sender: m.Sender, Mine: m.Mine, Text: m.Text, FileName: m.FileName, FileSize: m.FileSize, TS: m.Ts})
	if err != nil {
		return dto.SavedMessageDTO{}, v2Err(err)
	}
	return savedDTO(s), nil
}

func (b *v2MsgBackend) UnsaveMessage(id string) error { return v2Err(b.app.UnsaveMessage(id)) }

func (b *v2MsgBackend) ListSavedMessages() ([]dto.SavedMessageDTO, error) {
	list, err := b.app.ListSavedMessages()
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]dto.SavedMessageDTO, 0, len(list))
	for _, m := range list {
		out = append(out, savedDTO(m))
	}
	return out, nil
}

func (b *v2MsgBackend) SetChatMutedUntil(ref ChatRef, until int64) error {
	return v2Err(b.app.ChatMuteUntil(appRef(ref), until))
}

func (b *v2MsgBackend) ChatMutedUntil(ref ChatRef) (int64, error) {
	u, err := b.app.ChatMutedUntil(appRef(ref))
	return u, v2Err(err)
}

// ---- notifications, followed threads (T7.7) -------------------------------------------------------------------

func (b *v2MsgBackend) ChatNotify(ref ChatRef) (string, error) {
	m, err := b.app.ChatNotify(appRef(ref))
	return m, v2Err(err)
}

func (b *v2MsgBackend) SetChatNotify(ref ChatRef, mode string) error {
	return v2Err(b.app.SetChatNotify(appRef(ref), mode))
}

func (b *v2MsgBackend) ChatFollow(ref ChatRef, root string, on bool) error {
	return v2Err(b.app.ChatFollow(appRef(ref), root, on))
}

func (b *v2MsgBackend) ChatFollowing(ref ChatRef) ([]string, error) {
	l, err := b.app.ChatFollowing(appRef(ref))
	if err != nil {
		return nil, v2Err(err)
	}
	if l == nil {
		l = []string{}
	}
	return l, nil
}

func (b *v2MsgBackend) GroupVoice(groupID, channel string) ([]VoiceMemberDTO, error) {
	list, err := b.app.GroupVoice(groupID, channel)
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]VoiceMemberDTO, 0, len(list))
	for _, m := range list {
		out = append(out, VoiceMemberDTO{PeerID: m.Node, Muted: m.Muted, Camera: m.Camera, Stream: m.Stream})
	}
	return out, nil
}
