package servernode

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/echofog/echofog-core/internal/v2/release"
)

// ErrSetupCancelled: the operator left the setup before applying anything.
var ErrSetupCancelled = errors.New("servernode: setup cancelled, nothing was changed")

// SetupIsTerminal says whether stdin is a terminal. Tests replace it.
var SetupIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// SetupAdvertise proposes the address of the machine for the setup questions. Tests replace it.
var SetupAdvertise = detectAdvertise

// SetupFirewallProbe and SetupHealth are seams of the setup command: tests use them for the
// firewall and the health steps. Nil means the real host.
var (
	SetupFirewallProbe func(name string) (string, error)
	SetupHealth        func(ctx context.Context, dataDir string) error
)

// SetupAnswers are the answers of the setup, from the TUI or from the flags.
type SetupAnswers struct {
	Lang      string
	Preset    Preset
	Advertise string
	Port      int
	Domain    string
	SiteDir   string
	// CoverCert is the operator's own PEM bundle; empty means ACME when a domain is set.
	CoverCert string
	// Carriers is the chosen set; empty means the preset default.
	Carriers      []string
	Invites       bool
	BandwidthMbps int
	Sessions      int
	Roles         []string
	DataDir       string
	OpenPorts     bool
	Start         bool
}

// SetupPlan is everything the setup decided before any change on disk.
type SetupPlan struct {
	Lang      string
	Preset    Preset
	Answers   SetupAnswers
	Config    Config
	Notes     []Note
	DataDir   string
	OpenPorts bool
	Start     bool
}

// FinalInfo is what the operator sees once the node runs. Phrase and Token are secrets: show them
// once, never log them, never print them in the plain mode (it goes to files instead).
type FinalInfo struct {
	Version       string
	DataDir       string
	PublicLink    string
	InviteLink    string
	Phrase        string
	Token         string
	CoverCertDays int
	Warnings      []string
	Advice        []string
}

// DefaultAnswers are the answers Enter gives in the setup.
func DefaultAnswers(preset Preset, lang, advertise string, port int, dataDir string) SetupAnswers {
	if port == 0 {
		port = 443
	}
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	return SetupAnswers{
		Lang: lang, Preset: preset, Advertise: advertise, Port: port, DataDir: dataDir,
		Start: true, OpenPorts: false,
	}
}

// PlanFromAnswers turns the answers into a configuration. It changes nothing on disk.
func PlanFromAnswers(a SetupAnswers) (SetupPlan, error) {
	lang := normLang(a.Lang)
	opts := PresetOptions{
		Advertise: a.Advertise, Port: a.Port, Domain: a.Domain, SiteDir: a.SiteDir,
		Carriers: a.Carriers, Invites: a.Invites, Roles: a.Roles, Visibility: "",
	}
	if a.BandwidthMbps > 0 || a.Sessions > 0 {
		opts.Limits = &LimitsSpec{BandwidthMbps: a.BandwidthMbps, Sessions: a.Sessions}
	}
	cfg, notes, err := BuildPreset(a.Preset, opts)
	if err != nil {
		return SetupPlan{}, err
	}
	if a.CoverCert != "" {
		if a.Domain == "" {
			return SetupPlan{}, fmt.Errorf("%w: a certificate needs a cover domain", ErrConfig)
		}
		cfg.CoverCert = a.CoverCert
		cfg.CoverACME = nil
	}
	if err := cfg.Validate(); err != nil {
		return SetupPlan{}, err
	}
	return SetupPlan{Lang: lang, Preset: a.Preset, Answers: a, Config: cfg, Notes: notes, DataDir: a.DataDir, OpenPorts: a.OpenPorts, Start: a.Start}, nil
}

