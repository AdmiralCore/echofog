package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// A group is a space: its main chat, text channels (a ChatRef with Channel set) and voice rooms
// (a call in the room, which does not ring anybody).

// ChatChannels lists a group's channels and voice rooms, oldest first.
func (a *App) ChatChannels(ref api.ChatRef) ([]api.ChannelDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.ChatChannels(ref)
}

// ChatChannelSave creates a channel (spec.ID "") or changes one: name, topic, read-only.
func (a *App) ChatChannelSave(ref api.ChatRef, spec api.ChannelSpec) (api.ChannelDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChannelDTO{}, errNodeNotStarted
	}
	return n.ChatChannelSave(ref, spec)
}

// ChatChannelMove puts a channel at index among the channels of its kind.
func (a *App) ChatChannelMove(ref api.ChatRef, id string, index int) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatChannelMove(ref, id, index)
}

// ChatThreads are a channel's messages with comments, the most recently active first.
func (a *App) ChatThreads(ref api.ChatRef) ([]api.ChatMessage, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.ChatThreads(ref)
}

// ChatThreadTitle names the thread under a message.
func (a *App) ChatThreadTitle(ref api.ChatRef, root, title string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatThreadTitle(ref, root, title)
}

// ChatAttachments are a chat's media, files and links, newest first.
func (a *App) ChatAttachments(ref api.ChatRef) (api.ChatAttachments, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChatAttachments{}, errNodeNotStarted
	}
	return n.ChatAttachments(ref)
}

// SetContactProfile renames a contact and sets the picture shown for it (on this device only).
func (a *App) SetContactProfile(peerID, alias, avatarBase64 string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.SetContactProfile(peerID, alias, avatarBase64)
}

// LeaveRoomCall says we left a voice room whose call others stay in.
func (a *App) LeaveRoomCall(groupID, room, callID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.LeaveRoomCall(groupID, room, callID)
}

// ChatChannelRemove removes a channel.
func (a *App) ChatChannelRemove(ref api.ChatRef, id string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.ChatChannelRemove(ref, id)
}

// StartRoomCall joins a voice room: its call going on, or a new one.
func (a *App) StartRoomCall(groupID, room string) (api.GroupCall, error) {
	n := a.getAPI()
	if n == nil {
		return api.GroupCall{}, errNodeNotStarted
	}
	return n.StartRoomCall(groupID, room)
}

// KeepRoomCallAlive tells the members the room's call is still going; the UI calls it while in it.
func (a *App) KeepRoomCallAlive(groupID, room, callID, coordinator string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.KeepRoomCallAlive(groupID, room, callID, coordinator)
}

// EndRoomCall ends the room's call; call it when the last person leaves.
func (a *App) EndRoomCall(groupID, room, callID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.EndRoomCall(groupID, room, callID)
}
