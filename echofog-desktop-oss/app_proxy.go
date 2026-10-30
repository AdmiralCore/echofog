package main

import (
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// GetProxySettings reports the proxy (Tor) the app goes out through and how calls go, saved and in
// effect.
func (a *App) GetProxySettings() (api.ProxyStatus, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.ProxyStatus{}, errNodeNotStarted
	}
	return nodeAPI.GetProxySettings()
}

// SetProxySettings saves the proxy ("" for none) and the call route ("auto", "direct", "relay")
// for the next start.
func (a *App) SetProxySettings(proxy, callRoute string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetProxySettings(proxy, callRoute)
}

// GetAnonymity reports the anonymous mode, saved and in effect, and who sees the address now.
func (a *App) GetAnonymity() (api.AnonymityStatus, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.AnonymityStatus{}, errNodeNotStarted
	}
	return nodeAPI.GetAnonymity()
}

// SetAnonymity saves the anonymous mode ("tor", "relay" or "") for the next start, and whether the
// TON wallet may reach its servers directly while it is on.
func (a *App) SetAnonymity(mode string, walletDirect bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetAnonymity(mode, walletDirect)
}

// CheckProxy connects through the proxy at addr to a public site: the proxy is running.
func (a *App) CheckProxy(addr string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.CheckProxy(addr)
}
