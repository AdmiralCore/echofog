package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// GetGuardStatus reports how the node enters the network (guard mode) and the guards it holds.
func (a *App) GetGuardStatus() (api.GuardStatus, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.GuardStatus{}, errNodeNotStarted
	}
	return nodeAPI.GetGuardStatus()
}

// SetGuardMode saves and applies how the node enters the network: "auto", "on" or "off".
func (a *App) SetGuardMode(mode string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetGuardMode(mode)
}
