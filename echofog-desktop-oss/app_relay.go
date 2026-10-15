package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// GetRelayStatus returns the relay settings, the relay link to share and what the relay is doing.
func (a *App) GetRelayStatus() (api.RelayStatus, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.RelayStatus{}, errNodeNotStarted
	}
	return nodeAPI.GetRelayStatus()
}

// SetRelaySettings saves the relay settings and applies them (the listen port after a restart).
func (a *App) SetRelaySettings(s api.RelaySettingsDTO) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetRelaySettings(s)
}

// AddRelay adds a relay link (or host:port/<peer id>) to the preferred relays and connects to it.
func (a *App) AddRelay(link string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.AddRelay(link)
}

// StartVPNVia connects the proxy through one node given by a relay link, address or peer ID.
func (a *App) StartVPNVia(target string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.StartVPNVia(target)
}

// SetExitFreePeers sets the peers the shared connection serves for free.
func (a *App) SetExitFreePeers(ids []string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetExitFreePeers(ids)
}
