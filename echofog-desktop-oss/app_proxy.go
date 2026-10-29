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

// CheckProxy connects through the proxy at addr to a public site: the proxy is running.
func (a *App) CheckProxy(addr string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.CheckProxy(addr)
}