// SetupSummary lists what applying the plan will do. It is the same text in the TUI and in the
// plain mode, and it never contains a secret.
func SetupSummary(plan SetupPlan, width int) []string {
	cfg := plan.Config
	lines := []string{
		Message(plan.Lang, "setup.summary.preset", string(plan.Preset)),
		Message(plan.Lang, "setup.summary.roles", strings.Join(cfg.Roles, ", ")),
		Message(plan.Lang, "setup.summary.listen", listenText(cfg)),
	}
	cover := Message(plan.Lang, "setup.summary.no-cover")
	if cfg.Cover != "" {
		cover = Message(plan.Lang, "setup.summary.cover", cfg.Cover, cfg.SiteDir)
	}
	lines = append(lines,
		cover,
		Message(plan.Lang, "setup.summary.address", plan.Answers.Advertise, plan.Answers.Port),
		Message(plan.Lang, "setup.summary.files", filepath.ToSlash(filepath.Join(plan.DataDir, configFileName)), installUnit),
	)
	if plan.OpenPorts {
		var rules []string
		for _, r := range firewallRules(cfg) {
			if r.udp {
				rules = append(rules, fmt.Sprintf("%d/udp", r.port))
			}
			if r.tcp {
				rules = append(rules, fmt.Sprintf("%d/tcp", r.port))
			}
		}
		lines = append(lines, Message(plan.Lang, "setup.summary.firewall", strings.Join(rules, " ")))
	} else {
		lines = append(lines, Message(plan.Lang, "setup.summary.no-firewall"))
	}
	if plan.Config.InviteListen != nil {
		lines = append(lines, Message(plan.Lang, "setup.summary.invites", plan.Config.InviteListen.Bind))
	}
	lines = append(lines, Message(plan.Lang, "setup.summary.service"))
	for _, n := range plan.Notes {
		lines = append(lines, fmt.Sprintf("[%s] %s", n.Level, Message(plan.Lang, n.Code)))
	}
	return wrapLines(lines, width)
}

func listenText(cfg Config) string {
	var out []string
	for _, l := range cfg.Listen {
		out = append(out, l.Suite+" "+l.Bind)
	}
	if cfg.InviteListen != nil {
		out = append(out, "invite "+cfg.InviteListen.Bind)
	}
	return strings.Join(out, ", ")
}

// SystemCheckDeps are the inputs of the welcome-screen checks. A nil probe is skipped.
type SystemCheckDeps struct {
	Root      string
	DataDir   string
	Ports     []int
	Euid      func() int
	Runner    Runner
	FreeBytes func(path string) (uint64, error)
	LookPath  func(name string) (string, error)
	ListenUDP func(addr string) error
	ListenTCP func(addr string) error
}

