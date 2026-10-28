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

// ChatChannelSave creates a channel (id "") or renames one; voice makes it a voice room.
func (a *App) ChatChannelSave(ref api.ChatRef, id, name string, voice bool) (api.ChannelDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.ChannelDTO{}, errNodeNotStarted
	}
	return n.ChatChannelSave(ref, id, name, voice)
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
