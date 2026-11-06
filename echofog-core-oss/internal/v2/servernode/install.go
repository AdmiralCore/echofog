package servernode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

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

// Fixed layout of an installation (relative to InstallDeps.Root).
const (
	installBin   = "/usr/local/bin/echofog-node"
	installData  = "/var/lib/echofog"
	installUnit  = "/etc/systemd/system/echofog-node.service"
	unitServName = "echofog-node"
)

var (
	userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
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

// verifyBinary checks the binary against the signed manifest: two signatures of the build's keys,
// the artifact for this platform, its size and SHA-256, and a run of the binary that must report
// the manifest's version. It writes nothing.
func verifyBinary(ctx context.Context, d InstallDeps) (version string, err error) {
	if d.Manager == nil {
		return "", ErrNoReleaseKeys
	}
	if _, err := d.Manager.Offer(d.ManifestRaw); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotVerified, err)
	}
	last := d.Manager.Last()
	if last == nil || last.Release == nil {
		return "", ErrNotVerified
	}
	rel := last.Release
	if _, ok := parseVersion(rel.GetVersion()); !ok {
		return "", fmt.Errorf("%w: version %q", ErrNotVerified, rel.GetVersion())
	}
	version = strings.TrimPrefix(rel.GetVersion(), "v")
	var art *struct {
		size uint64
		sum  []byte
	}
	for _, a := range rel.GetArtifacts() {
		if a.GetPlatform() == d.Platform {
			art = &struct {
				size uint64
				sum  []byte
			}{a.GetSize(), a.GetSha256()}
			break
		}
	}
	if art == nil {
		return "", ErrNoArtifact
	}
	f, err := os.Open(d.Binary)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotVerified, err)
	}
	defer f.Close()
	if art.size == 0 || art.size > maxArtifactSize {
		return "", fmt.Errorf("%w: size in the manifest", ErrNotVerified)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, int64(art.size)+1))
	if err != nil || uint64(n) != art.size || string(h.Sum(nil)) != string(art.sum) {
		return "", ErrNotVerified
	}
	test := d.SelfTest
	if test == nil {
		test = defaultSelfTest
	}
	if v, err := test(ctx, d.Binary); err != nil || strings.TrimPrefix(v, "v") != version {
		return "", fmt.Errorf("%w: the binary reported %q (%v)", ErrNotVerified, v, err)
	}
	return version, nil
}

