package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// GetAccessStatus returns who may reach this node (public or hidden), its invitation link and who
// joined with it.
func (a *App) GetAccessStatus() (api.AccessStatus, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.AccessStatus{}, errNodeNotStarted
	}
	return nodeAPI.GetAccessStatus()
}

// SetAccessMode saves "public" or "hidden" for the next start of the node.
func (a *App) SetAccessMode(mode string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetAccessMode(mode)
}

// RotateAccessSecret makes a new invitation link; the old one lets nobody new in.
func (a *App) RotateAccessSecret() (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	return nodeAPI.RotateAccessSecret()
}

// RemoveJoinedPeer takes somebody off the list of those who joined with the link.
func (a *App) RemoveJoinedPeer(peerID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.RemoveJoinedPeer(peerID)
}

// ParseInvite reads an invitation link, to show what it is before connecting.
func (a *App) ParseInvite(link string) (api.InviteInfo, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.InviteInfo{}, errNodeNotStarted
	}
	return nodeAPI.ParseInvite(link)
}
