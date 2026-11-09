package servernode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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
	// Progress receives the code of each step before it runs ("verify", "user", "binary",
	// "profile", "unit", "reload", "service", "firewall", "health"). Nil is silent.
	Progress func(step string)
	// Firewall opens the node's ports in ufw or nftables, with the rollback of every rule it adds.
	// The operator's consent is the caller's business (T12.4f).
	Firewall bool
	// FirewallProbe finds a firewall tool; nil uses exec.LookPath.
	FirewallProbe func(name string) (string, error)
	// Health checks the started node; nil polls the operator API when Root is "" and does nothing
	// in a test root.
	Health func(ctx context.Context, dataDir string) error
}

// InstallResult is what the installer reports. Phrase is the recovery phrase: show it once, never log it.
type InstallResult struct {
	Phrase  string
	Version string
	DataDir string
	Advice  []string
}

// verified is a binary that passed every check, staged in a private folder of its own. The
// installer copies from the staged file, never from the path the operator gave.
type verified struct {
	version string
	stage   string // the staged copy
	dir     string // the private folder, removed by cleanup
	sum     []byte
	size    int64
}

func (v *verified) cleanup() {
	if v != nil && v.dir != "" {
		_ = os.RemoveAll(v.dir)
	}
}

// verifyBinary checks the binary against the signed manifest: two signatures of the build's keys,
// the artifact for this platform, its size and SHA-256, and a run of the binary that must report
// the manifest's version. The bytes are copied once into a private staging folder (outside the
// installation root, mode 0700) while they are hashed, and the self-test runs on that copy, so a
// file that changes afterwards, or during the test, cannot become the installed binary. Nothing is
// written to the installation root.
func verifyBinary(ctx context.Context, d InstallDeps) (*verified, error) {
	if d.Manager == nil {
		return nil, ErrNoReleaseKeys
	}
	if _, err := d.Manager.Offer(d.ManifestRaw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVerified, err)
	}
	last := d.Manager.Last()
	if last == nil || last.Release == nil {
		return nil, ErrNotVerified
	}
	rel := last.Release
	if _, ok := parseVersion(rel.GetVersion()); !ok {
		return nil, fmt.Errorf("%w: version %q", ErrNotVerified, rel.GetVersion())
	}
	version := strings.TrimPrefix(rel.GetVersion(), "v")
	var wantSize uint64
	var wantSum []byte
	found := false
	for _, a := range rel.GetArtifacts() {
		if a.GetPlatform() == d.Platform {
			wantSize, wantSum, found = a.GetSize(), a.GetSha256(), true
			break
		}
	}
	if !found {
		return nil, ErrNoArtifact
	}
	if wantSize == 0 || wantSize > maxArtifactSize || len(wantSum) != sha256.Size {
		return nil, fmt.Errorf("%w: size or hash in the manifest", ErrNotVerified)
	}
	in, err := os.Open(d.Binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVerified, err)
	}
	defer in.Close()
	dir, err := os.MkdirTemp("", "echofog-install-")
	if err != nil {
		return nil, err
	}
	v := &verified{version: version, dir: dir, stage: filepath.Join(dir, "echofog-node"), sum: wantSum, size: int64(wantSize)}
	fail := func(err error) (*verified, error) {
		v.cleanup()
		return nil, err
	}
	out, err := os.OpenFile(v.stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, int64(wantSize)+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil || uint64(n) != wantSize || !bytes.Equal(h.Sum(nil), wantSum) {
		return fail(ErrNotVerified)
	}
	test := d.SelfTest
	if test == nil {
		test = defaultSelfTest
	}
	if got, err := test(ctx, v.stage); err != nil || strings.TrimPrefix(got, "v") != version {
		return fail(fmt.Errorf("%w: the binary reported %q (%v)", ErrNotVerified, got, err))
	}
	return v, nil
}

// undoStack removes what the installer created, newest first.
type undoStack []func()

func (u *undoStack) push(f func()) { *u = append(*u, f) }

func (u undoStack) run() {
	for i := len(u) - 1; i >= 0; i-- {
		u[i]()
	}
}

