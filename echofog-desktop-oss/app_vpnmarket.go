package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

func (a *App) StartSharing(cfg api.SharingConfig) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.StartSharing(cfg)
}

func (a *App) StopSharing() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.StopSharing()
}

func (a *App) GetSharingStatus() api.SharingStatus {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.SharingStatus{}
	}
	return nodeAPI.GetSharingStatus()
}

func (a *App) ListExitNodes() []api.ExitNode {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return []api.ExitNode{}
	}
	return nodeAPI.ListExitNodes()
}

func (a *App) RefreshExitNodes() (int, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return 0, errNodeNotStarted
	}
	return nodeAPI.RefreshExitNodes()
}

func (a *App) ProbeExitNode(peerID string) (api.ProbeReport, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.ProbeReport{}, errNodeNotStarted
	}
	return nodeAPI.ProbeExitNode(peerID)
}

func (a *App) GetProbeDomains() []string {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return []string{}
	}
	return nodeAPI.GetProbeDomains()
}

func (a *App) SetProbeDomains(domains []string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetProbeDomains(domains)
}

// MeasureUploadCapacity measures this computer's upload speed against connected peers, in kbit/s.
func (a *App) MeasureUploadCapacity() (int, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return 0, errNodeNotStarted
	}
	return nodeAPI.MeasureUploadCapacity()
}

// CheckSites tells which sites open directly and which through an exit ("auto": the exit in
// use or the best one; "": only directly).
func (a *App) CheckSites(exitPeerID string, sites []string) ([]coreNode.SiteCheck, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errNodeNotStarted
	}
	return nodeAPI.CheckSites(exitPeerID, sites)
}