// Install verifies the binary against the signed manifest and only then installs it as a service:
// nothing is written and no command is run before the verification passes.
func Install(ctx context.Context, d InstallDeps) (*InstallResult, error) {
	if !userName.MatchString(d.User) || d.User == "root" {
		return nil, ErrBadUser
	}
	if d.Root == "" && d.Euid != 0 {
		return nil, ErrNotRoot
	}
	if d.Runner == nil {
		return nil, errors.New("servernode: no command runner")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, err
	}
	version, err := verifyBinary(ctx, d)
	if err != nil {
		return nil, err
	}
	at := func(p string) string { return filepath.Join(d.Root, filepath.FromSlash(p)) }
	if _, err := os.Stat(filepath.Join(at(installData), identityFileName)); err == nil {
		return nil, ErrAlreadyInstalled
	}
	unit, err := UnitFile(d.User, installData, installBin)
	if err != nil {
		return nil, err
	}

	if _, err := d.Runner.Run(ctx, "id", "-u", d.User); err != nil {
		if out, err := d.Runner.Run(ctx, "useradd", "--system", "--user-group", "--home-dir", installData, "--shell", "/usr/sbin/nologin", d.User); err != nil {
			return nil, fmt.Errorf("create the service user: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.MkdirAll(filepath.Dir(at(installBin)), 0o755); err != nil {
		return nil, err
	}
	if err := copyExecutable(d.Binary, at(installBin)); err != nil {
		return nil, fmt.Errorf("install the binary: %w", err)
	}
	phrase, err := InitLang(at(installData), d.Config, d.Lang)
	if err != nil {
		return nil, err
	}
	if out, err := d.Runner.Run(ctx, "chown", "-R", d.User+":"+d.User, at(installData)); err != nil {
		return nil, fmt.Errorf("hand the data folder to %s: %w (%s)", d.User, err, strings.TrimSpace(string(out)))
	}
	if err := os.MkdirAll(filepath.Dir(at(installUnit)), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(at(installUnit), []byte(unit), 0o644); err != nil {
		return nil, err
	}
	if _, err := d.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return nil, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if d.Start {
		if out, err := d.Runner.Run(ctx, "systemctl", "enable", "--now", unitServName); err != nil {
			return nil, fmt.Errorf("start the service: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	advice := FirewallAdvice(d.Lang, d.Config)
	advice = append(advice,
		tr(d.Lang, "Запуск: systemctl enable --now echofog-node (если не запущено ключом --start)", "Start: systemctl enable --now echofog-node (if --start was not given)"),
		tr(d.Lang, "Состояние: "+installBin+" v2 status --data "+installData, "Status: "+installBin+" v2 status --data "+installData))
	return &InstallResult{Phrase: phrase, Version: version, DataDir: installData, Advice: advice}, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// UnitFile is the hardened systemd unit of the node: an unprivileged user, no new privileges, a
// read-only system with one writable folder, only the capability to bind a low port, and a closed
// set of address families. No secret appears in it.
func UnitFile(user, dataDir, binPath string) (string, error) {
	if !userName.MatchString(user) || user == "root" {
		return "", ErrBadUser
	}
	if !safePath.MatchString(dataDir) || !safePath.MatchString(binPath) || strings.Contains(dataDir, "..") || strings.Contains(binPath, "..") {
		return "", errors.New("servernode: unsafe path in the unit")
	}
	return `[Unit]
Description=EchoFog v2 server node
After=network-online.target
Wants=network-online.target

[Service]
User=` + user + `
Group=` + user + `
ExecStart=` + binPath + ` v2 run --data ` + dataDir + `
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ReadWritePaths=` + dataDir + `
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
LockPersonality=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
SystemCallArchitectures=native
UMask=0077
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, nil
}

// FirewallAdvice lists the commands that open the node's ports and the way to check them. The
// installer prints them and does not run them; the operator API is loopback only and never listed.
func FirewallAdvice(lang string, cfg Config) []string {
	seen := map[int]bool{}
	var ports []int
	for _, l := range append(append([]ListenSpec(nil), cfg.Listen...), optional(cfg.InviteListen)...) {
		if _, p, err := net.SplitHostPort(l.Bind); err == nil {
			if n, err := strconv.Atoi(p); err == nil && !seen[n] {
				seen[n] = true
				ports = append(ports, n)
			}
		}
	}
	sort.Ints(ports)
	out := []string{tr(lang, "Открыть UDP-порты ноды в файрволе сервера (и в панели хостера):", "Open the node's UDP ports in the server firewall (and in the host's panel):")}
	for _, p := range ports {
		out = append(out, fmt.Sprintf("  ufw allow %d/udp", p), fmt.Sprintf("  firewall-cmd --permanent --add-port=%d/udp", p))
	}
	out = append(out, "  firewall-cmd --reload",
		tr(lang, "Проверка: "+installBin+" v2 doctor --data "+installData+" (достижимость определяется по подключению пира, внешние сервисы не используются).",
			"Check: "+installBin+" v2 doctor --data "+installData+" (reachability is judged by a peer connecting; no outside service is used)."))
	return out
}

func optional(l *ListenSpec) []ListenSpec {
	if l == nil {
		return nil
	}
	return []ListenSpec{*l}
}

// Seams of the install command (tests).
var (
	InstallRunner Runner
	InstallEuid   func() int
)