// topMissing returns the outermost directory of path that does not exist yet ("" if path exists).
func topMissing(path string) string {
	top := ""
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return top
		}
		top = p
		if filepath.Dir(p) == p {
			return top
		}
	}
}

// mkdirTracked creates path and records the directories it created for the rollback.
func mkdirTracked(path string, perm os.FileMode, undo *undoStack) error {
	if top := topMissing(path); top != "" {
		undo.push(func() { _ = os.RemoveAll(top) })
	}
	return os.MkdirAll(path, perm)
}

// Install verifies the binary against the signed manifest and only then installs it as a service:
// nothing is written and no command is run before the verification passes. A failure after the
// first write takes back what this call created (the service user is left in place), so the next
// attempt starts clean and no half-installed node is left with keys and no recovery phrase shown.
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
	progress := d.Progress
	if progress == nil {
		progress = func(string) {}
	}
	progress("verify")
	v, err := verifyBinary(ctx, d)
	if err != nil {
		return nil, err
	}
	defer v.cleanup()
	at := func(p string) string { return filepath.Join(d.Root, filepath.FromSlash(p)) }
	if _, err := os.Stat(filepath.Join(at(installData), identityFileName)); err == nil {
		return nil, ErrAlreadyInstalled
	}
	unit, err := UnitFile(d.User, installData, installBin)
	if err != nil {
		return nil, err
	}

	var undo undoStack
	failed := func(step string, err error, out []byte) (*InstallResult, error) {
		undo.run()
		if len(undo) > 0 {
			_, _ = d.Runner.Run(ctx, "systemctl", "daemon-reload") // the unit is gone again
		}
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return nil, fmt.Errorf("install failed at %s and was rolled back: %w (%s)", step, err, msg)
		}
		return nil, fmt.Errorf("install failed at %s and was rolled back: %w", step, err)
	}

	if _, err := d.Runner.Run(ctx, "id", "-u", d.User); err != nil {
		progress("user")
		if out, err := d.Runner.Run(ctx, "useradd", "--system", "--user-group", "--home-dir", installData, "--shell", "/usr/sbin/nologin", d.User); err != nil {
			return failed("useradd (create the service user)", err, out)
		}
	}
	progress("binary")
	if err := mkdirTracked(filepath.Dir(at(installBin)), 0o755, &undo); err != nil {
		return failed("creating the binary folder", err, nil)
	}
	if err := copyVerified(v, at(installBin), &undo); err != nil {
		return failed("copying the binary", err, nil)
	}
	progress("profile")
	dataBefore := dirNames(at(installData))
	if top := topMissing(at(installData)); top != "" {
		undo.push(func() { _ = os.RemoveAll(top) })
	} else {
		undo.push(func() { removeNew(at(installData), dataBefore) })
	}
	phrase, err := InitLang(at(installData), d.Config, d.Lang)
	if err != nil {
		return failed("creating the profile", err, nil)
	}
	if out, err := d.Runner.Run(ctx, "chown", "-R", d.User+":"+d.User, at(installData)); err != nil {
		return failed("chown (hand the data folder to "+d.User+")", err, out)
	}
	progress("unit")
	if err := mkdirTracked(filepath.Dir(at(installUnit)), 0o755, &undo); err != nil {
		return failed("creating the unit folder", err, nil)
	}
	if err := os.WriteFile(at(installUnit), []byte(unit), 0o644); err != nil {
		return failed("writing the unit", err, nil)
	}
	undo.push(func() { _ = os.Remove(at(installUnit)) })
	progress("reload")
	if out, err := d.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return failed("systemctl daemon-reload", err, out)
	}
	if d.Start {
		progress("service")
		if out, err := d.Runner.Run(ctx, "systemctl", "enable", "--now", unitServName); err != nil {
			return failed("systemctl enable (start the service)", err, out)
		}
	}
	if d.Firewall {
		progress("firewall")
		probe := d.FirewallProbe
		if probe == nil {
			probe = exec.LookPath
		}
		tool := firewallTool(probe)
		if tool != "" {
			undoFw, err := openFirewall(ctx, d.Runner, d.Root, d.Config, tool)
			if err != nil {
				return failed("opening the firewall ports", err, nil)
			}
			undo.push(func() { undoFw(context.Background(), d.Runner) })
		}
	}
	health := d.Health
	if health == nil && d.Start && d.Root == "" {
		health = defaultHealth
	}
	if health != nil && d.Start {
		progress("health")
		if err := health(ctx, at(installData)); err != nil {
			return failed("checking the node's health", err, nil)
		}
	}
	advice := FirewallAdvice(d.Lang, d.Config)
	advice = append(advice,
		tr(d.Lang, "Запуск: systemctl enable --now echofog-node (если не запущено ключом --start)", "Start: systemctl enable --now echofog-node (if --start was not given)"),
		tr(d.Lang, "Состояние: "+installBin+" v2 status --data "+installData, "Status: "+installBin+" v2 status --data "+installData))
	return &InstallResult{Phrase: phrase, Version: v.version, DataDir: installData, Advice: advice}, nil
}

