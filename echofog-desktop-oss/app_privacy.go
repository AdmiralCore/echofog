package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// GetPrivacy returns who may write (everyone | contacts) and call (everyone | contacts | nobody).
func (a *App) GetPrivacy() (api.PrivacyDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.PrivacyDTO{}, errNodeNotStarted
	}
	return n.GetPrivacy(), nil
}

// SetPrivacy saves and applies who may write and call.
func (a *App) SetPrivacy(p api.PrivacyDTO) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.SetPrivacy(p)
}

// BlockPeer blocks a person: messages, calls, files and group invitations from them are ignored.
func (a *App) BlockPeer(peerID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.BlockPeer(peerID)
}

// UnblockPeer lifts a block.
func (a *App) UnblockPeer(peerID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.UnblockPeer(peerID)
}

// GetBlockedPeers lists blocked people, the latest first.
func (a *App) GetBlockedPeers() ([]api.BlockedDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.GetBlockedPeers()
}

// GetMessageRequests lists strangers who wrote first.
func (a *App) GetMessageRequests() ([]api.MessageRequestDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.GetMessageRequests()
}

// AcceptMessageRequest makes a stranger a contact.
func (a *App) AcceptMessageRequest(peerID, alias string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.AcceptMessageRequest(peerID, alias)
}

// DismissMessageRequest removes a request without blocking.
func (a *App) DismissMessageRequest(peerID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.DismissMessageRequest(peerID)
}