// RunSystemChecks checks the machine before the questions: it is what the operator must fix first.
// It reuses the doctor's levels and messages and adds the host checks the doctor does not do.
func RunSystemChecks(d SystemCheckDeps) []CheckResult {
	euid := os.Geteuid
	if d.Euid != nil {
		euid = d.Euid
	}
	runner := d.Runner
	if runner == nil {
		runner = execRunner{}
	}
	look := d.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if d.ListenUDP == nil {
		d.ListenUDP = listenUDP
	}
	if d.ListenTCP == nil {
		d.ListenTCP = listenTCP
	}
	if len(d.Ports) == 0 {
		d.Ports = []int{443}
	}
	var out []CheckResult
	add := func(id string, l Level, code string, args ...any) {
		out = append(out, CheckResult{ID: id, Level: l, Code: code, Args: args})
	}

	switch {
	case d.Root != "":
		add("root", LevelOK, "setup.check.root.ok")
	case euid() == 0:
		add("root", LevelOK, "setup.check.root.ok")
	default:
		add("root", LevelFail, "setup.check.root.fail")
	}

	_, sdErr := os.Stat("/run/systemd/system")
	switch {
	case d.Root != "":
		add("systemd", LevelOK, "setup.check.systemd.ok")
	case sdErr == nil:
		// systemd is there; a live installation also asks systemctl, which the runner sees.
		if out, err := runner.Run(context.Background(), "systemctl", "--version"); err != nil {
			add("systemd", LevelFail, "setup.check.systemd.fail", strings.TrimSpace(string(out)))
		} else {
			add("systemd", LevelOK, "setup.check.systemd.ok")
		}
	default:
		add("systemd", LevelFail, "setup.check.systemd.fail", "no /run/systemd/system")
	}

	busy := 0
	for _, p := range d.Ports {
		for _, proto := range []string{"tcp", "udp"} {
			var err error
			if proto == "udp" {
				err = d.ListenUDP(fmt.Sprintf("0.0.0.0:%d", p))
			} else {
				err = d.ListenTCP(fmt.Sprintf("0.0.0.0:%d", p))
			}
			if err != nil {
				busy++
			}
		}
	}
	if busy == 0 {
		add("ports", LevelOK, "setup.check.ports.ok", portList(d.Ports))
	} else {
		add("ports", LevelFail, "setup.check.ports.busy", portList(d.Ports))
	}

	if _, err := look("timedatectl"); err != nil {
		add("clock", LevelInfo, "setup.check.clock.unknown")
	} else if out, err := runner.Run(context.Background(), "timedatectl", "show", "-p", "NTPSynchronized", "--value"); err != nil {
		add("clock", LevelInfo, "setup.check.clock.unknown")
	} else {
		switch strings.TrimSpace(string(out)) {
		case "yes":
			add("clock", LevelOK, "setup.check.clock.ok")
		case "no":
			add("clock", LevelWarn, "setup.check.clock.warn")
		default:
			add("clock", LevelInfo, "setup.check.clock.unknown")
		}
	}

	if d.FreeBytes == nil {
		add("disk", LevelInfo, "setup.check.disk.unknown")
	} else {
		dir := d.DataDir
		if dir == "" {
			dir = "/"
		}
		if free, err := d.FreeBytes(dir); err == nil {
			mb := int(free >> 20)
			switch {
			case free < diskFailBelow:
				add("disk", LevelFail, "setup.check.disk.fail", mb)
			case free < diskWarnBelow:
				add("disk", LevelWarn, "setup.check.disk.low", mb)
			default:
				add("disk", LevelOK, "setup.check.disk.ok", mb)
			}
		} else {
			add("disk", LevelInfo, "setup.check.disk.unknown")
		}
	}

	if hasGlobalIPv6() {
		add("ipv6", LevelOK, "setup.check.ipv6.ok")
	} else {
		add("ipv6", LevelInfo, "setup.check.ipv6.none")
	}

	switch firewallTool(look) {
	case "ufw":
		add("firewall", LevelOK, "setup.check.firewall.ok", "ufw")
	case "nft":
		add("firewall", LevelOK, "setup.check.firewall.ok", "nftables")
	default:
		add("firewall", LevelInfo, "setup.check.firewall.none")
	}

	if _, err := look("docker"); err == nil {
		add("docker", LevelInfo, "setup.check.docker.present")
	} else {
		add("docker", LevelInfo, "setup.check.docker.absent")
	}
	return out
}

func portList(ports []int) string {
	var out []string
	for _, p := range ports {
		out = append(out, strconv.Itoa(p))
	}
	return strings.Join(out, ", ")
}

