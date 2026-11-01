package node

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
)

// The proxy the node goes out through (Tor, or any SOCKS5 proxy) and how calls reach the other
// side. Both take effect when the node next starts: a running node keeps the links it has, and
// links made without the proxy would go on showing its address.

// proxyCheckTarget is what CheckProxy connects to through the proxy: a site that is up wherever
// Tor is, and says nothing about EchoFog.
const proxyCheckTarget = "check.torproject.org:443"

// ProxyStatus is the proxy and call route saved for the next start and those in effect now.
type ProxyStatus struct {
	Proxy     string `json:"proxy"`     // SOCKS5 host:port saved for the next start, "" none
	CallRoute string `json:"callRoute"` // "auto", "direct" or "relay", saved for the next start
	// Running is the proxy the node goes through now ("" none); RunningCallRoute how calls go now
	// (with a proxy on, "auto" is "relay").
	Running          string `json:"running"`
	RunningCallRoute string `json:"runningCallRoute"`
	Restart          bool   `json:"restart"` // what is saved differs from what runs: restart the app
}

func normalizeCallRoute(r string) (string, error) {
	switch r {
	case "", config.CallAuto:
		return config.CallAuto, nil
	case config.CallDirect, config.CallRelay:
		return r, nil
	}
	return "", errors.New("call route must be auto, direct or relay")
}

// GetProxySettings reports the proxy and call route, saved and in effect.
func (n *NodeApi) GetProxySettings() (ProxyStatus, error) {
	if n.node == nil || n.node.Cfg == nil {
		return ProxyStatus{}, errors.New("node not started")
	}
	running := n.node.Cfg.Network.Proxy
	runningRoute, _ := normalizeCallRoute(n.node.Cfg.Network.CallRoute)
	settingsMu.Lock()
	next, nextRoute := n.nextProxy, n.nextCallRoute
	settingsMu.Unlock()
	if next == nil {
		next = &running
	}
	if nextRoute == "" {
		nextRoute = runningRoute
	}
	return ProxyStatus{
		Proxy: *next, CallRoute: nextRoute,
		Running: running, RunningCallRoute: n.node.CallRoute(),
		Restart: *next != running || nextRoute != runningRoute,
	}, nil
}

// SetProxySettings saves the proxy ("" for none; host:port, or with socks5://) and the call route
// for the next start. Before the node first starts they apply to that very start.
func (n *NodeApi) SetProxySettings(proxy, callRoute string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	p, err := network.NormalizeProxyAddr(proxy)
	if err != nil {
		return err
	}
	route, err := normalizeCallRoute(callRoute)
	if err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := saveSettingsLocked(n.node.Cfg, func(s *persistedSettings) {
		s.ProxySet, s.Proxy, s.CallRoute = true, p, route
	}); err != nil {
		return err
	}
	n.nextProxy, n.nextCallRoute = &p, route
	if n.node.Swarm == nil {
		n.node.Cfg.Network.Proxy, n.node.Cfg.Network.CallRoute = p, route
	}
	return nil
}

// CheckProxy connects through the proxy at addr to a public site and closes the connection: the
// proxy is running and lets the app out. Nothing is sent.
func (n *NodeApi) CheckProxy(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return network.CheckProxy(ctx, addr, proxyCheckTarget)
}
