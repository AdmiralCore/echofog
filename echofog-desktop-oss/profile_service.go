package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	api "github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

var validProfileNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-\.]{1,64}$`)

// ProfileService defines the contract for managing user profiles and storage vaults.
type ProfileService interface {
	GetProfiles() ([]string, error)
	DeleteProfile(profileName string) error
	ValidateProfileName(name string) error
	IsRegistered(ctx context.Context, profileName string) bool
	GetProfileDir(profileName string) (string, error)
}

type defaultProfileService struct{}

// NewProfileService creates a default ProfileService instance.
func NewProfileService() ProfileService {
	return &defaultProfileService{}
}

func (s *defaultProfileService) ValidateProfileName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("profile name cannot be empty")
	}
	if !validProfileNameRegex.MatchString(trimmed) {
		return fmt.Errorf("invalid profile name %q: must be 1-64 alphanumeric characters, dots, dashes, or underscores", trimmed)
	}
	return nil
}

func (s *defaultProfileService) GetProfileDir(profileName string) (string, error) {
	if err := s.ValidateProfileName(profileName); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("unable to resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".echofog", "profiles", profileName), nil
}

func (s *defaultProfileService) GetProfiles() ([]string, error) {
	profiles, err := api.GetAvailableProfiles()
	if err != nil {
		slog.Error("Failed to fetch available profiles", "error", err)
		return nil, err
	}
	return profiles, nil
}

func (s *defaultProfileService) DeleteProfile(profileName string) error {
	if err := s.ValidateProfileName(profileName); err != nil {
		return err
	}

	profilePath, err := s.GetProfileDir(profileName)
	if err != nil {
		return err
	}

	if _, err := os.Stat(profilePath); os.IsNotExist(err) {
		return errors.New("profile does not exist")
	}

	if err := os.RemoveAll(profilePath); err != nil {
		slog.Error("Failed to remove profile directory", "profile", profileName, "path", profilePath, "error", err)
		return fmt.Errorf("failed to delete profile %q: %w", profileName, err)
	}

	slog.Info("Successfully deleted profile", "profile", profileName)
	return nil
}

func (s *defaultProfileService) IsRegistered(ctx context.Context, profileName string) bool {
	if profileName == "" {
		profileName = "default"
	}
	cfg := api.GetDefaultConfig(profileName)
	tempNode := coreNode.NewNode(ctx, cfg, "CheckReg-"+profileName)
	return tempNode.IsRegistered()
}
