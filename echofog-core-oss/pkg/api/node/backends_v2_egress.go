package node

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	v2rt "github.com/echofog/echofog-core/internal/v2/node"
)

// Proxy, Tor and the call route on protocol 2 (T11.6a). The settings are saved for the next start
// exactly as on protocol 1; at start EgressFromConfig turns them into the node's Egress. What runs
// comes from the node itself (app.NetStatus).

// relayModeNote is the caption under the own-relay mode on protocol 2: shown, not hidden, not yet
// selectable (T11.6b).
const relayModeNote = "Будет в следующей версии"

var errRelayLater = &v2UserError{msg: "Режим «через свой relay» будет в следующей версии"}

// EgressFromConfig is the proxy mode a start must run in, from the saved settings: nil for a direct
// node. A mode that cannot hold is an error and the node must not start: Tor without a proxy, and the
// own-relay mode, which protocol 2 does not have yet, never fall back to an open connection.
func EgressFromConfig(c *config.AppConfig) (*v2rt.Egress, error) {
	if c == nil {
		return nil, errors.New("no configuration")
	}
	proxy, err := network.NormalizeProxyAddr(c.Network.Proxy)
	if err != nil {
		return nil, &v2UserError{msg: "Адрес прокси указан неверно"}
	}
	switch c.Network.Anonymous {
	case "":
		if proxy == "" {
			return nil, nil
		}
		return &v2rt.Egress{Proxy: proxy}, nil
	case config.AnonTor:
		if proxy == "" {
			return nil, &v2UserError{msg: "Режим Tor включён, но прокси не задан: соединений не будет"}
		}
		return &v2rt.Egress{Proxy: proxy, Tor: true}, nil
	case config.AnonRelay:
		return nil, errRelayLater
	}
	return nil, &v2UserError{msg: "Режим анонимности указан неверно"}
}

func (b *v2NetBackend) cfg() (*config.AppConfig, error) {
	if b.api == nil || b.api.node == nil || b.api.node.Cfg == nil {
		return nil, errors.New("node not started")
	}
	return b.api.node.Cfg, nil
}

func (b *v2NetBackend) GetProxySettings() (ProxyStatus, error) {
	cfg, err := b.cfg()
	if err != nil {
		return ProxyStatus{}, err
	}
	st := b.app.NetStatus()
	cfgRoute, _ := normalizeCallRoute(cfg.Network.CallRoute)
	settingsMu.Lock()
	next, nextRoute := b.api.nextProxy, b.api.nextCallRoute
	settingsMu.Unlock()
	saved := cfg.Network.Proxy
	if next == nil {
		next = &saved
	}
	if nextRoute == "" {
		nextRoute = cfgRoute
	}
	running := st.Proxy
	runRoute := st.CallRoute
	if runRoute == "" {
		runRoute = config.CallAuto
	}
	return ProxyStatus{Proxy: *next, CallRoute: nextRoute, Running: running, RunningCallRoute: runRoute,
		Restart: *next != running || nextRoute != cfgRoute}, nil
}

func (b *v2NetBackend) SetProxySettings(proxy, callRoute string) error {
	cfg, err := b.cfg()
	if err != nil {
		return err
	}
	p, err := network.NormalizeProxyAddr(proxy)
	if err != nil {
		return &v2UserError{msg: "Адрес прокси указан неверно: нужен вид хост:порт"}
	}
	route, err := normalizeCallRoute(callRoute)
	if err != nil {
		return &v2UserError{msg: "Маршрут звонков: auto, direct или relay"}
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := saveSettingsLocked(cfg, func(s *persistedSettings) {
		s.ProxySet, s.Proxy, s.CallRoute = true, p, route
	}); err != nil {
		return err
	}
	b.api.nextProxy, b.api.nextCallRoute = &p, route
	return nil
}

// CheckProxy connects through the proxy at addr to a public site and closes the connection.
func (b *v2NetBackend) CheckProxy(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := network.CheckProxy(ctx, addr, proxyCheckTarget); err != nil {
		return &v2UserError{msg: "Прокси не отвечает или не пропускает соединения"}
	}
	return nil
}

func (b *v2NetBackend) GetAnonymity() (AnonymityStatus, error) {
	cfg, err := b.cfg()
	if err != nil {
		return AnonymityStatus{}, err
	}
	ns := b.app.NetStatus()
	running := ""
	if ns.Tor {
		running = config.AnonTor
	}
	settingsMu.Lock()
	next, nextWallet, nextProxy := b.api.nextAnon, b.api.nextAnonWallet, b.api.nextProxy
	settingsMu.Unlock()
	st := AnonymityStatus{Mode: running, WalletDirect: cfg.Network.AnonWalletDirect, Running: running,
		Proxy: ns.Proxy, HasProxy: cfg.Network.Proxy != "", RelayNote: relayModeNote}
	if running == "" && cfg.Network.Anonymous != "" {
		st.Mode = cfg.Network.Anonymous
	}
	if next != nil {
		st.Mode = *next
	}
	if nextWallet != nil {
		st.WalletDirect = *nextWallet
	}
	if nextProxy != nil {
		st.HasProxy = *nextProxy != ""
	}
	st.Restart = st.Mode != running || st.WalletDirect != cfg.Network.AnonWalletDirect
	if ns.Proxy != "" {
		st.SeenBy = "nobody" // the nodes of the network see the proxy, not this computer
	} else {
		st.SeenBy = "network"
	}
	return st, nil
}

// SetAnonymity saves the mode for the next start: "tor" needs a proxy; the own-relay mode is not
// there yet on protocol 2 and says so.
func (b *v2NetBackend) SetAnonymity(mode string, walletDirect bool) error {
	cfg, err := b.cfg()
	if err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	switch mode {
	case "":
	case config.AnonTor:
		p := cfg.Network.Proxy
		if b.api.nextProxy != nil {
			p = *b.api.nextProxy
		}
		if p == "" {
			return &v2UserError{msg: "Сначала нужно задать прокси Tor (Настройки → Tor и прокси)"}
		}
	case config.AnonRelay:
		return errRelayLater
	default:
		return &v2UserError{msg: "Режим анонимности: tor или выключено"}
	}
	if err := saveSettingsLocked(cfg, func(s *persistedSettings) {
		s.AnonSet, s.Anonymous, s.AnonWalletDirect = true, mode, walletDirect
	}); err != nil {
		return err
	}
	b.api.nextAnon, b.api.nextAnonWallet = &mode, &walletDirect
	return nil
}
