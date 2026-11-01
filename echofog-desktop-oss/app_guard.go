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

// PinGuard saves a relay the node always routes through (a relay link or host:port/<peer id>?sni=…).
func (a *App) PinGuard(entry string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.PinGuard(entry)
}

// UnpinGuard forgets one of the user's pinned relays.
func (a *App) UnpinGuard(entry string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.UnpinGuard(entry)
}
