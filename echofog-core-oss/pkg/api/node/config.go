package node

import "github.com/echofog/echofog-core/internal/config"

func GetDefaultConfig(profileName string) *config.AppConfig {
	return config.DefaultConfig(profileName)
}

func GetAvailableProfiles() ([]string, error) {
	return config.GetAvailableProfiles()
}
