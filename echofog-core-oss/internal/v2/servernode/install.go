package servernode

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/release"
)

var (
	// ErrNotRoot: installing a system service needs root.
	ErrNotRoot = errors.New("servernode: the installer must run as root")
	// ErrNotVerified: the binary does not match the signed manifest. Nothing was written.
	ErrNotVerified = errors.New("servernode: the binary does not match the signed release manifest")
	// ErrAlreadyInstalled: a profile exists; use update, not install.
	ErrAlreadyInstalled = errors.New("servernode: a node is already installed here; use update")
	// ErrBadUser: not a safe system user name.
	ErrBadUser = errors.New("servernode: bad service user name")
)

// Runner runs the system commands of the installer (useradd, chown, systemctl). Tests record them.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// InstallDeps are the inputs of the installer.
type InstallDeps struct {
	// Root is prepended to every path ("" is the real root); tests and image builders use a folder.
	Root string
	User string
	// Binary is the downloaded node binary; ManifestRaw the signed release manifest.
	Binary      string
	ManifestRaw []byte
	Manager     *release.Manager
	Platform    string
	Config      Config
	Lang        string
	Runner      Runner
	Euid        int
	SelfTest    func(ctx context.Context, exe string) (string, error)
	Start       bool
}

// InstallResult is what the installer reports. Phrase is the recovery phrase: show it once, never log it.
type InstallResult struct {
	Phrase  string
	Version string
	DataDir string
	Advice  []string
}

// Install verifies the binary against the signed manifest and only then installs it as a service.
func Install(ctx context.Context, d InstallDeps) (*InstallResult, error) {
	return nil, errors.New("servernode: not implemented")
}

// UnitFile is the hardened systemd unit of the node.
func UnitFile(user, dataDir, binPath string) (string, error) {
	return "", errors.New("servernode: not implemented")
}

// FirewallAdvice lists the commands that open the node's ports and the way to check them. The
// installer prints them and does not run them.
func FirewallAdvice(lang string, cfg Config) []string { return nil }

// Seams of the install command (tests).
var (
	InstallRunner Runner
	InstallEuid   func() int
)