// hasGlobalIPv6 reports a global unicast IPv6 address on this machine.
func hasGlobalIPv6() bool {
	addrs, err := netInterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is6() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// netInterfaceAddrs is a seam for hasGlobalIPv6 (tests provide their own list).
var netInterfaceAddrs = net.InterfaceAddrs

// ---- the setup command

// cmdSetup is `echofog-node v2 setup`: the TUI installer and its plain form. Nothing is written
// before the summary is confirmed.
func cmdSetup(args []string, env func(string) string, stdin io.Reader, out, errw io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	root := fs.String("root", "", "install below this folder (image builds and tests)")
	binary := fs.String("binary", "", "the verified node binary (default: this executable)")
	manifestFile := fs.String("manifest", "", "file with the signed release manifest")
	manifestURL := fs.String("manifest-url", env("ECHOFOG_MANIFEST_URL"), "https URL of the signed release manifest")
	user := fs.String("user", "echofog", "service user")
	presetName := fs.String("preset", "", "relay, exit-free, home-entry, exit-private or own")
	domain := fs.String("domain", "", "domain of the node's own cover")
	siteDir := fs.String("site-dir", "", "folder with the site of the cover")
	coverCert := fs.String("cover-cert", "", "PEM bundle (certificate chain and key) of the cover domain")
	port := fs.Int("port", 443, "public port of the node")
	advertise := fs.String("advertise", "", "public IP address of this server")
	data := fs.String("data", env("ECHOFOG_DATA_DIR"), "data folder")
	roles := fs.String("roles", "", "comma-separated roles for --preset own")
	carriers := fs.String("carriers", "", "comma-separated carriers: noise3, reality2-quic, reality2-tcp, ws2")
	invites := fs.Bool("invite", false, "add the invitation listener")
	openPorts := fs.Bool("open-ports", false, "open the node's ports in ufw or nftables")
	bandwidth := fs.Int("bandwidth", 0, "shared bandwidth limit in Mbit/s (0: no limit)")
	sessions := fs.Int("sessions", 0, "live session limit (0: default)")
	yes := fs.Bool("yes", false, "do not ask, apply the flags as they are")
	plain := fs.Bool("plain", false, "no full-screen interface, line-by-line output only")
	langFlag := fs.String("lang", "", "ru or en")
	platform := fs.String("platform", runtimePlatform(), "platform of the artifact in the manifest")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	lang := langOf(*langFlag, env)
	if len(ReleaseKeys) == 0 {
		return errors.New(Message(lang, "update.no-keys"))
	}
	if *binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		*binary = exe
	}
	if *manifestFile == "" && *manifestURL == "" {
		return usageError{errors.New("--manifest (or --manifest-url) is required: the signed manifest is the trust root")}
	}
	if *data == "" {
		*data = defaultDataDir()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	raw, err := readManifest(ctx, *manifestFile, *manifestURL)
	if err != nil {
		return err
	}
	mgr, err := release.Open(release.Config{Keys: ReleaseKeys, Clock: wallClock{}})
	if err != nil {
		return err
	}
	euid := os.Geteuid()
	if InstallEuid != nil {
		euid = InstallEuid()
	}
	runner := Runner(execRunner{})
	if InstallRunner != nil {
		runner = InstallRunner
	}

	interactive := !*plain && !*yes && SetupIsTerminal()
	color := !*plain && env("NO_COLOR") == "" && SetupIsTerminal()

	installDeps := InstallDeps{
		Root: *root, User: *user, Binary: *binary, ManifestRaw: raw, Manager: mgr,
		Platform: *platform, Lang: lang, Runner: runner, Euid: euid, SelfTest: UpdateSelfTest, Start: true,
		FirewallProbe: SetupFirewallProbe, Health: SetupHealth,
	}
	apply := func(ctx context.Context, plan *SetupPlan, progress func(string)) (*FinalInfo, error) {
		return applySetup(ctx, installDeps, *plan, progress)
	}
	checks := func(ctx context.Context) []CheckResult {
		return RunSystemChecks(SystemCheckDeps{
			Root: *root, DataDir: *data, Ports: []int{*port}, Euid: func() int { return euid },
			Runner: runner, FreeBytes: FreeBytes, LookPath: exec.LookPath,
		})
	}

	var plan *SetupPlan
	// The flags always seed the answers, in every mode. The wizard asks about what it needs and
	// the flags win over its answers; --yes takes the seed as it is.
	seed := DefaultAnswers(PresetRelay, lang, *advertise, *port, *data)
	if *presetName != "" {
		seed.Preset = Preset(*presetName)
	}
	seed.Domain, seed.SiteDir, seed.CoverCert = *domain, *siteDir, *coverCert
	seed.Roles, seed.Carriers = splitList(*roles), splitList(*carriers)
	seed.Invites, seed.OpenPorts = *invites, *openPorts
	seed.BandwidthMbps, seed.Sessions = *bandwidth, *sessions
	if seed.Advertise == "" {
		seed.Advertise = SetupAdvertise()
	}
	switch {
	case interactive:
		res, err := SetupTUI(ctx, UIOptions{
			Lang: lang, Advertise: seed.Advertise, DataDir: *data, Port: *port, Color: color, Seed: seed,
		}, UIDeps{Version: BuildVersion, Checks: checks, Apply: apply})
		if err != nil {
			if errors.Is(err, ErrSetupCancelled) {
				fmt.Fprintln(out, Message(lang, "setup.cancelled"))
				return nil
			}
			return err
		}
		if res == nil || res.Plan == nil {
			return errors.New("servernode: the setup screen returned no plan")
		}
		plan = res.Plan
		if res.Applied {
			// The screen owns the apply: a second Install would see the fresh profile and fail.
			return printTUIResult(out, plan, res.Info)
		}
	case *yes:
		if *presetName == "" || *advertise == "" {
			return usageError{errors.New("--yes needs --preset and --advertise")}
		}
		p, err := PlanFromAnswers(seed)
		if err != nil {
			return usageError{err}
		}
		plan = &p
	default:
		// Plain mode: the line-by-line wizard, then one summary and one confirmation. One buffered
		// reader serves both, so piped answers are not lost between the questions and the confirm.
		br := bufio.NewReader(stdin)
		res, err := runWizard(br, out, WizardDefaults{Lang: lang, Advertise: seed.Advertise, Port: seed.Port, DataDir: seed.DataDir})
		if err != nil {
			return err
		}
		answers := DefaultAnswers(res.Preset, res.Lang, res.Opts.Advertise, res.Opts.Port, res.DataDir)
		answers.Domain, answers.SiteDir = res.Opts.Domain, res.Opts.SiteDir
		answers.Invites = res.Config.InviteListen != nil
		answers.Start = res.Start
		// The flags are the same installer as --yes: an explicit flag wins over the wizard answer.
		if given["lang"] {
			answers.Lang = lang
		}
		if given["preset"] {
			answers.Preset = Preset(*presetName)
		}
		if given["domain"] {
			answers.Domain = *domain
		}
		if given["site-dir"] {
			answers.SiteDir = *siteDir
		}
		if given["cover-cert"] {
			answers.CoverCert = *coverCert
		}
		if given["carriers"] {
			answers.Carriers = splitList(*carriers)
		}
		if given["roles"] {
			answers.Roles = splitList(*roles)
		}
		if given["invite"] {
			answers.Invites = *invites
		}
		if given["open-ports"] {
			answers.OpenPorts = *openPorts
		}
		if given["bandwidth"] {
			answers.BandwidthMbps = *bandwidth
		}
		if given["sessions"] {
			answers.Sessions = *sessions
		}
		if given["advertise"] {
			answers.Advertise = *advertise
		}
		if given["port"] {
			answers.Port = *port
		}
		if given["data"] {
			answers.DataDir = *data
		}
		p, err := PlanFromAnswers(answers)
		if err != nil {
			return err
		}
		plan = &p
		fmt.Fprintln(out, "\n"+Message(plan.Lang, "setup.summary.title"))
		for _, line := range SetupSummary(*plan, 100) {
			fmt.Fprintln(out, " ", line)
		}
		ok, err := confirmReader(br, out, plan.Lang)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, Message(plan.Lang, "setup.cancelled"))
			return nil
		}
	}
	if plan == nil {
		return errors.New("servernode: the setup produced no plan")
	}

	progress := func(step string) {
		fmt.Fprintln(out, "  "+Message(plan.Lang, "setup.apply.step."+step))
	}
	info, err := apply(ctx, plan, progress)
	if err != nil {
		return err
	}
	return printPlainFinal(out, plan, info)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runtimePlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

