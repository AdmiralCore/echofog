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

// GetBootstrapNodes lists the built-in bootstrap nodes and the ones the user added.
func (a *App) GetBootstrapNodes() (api.BootstrapStatus, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.BootstrapStatus{}, errNodeNotStarted
	}
	return nodeAPI.GetBootstrapNodes()
}

// AddBootstrapNode saves a node to join the network through (a node link or host:port/<peer id>)
// and connects to it at once; for when the built-in nodes are blocked.
func (a *App) AddBootstrapNode(entry string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.AddBootstrapNode(entry)
}

// RemoveBootstrapNode forgets one of the user's bootstrap nodes.
func (a *App) RemoveBootstrapNode(entry string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.RemoveBootstrapNode(entry)
}

// SharingApprove confirms a client waiting for the owner of the shared connection.
func (a *App) SharingApprove(id, label string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SharingApprove(id, label)
}

// SharingReject turns a waiting client down.
func (a *App) SharingReject(id string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SharingReject(id)
}

// SharingRevoke takes a confirmed client's access back.
func (a *App) SharingRevoke(id string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SharingRevoke(id)
}

// SharingOpenFor lets anyone with the link use the shared connection for the given minutes.
func (a *App) SharingOpenFor(minutes int) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SharingOpenFor(minutes)
}

// SharingCloseOpen ends the open access.
func (a *App) SharingCloseOpen() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SharingCloseOpen()
}
