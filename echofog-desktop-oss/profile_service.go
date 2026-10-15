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
	// DeleteProfile removes a profile's directory. appPassword must unlock that profile's
	// identity: the call is reachable from the WebView, and without the password a page could wipe
	// the user's keys and data on its own.
	DeleteProfile(profileName string, appPassword string) error
	ValidateProfileName(name string) error
	IsRegistered(ctx context.Context, profileName string) bool
	GetProfileDir(profileName string) (string, error)
}

type defaultProfileService struct {
	// seams for testing; nil falls back to the real implementation
	home   func() (string, error)
	verify func(profileName, appPassword string) error
}

// NewProfileService creates a default ProfileService instance.
func NewProfileService() ProfileService {
	return &defaultProfileService{
		home:   os.UserHomeDir,
		verify: api.VerifyProfilePassword,
	}
}

func (s *defaultProfileService) homeDir() (string, error) {
	if s.home != nil {
		return s.home()
	}
	return os.UserHomeDir()
}

func (s *defaultProfileService) ValidateProfileName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("profile name cannot be empty")
	}
	if !validProfileNameRegex.MatchString(trimmed) {
		return fmt.Errorf("invalid profile name %q: must be 1-64 alphanumeric characters, dots, dashes, or underscores", trimmed)
	}
	// A leading dot lets "." and ".." (and any dot-only name) through the regex above; joined onto
	// the profiles directory they resolve to the directory itself or its parent, so DeleteProfile
	// would wipe every profile or escape the directory entirely. Reject them outright.
	if strings.HasPrefix(trimmed, ".") {
		return fmt.Errorf("invalid profile name %q: must not start with a dot", trimmed)
	}
	return nil
}

func (s *defaultProfileService) GetProfileDir(profileName string) (string, error) {
	if err := s.ValidateProfileName(profileName); err != nil {
		return "", err
	}
	home, err := s.homeDir()
	if err != nil {
		return "", fmt.Errorf("unable to resolve user home directory: %w", err)
	}
	root := filepath.Join(home, ".echofog", "profiles")
	dir := filepath.Join(root, profileName)
	// Defence in depth: whatever the name, the resolved path must be a direct child of profiles/.
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.ContainsRune(rel, filepath.Separator) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid profile name %q: resolved path escapes the profiles directory", profileName)
	}
	return dir, nil
}

func (s *defaultProfileService) GetProfiles() ([]string, error) {
	profiles, err := api.GetAvailableProfiles()
	if err != nil {
		slog.Error("Failed to fetch available profiles", "error", err)
		return nil, err
	}
	return profiles, nil
}

func (s *defaultProfileService) DeleteProfile(profileName string, appPassword string) error {
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

	if appPassword == "" {
		return errors.New("the app password is required to delete a profile")
	}
	verify := s.verify
	if verify == nil {
		verify = api.VerifyProfilePassword
	}
	if err := verify(profileName, appPassword); err != nil {
		slog.Warn("Profile deletion refused: wrong app password", "profile", profileName)
		return errors.New("wrong app password")
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
