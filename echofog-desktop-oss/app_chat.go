package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// Chats on the conversation log: direct chats and groups, with replies, threads (comments),
// edits, deletes, reactions and pins (see echofog-core-oss/docs/design/conversation-log.md).
// Changes arrive as the "conv:change" event (api.ConvChange).

// ChatList is the chat list, most recently active first.
func (a *App) ChatList() ([]api.ConversationDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.ChatRows()
}

// ChatHistory is a page of a chat, newest first; pass "" for the newest page and the returned
// next for the one before it.
func (a *App) ChatHistory(ref api.ChatRef, cursor string, limit int) (api.ChatPage, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChatPage{}, errNodeNotStarted
	}
	return n.ChatHistory(ref, cursor, limit)
}

// ChatThread is a page of the comments under a message, oldest first.
func (a *App) ChatThread(ref api.ChatRef, rootID, cursor string, limit int) (api.ChatPage, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChatPage{}, errNodeNotStarted
	}
	return n.ChatThread(ref, rootID, cursor, limit)
}

// ChatPins are a chat's pinned messages, most recently pinned first.
func (a *App) ChatPins(ref api.ChatRef) ([]api.ChatMessage, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.ChatPins(ref)
}

// ChatMessage is one message.
func (a *App) ChatMessage(ref api.ChatRef, id string) (api.ChatMessage, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChatMessage{}, errNodeNotStarted
	}
	return n.ChatMessage(ref, id)
}

// ChatSend sends a message: plain, an answer (replyTo) or a comment (thread).
func (a *App) ChatSend(ref api.ChatRef, req api.SendRequest) (api.ChatMessage, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChatMessage{}, errNodeNotStarted
	}
	return n.ChatSend(ref, req)
}

// ChatEdit replaces the text of one of my messages.
func (a *App) ChatEdit(ref api.ChatRef, id, text string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatEdit(ref, id, text)
}

// ChatDelete deletes a message for everyone.
func (a *App) ChatDelete(ref api.ChatRef, id string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatDelete(ref, id)
}

// ChatReact sets (on) or clears my reaction on a message.
func (a *App) ChatReact(ref api.ChatRef, id, emoji string, on bool) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatReact(ref, id, emoji, on)
}

// ChatPin pins (on) or unpins a message for the whole chat.
func (a *App) ChatPin(ref api.ChatRef, id string, on bool) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatPin(ref, id, on)
}

// ChatMarkRead records that the chat was read up to message id.
func (a *App) ChatMarkRead(ref api.ChatRef, id string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatMarkRead(ref, id)
}