func readManifest(ctx context.Context, file, url string) ([]byte, error) {
	if file != "" {
		return readLimited(file, release.MaxManifestSize)
	}
	hc := UpdateHTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second}}
	}
	return fetchManifest(ctx, hc, url)
}

// confirmReader asks one yes/no question on the reader the wizard already buffered.
func confirmReader(in *bufio.Reader, out io.Writer, lang string) (bool, error) {
	fmt.Fprint(out, Message(lang, "setup.plain.confirm")+" ")
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		return false, fmt.Errorf("%w: %v", ErrWizardInput, err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "д", "да":
		return true, nil
	}
	return false, nil
}

// printPlainFinal shows the result without a full-screen interface. The recovery phrase and the
// operator token never go to the plain output: they live in files under the data folder.
func printPlainFinal(out io.Writer, plan *SetupPlan, info *FinalInfo) error {
	return printFinal(out, plan, info, true)
}

// printTUIResult is the short text after the full-screen pass. The operator confirmed the phrase
// on its screen and the file is deleted, so pointing at that file would be wrong.
func printTUIResult(out io.Writer, plan *SetupPlan, info *FinalInfo) error {
	return printFinal(out, plan, info, false)
}

func printFinal(out io.Writer, plan *SetupPlan, info *FinalInfo, phraseFile bool) error {
	fmt.Fprintln(out, "\n"+Message(plan.Lang, "setup.done.title"))
	if info.PublicLink != "" {
		fmt.Fprintln(out, Message(plan.Lang, "setup.done.link", info.PublicLink))
	} else {
		fmt.Fprintln(out, Message(plan.Lang, "setup.done.no-link"))
	}
	if info.InviteLink != "" {
		fmt.Fprintln(out, Message(plan.Lang, "setup.done.invite", info.InviteLink))
	}
	// The installer decides where the profile really lives; the wizard answer may differ.
	dataDir := plan.DataDir
	if info.DataDir != "" {
		dataDir = info.DataDir
	}
	if phraseFile {
		fmt.Fprintln(out, Message(plan.Lang, "setup.done.phrase-file", filepath.ToSlash(recoveryPath(dataDir))))
	}
	fmt.Fprintln(out, Message(plan.Lang, "setup.done.token-file", filepath.ToSlash(filepath.Join(dataDir, tokenFileName))))
	fmt.Fprintln(out, Message(plan.Lang, "setup.done.status", dataDir))
	for _, w := range info.Warnings {
		fmt.Fprintln(out, "  "+w)
	}
	for _, a := range info.Advice {
		fmt.Fprintln(out, "  "+a)
	}
	return nil
}

