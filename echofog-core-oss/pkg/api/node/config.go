package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
)

func GetDefaultConfig(profileName string) *config.AppConfig {
	return config.DefaultConfig(profileName)
}

func GetAvailableProfiles() ([]string, error) {
	return config.GetAvailableProfiles()
}

// VerifyProfilePassword reports nil when appPassword unlocks the named profile's identity. It is a
// cheap check (no node is started) used to gate destructive actions behind the app password.
func VerifyProfilePassword(profileName, appPassword string) error {
	return node.VerifyProfilePassword(config.DefaultConfig(profileName), []byte(appPassword))
}
