package node

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
)

// Anonymous mode, as the app sets it: "tor" (nobody in the network sees this computer's address),
// "relay" (only the user's own relay sees it) or "" (off). Like the proxy it takes effect on the
// next start, since links made before would go on showing the address. See pkg/node/anonymous.go.

// AnonymityStatus is the anonymous mode saved for the next start and the one in effect now.
type AnonymityStatus struct {
	Mode         string `json:"mode"`         // saved for the next start: "tor", "relay" or ""
	WalletDirect bool   `json:"walletDirect"` // saved: the TON wallet may reach its servers directly
	Running      string `json:"running"`      // in effect now
	// SeenBy is who sees this computer's address now: "nobody" (Tor), "relay" (the own relay
	// alone), or "network" (the nodes this one links to, as without anonymous mode).
	SeenBy string `json:"seenBy"`
	// Broken says why the mode in effect cannot hold ("" when it does): the node then connects
	// nowhere.
	Broken  string   `json:"broken,omitempty"`
	Proxy   string   `json:"proxy,omitempty"`   // the Tor proxy in effect
	Relays  []string `json:"relays,omitempty"`  // the relays the node may connect to now (relay mode)
	Restart bool     `json:"restart,omitempty"` // what is saved differs from what runs
	// HasProxy and HasRelay: what the saved settings give each mode to work with.
	HasProxy bool `json:"hasProxy"`
	HasRelay bool `json:"hasRelay"`
}

// GetAnonymity reports the anonymous mode, saved and in effect.
func (n *NodeApi) GetAnonymity() (AnonymityStatus, error) {
	if n.node == nil || n.node.Cfg == nil {
		return AnonymityStatus{}, errors.New("node not started")
	}
	cfg := n.node.Cfg
	running := n.node.AnonMode()
	settingsMu.Lock()
	next, nextWallet, nextProxy := n.nextAnon, n.nextAnonWallet, n.nextProxy
	settingsMu.Unlock()
	st := AnonymityStatus{Mode: running, WalletDirect: cfg.Network.AnonWalletDirect, Running: running,
		Broken: n.node.AnonBroken(), Proxy: network.OutboundProxy(), Relays: network.DialOnly(),
		HasRelay: len(cfg.AntiDPI.PinnedGuards) > 0, HasProxy: cfg.Network.Proxy != ""}
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
	switch {
	case running == config.AnonTor && st.Broken == "":
		st.SeenBy = "nobody"
	case running == config.AnonRelay && st.Broken == "":
		st.SeenBy = "relay"
	case running != "":
		st.SeenBy = "nobody" // broken: it connects nowhere
	default:
		st.SeenBy = "network"
	}
	return st, nil
}

// SetAnonymity saves the anonymous mode for the next start: "tor" needs the Tor proxy set (Settings,
// Tor and proxy), "relay" a relay pinned to go in through (Settings, Joining the network).
// walletDirect lets the TON wallet reach its servers directly while it is on.
func (n *NodeApi) SetAnonymity(mode string, walletDirect bool) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	switch mode {
	case "":
	case config.AnonTor:
		p := n.node.Cfg.Network.Proxy
		if n.nextProxy != nil {
			p = *n.nextProxy
		}
		if p == "" {
			return errors.New("set the Tor proxy first (Settings → Tor and proxy)")
		}
	case config.AnonRelay:
		if len(n.node.Cfg.AntiDPI.PinnedGuards) == 0 {
			return errors.New("pin your relay first (Settings → Joining the network → Route through your own relay)")
		}
	default:
		return errors.New("anonymous mode must be tor, relay or off")
	}
	if err := saveSettingsLocked(n.node.Cfg, func(s *persistedSettings) {
		s.AnonSet, s.Anonymous, s.AnonWalletDirect = true, mode, walletDirect
	}); err != nil {
		return err
	}
	n.nextAnon, n.nextAnonWallet = &mode, &walletDirect
	if n.node.Swarm == nil {
		n.node.Cfg.Network.Anonymous, n.node.Cfg.Network.AnonWalletDirect = mode, walletDirect
	}
	return nil
}

// ownHTTPTimeout bounds the app's own lookups; Tor takes seconds to build a circuit.
const ownHTTPTimeout = 30 * time.Second

// OwnHTTPClient is the HTTP client for the app's own lookups (updates, location, the TON config and
// rate): through the Tor proxy when one is set, through the VPN exit while it runs, and none (nil)
// in anonymous mode through the own relay with no VPN on, since the site would see the address.
func (n *NodeApi) OwnHTTPClient() *http.Client {
	if p := network.OutboundProxy(); p != "" {
		d, err := proxy.SOCKS5("tcp", p, nil, network.DirectDialer(10*time.Second))
		if cd, ok := d.(proxy.ContextDialer); err == nil && ok {
			// names go to the proxy unresolved: a lookup here would show on this network
			return &http.Client{Timeout: ownHTTPTimeout, Transport: &http.Transport{DialContext: cd.DialContext}}
		}
		return nil
	}
	n.vpn.mu.Lock()
	srv := n.vpn.server
	n.vpn.mu.Unlock()
	if srv != nil {
		if a := srv.Addr(); a != nil {
			u := &url.URL{Scheme: "socks5", Host: a.String()}
			return &http.Client{Timeout: ownHTTPTimeout, Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
		}
	}
	if n.node != nil && n.node.AnonMode() != "" {
		return nil
	}
	return &http.Client{Timeout: ownHTTPTimeout, Transport: &http.Transport{DialContext: (&net.Dialer{}).DialContext}}
}