// recoveryPath is where the phrase waits for the operator in the plain mode.
func recoveryPath(dataDir string) string {
	return filepath.Join(dataDir, "recovery-phrase.txt")
}

// removeFile deletes a file and treats "already gone" as success.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// applySetup installs the plan and collects what the final screen shows. Install's own rollback is
// the safety net: a failed step leaves nothing behind.
func applySetup(ctx context.Context, d InstallDeps, plan SetupPlan, progress func(string)) (*FinalInfo, error) {
	d.Config = plan.Config
	d.Lang = plan.Lang
	d.Start = plan.Start
	d.Firewall = plan.OpenPorts
	d.Progress = progress
	res, err := Install(ctx, d)
	if err != nil {
		return nil, err
	}
	at := func(p string) string { return filepath.Join(d.Root, filepath.FromSlash(p)) }
	dataDir := at(res.DataDir)
	info := &FinalInfo{Version: res.Version, DataDir: dataDir, Phrase: res.Phrase, Advice: res.Advice}
	// The phrase goes to a private file: the full-screen mode shows it once and deletes the file
	// after the operator confirms, the plain mode leaves it for the operator to copy. The install
	// already succeeded, so a failed write is a warning about the file, never a rollback: the
	// phrase is still on the final screen of the full-screen mode.
	if err := writePrivate(filepath.Join(dataDir, "recovery-phrase.txt"), []byte(res.Phrase+"\n")); err != nil {
		info.Warnings = append(info.Warnings, Message(plan.Lang, "setup.warn.phrase-file", err.Error()))
	}
	if tok, err := NewTokenFile(filepath.Join(dataDir, tokenFileName)); err == nil {
		info.Token = tok
	} else {
		info.Warnings = append(info.Warnings, Message(plan.Lang, "setup.warn.token-file", err.Error()))
	}
	if !plan.Start || d.Root != "" {
		// A test or image root has no live node to ask; the health step is the seam's business.
		return info, nil
	}
	st, err := waitStatus(ctx, dataDir, 30*time.Second)
	if err != nil {
		info.Warnings = append(info.Warnings, Message(plan.Lang, "setup.warn.no-status", err.Error()))
		return info, nil
	}
	info.PublicLink = st.PublicLink
	info.Warnings = append(info.Warnings, st.Warnings...)
	if st.CoverCertNotAfter > 0 {
		info.CoverCertDays = int(time.Until(time.Unix(st.CoverCertNotAfter, 0)).Hours() / 24)
	}
	if plan.Config.InviteListen != nil {
		if c, err := newClient(dataDir); err == nil {
			var res struct {
				Link string `json:"link"`
			}
			if err := c.call("POST", "/v1/invites", map[string]any{"access": "30d", "link": "7d", "max_entries": 0}, &res); err == nil {
				info.InviteLink = res.Link
			}
		}
	}
	return info, nil
}

// waitStatus asks the running node for its status until it answers or the deadline passes.
func waitStatus(ctx context.Context, dataDir string, d time.Duration) (*StatusInfo, error) {
	deadline := time.Now().Add(d)
	var last error
	for {
		c, err := newClient(dataDir)
		if err == nil {
			var st StatusInfo
			if err = c.call("GET", "/v1/status", nil, &st); err == nil {
				return &st, nil
			}
		}
		last = err
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, last
}
