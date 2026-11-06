package servernode

import (
	"context"
	"errors"
	"net/http"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/release"
)

var (
	// ErrDowngrade: the offered version is not newer than the running one.
	ErrDowngrade = errors.New("servernode: the offered version is not newer; rolling back is not allowed")
	// ErrNoReleaseKeys: this build has no release keys, so it cannot verify an update.
	ErrNoReleaseKeys = errors.New("servernode: this build has no release keys")
	// ErrNotConfirmed: the operator did not agree.
	ErrNotConfirmed = errors.New("servernode: the update was not confirmed")
	// ErrUpdateFailed: the new binary did not start; the old one was put back.
	ErrUpdateFailed = errors.New("servernode: the new binary did not start; the old one was restored")
)

var (
	// ErrDevBuild: a development build is never updated.
	ErrDevBuild = errors.New("servernode: a development build is not updated")
	// ErrNoArtifact: the manifest has no file for this platform.
	ErrNoArtifact = errors.New("servernode: the manifest has no artifact for this platform")
)

// Build-time and test seams of the update command. ReleaseKeys are the release public keys of the
// build (set by the main package; empty in an unofficial build).
var (
	ReleaseKeys      []keys.AKey
	UpdateHTTPClient *http.Client
	UpdateExePath    string
	BuildVersion     = "dev"
	UpdateSelfTest   func(ctx context.Context, exe string) (string, error)
)

// UpdateInfo describes an update that the signed manifest offers.
type UpdateInfo struct {
	Version  string
	Artifact string
	URL      string
	Size     uint64
	SHA256   []byte
	NotesURL string
}

// UpdateDeps are the inputs of the update.
type UpdateDeps struct {
	Manager        *release.Manager
	HTTP           *http.Client
	ManifestURL    string
	CurrentVersion string
	Platform       string
	ExePath        string
	Confirm        func(info UpdateInfo) bool
	SelfTest       func(ctx context.Context, exe string) (version string, err error)
}

// CheckUpdate fetches the manifest and returns the update for this platform, or nil.
func CheckUpdate(ctx context.Context, d UpdateDeps) (*UpdateInfo, error) {
	return nil, errors.New("servernode: not implemented")
}

// ApplyUpdate downloads, verifies and installs an update after the operator confirmed it.
func ApplyUpdate(ctx context.Context, d UpdateDeps, info UpdateInfo) error {
	return errors.New("servernode: not implemented")
}
