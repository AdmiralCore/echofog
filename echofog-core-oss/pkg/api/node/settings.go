package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/echofog/echofog-core/internal/config"
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

func loadPersistedSettings(c *config.AppConfig) {
	path := settingsPath(c)
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s := settingsFromConfig(c)
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	applySettings(c, s)
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
	path := settingsPath(n.node.Cfg)
	if path == "" {
		return errors.New("profile directory unavailable")
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	applySettings(n.node.Cfg, s)
	return nil
}
