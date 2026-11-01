package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// GetCallPath says how the active call reaches each member: directly or through a relay, and why.
func (a *App) GetCallPath() api.CallPath {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.CallPath{Members: []api.CallPathMember{}}
	}
	return nodeAPI.GetCallPath()
}

// GetCallRelays lists the relays a call can go through, each pinged, and the paid ones known.
func (a *App) GetCallRelays() []api.CallRelay {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return []api.CallRelay{}
	}
	return nodeAPI.GetCallRelays()
}

// SwitchCallRelay moves the call's relayed members to another relay: the one given by peer ID, or
// the best other one when it is "".
func (a *App) SwitchCallRelay(via string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SwitchCallRelay(via)
}
