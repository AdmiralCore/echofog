package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/echofog/echofog-core/internal/config"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

const settingsFileName = "ui_settings.json"

// SettingsDTO holds the user-changeable connection settings. Only settings that change what the node
// does are offered here; a setting takes effect the next time the node starts.
type SettingsDTO struct {
	DirectSocket   bool `json:"directSocket"`
	SocketBufferMB int  `json:"socketBufferMb"`
}

var settingsMu sync.Mutex

func settingsFromConfig(c *config.AppConfig) SettingsDTO {
	return SettingsDTO{
		DirectSocket:   c.Network.DirectSocket,
		SocketBufferMB: c.Network.SocketBufferMB,
	}
}

func applySettings(c *config.AppConfig, s SettingsDTO) {
	c.Network.DirectSocket = s.DirectSocket
	c.Network.SocketBufferMB = s.SocketBufferMB
}

func settingsPath(c *config.AppConfig) string {
	if c == nil || c.Storage.DatabasePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.Storage.DatabasePath), settingsFileName)
}

// persistedSettings is the settings file: the connection settings, and the relay settings once
// the user has chosen them (until then the defaults apply).
type persistedSettings struct {
	SettingsDTO
	Relay *RelaySettingsDTO `json:"relay,omitempty"`
	// ExitFreePeers are the peers the exit serves for free when sharing the connection.
	ExitFreePeers []string `json:"exitFreePeers,omitempty"`
	// BootstrapNodes are nodes the user added to join the network through ("host:port/<peer id>"),
	// for when the built-in ones are blocked.
	BootstrapNodes []string `json:"bootstrapNodes,omitempty"`
	// VPNNoAutoSwitch: the proxy stays on the chosen exit even when it stops working.
	VPNNoAutoSwitch bool `json:"vpnNoAutoSwitch,omitempty"`
	// AccessMode is who may reach this node from its next start: "public" or "hidden" (see
	// config.AccessConfig). Empty: what the configuration says.
	AccessMode string `json:"accessMode,omitempty"`
	// Privacy is who may write and call; nil: the defaults.
	Privacy *coreNode.Privacy `json:"privacy,omitempty"`
}

// loadPersistedSettings applies the settings file to c. It returns what the file holds beyond the
// config (the exit's free list) and whether it held relay settings.
func loadPersistedSettings(c *config.AppConfig) (p persistedSettings, relayChosen bool) {
	p.SettingsDTO = settingsFromConfig(c)
	path := settingsPath(c)
	if path == "" {
		return p, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return p, false
	}
	if json.Unmarshal(raw, &p) != nil {
		return persistedSettings{SettingsDTO: settingsFromConfig(c)}, false
	}
	applySettings(c, p.SettingsDTO)
	if p.AccessMode == config.AccessPublic || p.AccessMode == config.AccessHidden {
		c.Access.Mode = p.AccessMode
	}
	if boot, err := normalizeBootstrap(p.BootstrapNodes); err == nil {
		p.BootstrapNodes = boot
	} else {
		p.BootstrapNodes = nil
	}
	if free, err := normalizePeerIDs(p.ExitFreePeers); err == nil {
		p.ExitFreePeers = free
	} else {
		p.ExitFreePeers = nil
	}
	if p.Relay != nil {
		r, err := normalizeRelaySettings(*p.Relay)
		if err != nil {
			return p, false // a hand-edited file with a bad entry: keep the defaults rather than half of it
		}
		applyRelaySettings(c, r)
		return p, true
	}
	return p, false
}

// saveSettingsLocked rewrites the settings file with change applied; the caller holds settingsMu.
func saveSettingsLocked(c *config.AppConfig, change func(*persistedSettings)) error {
	path := settingsPath(c)
	if path == "" {
		return errors.New("profile directory unavailable")
	}
	p := persistedSettings{SettingsDTO: settingsFromConfig(c)}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &p)
	}
	change(&p)
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func validateSettings(s SettingsDTO) error {
	if s.SocketBufferMB < 1 || s.SocketBufferMB > 256 {
		return errors.New("socket buffer must be between 1 and 256 MB")
	}
	return nil
}

func (n *NodeApi) GetSettings() (SettingsDTO, error) {
	if n.node == nil || n.node.Cfg == nil {
		return SettingsDTO{}, errors.New("node not started")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return settingsFromConfig(n.node.Cfg), nil
}

func (n *NodeApi) SetSettings(s SettingsDTO) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	if err := validateSettings(s); err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.SettingsDTO = s }); err != nil {
		return err
	}
	applySettings(n.node.Cfg, s)
	return nil
}