func dirNames(dir string) map[string]bool {
	out := map[string]bool{}
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			out[e.Name()] = true
		}
	}
	return out
}

// removeNew deletes what appeared in dir since it was listed as before.
func removeNew(dir string, before map[string]bool) {
	for name := range dirNames(dir) {
		if !before[name] {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}
}

// copyVerified copies the staged, verified binary to dst (temporary file and rename), hashing the
// bytes again on the way: if the staged file was touched, nothing is created.
func copyVerified(v *verified, dst string, undo *undoStack) error {
	in, err := os.Open(v.stage)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, v.size+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != v.size || !bytes.Equal(h.Sum(nil), v.sum)) {
		err = ErrNotVerified
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	undo.push(func() { _ = os.Remove(dst) })
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
	out := []string{tr(lang,
		"Открыть порты ноды в файрволе сервера (и в панели хостера): UDP для noise3 и reality2-quic, TCP для reality2-tcp, ws2 и ACME:",
		"Open the node's ports in the server firewall (and in the host's panel): UDP for noise3 and reality2-quic, TCP for reality2-tcp, ws2 and ACME:")}
	for _, r := range firewallRules(cfg) {
		if r.udp {
			out = append(out, fmt.Sprintf("  ufw allow %d/udp", r.port), fmt.Sprintf("  firewall-cmd --permanent --add-port=%d/udp", r.port))
		}
		if r.tcp {
			out = append(out, fmt.Sprintf("  ufw allow %d/tcp", r.port), fmt.Sprintf("  firewall-cmd --permanent --add-port=%d/tcp", r.port))
		}
	}
	out = append(out, "  firewall-cmd --reload",
		tr(lang, "Проверка: "+installBin+" v2 doctor --data "+installData+" (достижимость определяется по подключению пира, внешние сервисы не используются).",
			"Check: "+installBin+" v2 doctor --data "+installData+" (reachability is judged by a peer connecting; no outside service is used)."))
	return out
}

type firewallRule struct {
	port     int
	udp, tcp bool
}

// firewallRules returns the ports the node needs, split by transport.
func firewallRules(cfg Config) []firewallRule {
	type rule struct{ udp, tcp bool }
	ports := map[int]*rule{}
	add := func(port int, udp bool) {
		r := ports[port]
		if r == nil {
			r = &rule{}
			ports[port] = r
		}
		if udp {
			r.udp = true
		} else {
			r.tcp = true
		}
	}
	for _, l := range append(append([]ListenSpec(nil), cfg.Listen...), optional(cfg.InviteListen)...) {
		if _, p, err := net.SplitHostPort(l.Bind); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				add(n, udpSuite(l.Suite))
			}
		}
	}
	// ACME tls-alpn-01 validates on TCP 443 even when the node binds the cover elsewhere.
	if cfg.Cover != "" && cfg.CoverCert == "" {
		add(443, false)
	}
	nums := make([]int, 0, len(ports))
	for n := range ports {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	out := make([]firewallRule, 0, len(nums))
	for _, n := range nums {
		out = append(out, firewallRule{port: n, udp: ports[n].udp, tcp: ports[n].tcp})
	}
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
