package servernode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/release"
)

// errReported marks an error whose message was already printed (a failed doctor).
var errReported = errors.New("reported")

// FreeBytes measures the free space of a file system. The main package sets it where the platform
// can tell; without it the doctor does not measure the disk.
var FreeBytes func(path string) (uint64, error)

func langOf(flagValue string, env func(string) string) string {
	if flagValue != "" {
		return normLang(flagValue)
	}
	for _, k := range []string{"ECHOFOG_LANG", "LC_ALL", "LANG"} {
		if v := env(k); v != "" {
			return normLang(v)
		}
	}
	return "en"
}

func tr(lang, ru, en string) string {
	if lang == "ru" {
		return ru
	}
	return en
}

// detectAdvertise proposes the first public IPv4 address of this machine; empty when there is none
// (behind NAT, in a container). Nothing is asked of the network.
func detectAdvertise() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
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
		if ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	return ""
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		return "echofog-data"
	}
	return "/var/lib/echofog"
}

func cmdInit(args []string, env func(string) string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	data := dataFlag(fs, env)
	var listens listFlag
	fs.Var(&listens, "listen", "suite=BIND:PORT, repeatable")
	advertise := fs.String("advertise", "", "IP literal the card advertises")
	invite := fs.String("invite-listen", "", "suite=BIND:PORT of the invitation listener")
	roles := fs.String("roles", "", "comma-separated roles")
	cover := fs.String("cover", env("ECHOFOG_COVER"), "HOST:PORT of the site the camouflaged suites imitate")
	coverCert := fs.String("cover-cert", env("ECHOFOG_COVER_CERT"), "PEM bundle (certificate chain and key) of the cover domain")
	acmeTOS := fs.Bool("acme-accept-tos", envBool(env("ECHOFOG_ACME_ACCEPT_TOS")), "accept the ACME certificate authority's terms (empty cover-cert)")
	acmeEmail := fs.String("acme-email", env("ECHOFOG_ACME_EMAIL"), "optional contact of the ACME account")
	acmeChallenge := fs.String("acme-challenge", env("ECHOFOG_ACME_CHALLENGE"), "ACME challenge: tls-alpn-01 or http-01 (default: the CA's choice)")
	acmeDirectory := fs.String("acme-directory", env("ECHOFOG_ACME_DIRECTORY"), "ACME directory URL (default: the production CA)")
	presetName := fs.String("preset", "", "relay, exit-free or home-entry")
	domain := fs.String("domain", "", "domain of the node's own cover")
	siteDir := fs.String("site-dir", env("ECHOFOG_SITE_DIR"), "folder with the site of the cover")
	port := fs.Int("port", 443, "UDP port of the node")
	yes := fs.Bool("yes", false, "do not ask, do not start the node")
	langFlag := fs.String("lang", "", "ru or en")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	lang := langOf(*langFlag, env)
	rolesGiven := false
	fs.Visit(func(f *flag.Flag) { rolesGiven = rolesGiven || f.Name == "roles" })

	var (
		cfg     Config
		notes   []Note
		dataDir = *data
		start   bool
	)
	switch {
	case *presetName != "":
		if err := needData(dataDir); err != nil {
			return err
		}
		if *advertise == "" {
			return usageError{errors.New("--advertise is required with --preset: the public IP address of this server")}
		}
		cfg, notes, err = BuildPreset(Preset(*presetName), PresetOptions{Advertise: *advertise, Port: *port, Domain: *domain, SiteDir: *siteDir})
		if err != nil {
			return usageError{err}
		}
		if *coverCert != "" {
			cfg.CoverCert = *coverCert
		}
	case len(listens) > 0:
		if err := needData(dataDir); err != nil {
			return err
		}
		cfg = DefaultConfig()
		cfg.Cover, cfg.SiteDir, cfg.CoverCert = *cover, *siteDir, *coverCert
		for _, l := range listens {
			suite, bind, ok := strings.Cut(l, "=")
			if !ok {
				return fmt.Errorf("%w: listener %q: want suite=host:port", ErrConfig, l)
			}
			cfg.Listen = append(cfg.Listen, ListenSpec{Suite: strings.TrimSpace(suite), Bind: strings.TrimSpace(bind), Advertise: *advertise})
		}
		if *invite != "" {
			suite, bind, ok := strings.Cut(*invite, "=")
			if !ok {
				return fmt.Errorf("%w: listener %q: want suite=host:port", ErrConfig, *invite)
			}
			cfg.InviteListen = &ListenSpec{Suite: strings.TrimSpace(suite), Bind: strings.TrimSpace(bind), Advertise: *advertise}
		}
	default:
		def := dataDir
		if def == "" {
			def = defaultDataDir()
		}
		res, err := RunWizard(stdin, out, WizardDefaults{Lang: lang, Advertise: detectAdvertise(), Port: 443, DataDir: def})
		if err != nil {
			return err
		}
		cfg, notes, dataDir, lang, start = res.Config, res.Notes, res.DataDir, res.Lang, res.Start
	}
	if rolesGiven {
		cfg.Roles = []string{}
		if *roles != "" {
			cfg.Roles = strings.Split(*roles, ",")
		}
	}
	// The ACME flags are the operator's act for a hand-written configuration; the presets with a
	// domain already carry accept_tos.
	if *acmeTOS || *acmeEmail != "" || *acmeChallenge != "" || *acmeDirectory != "" {
		if cfg.CoverACME == nil {
			cfg.CoverACME = &CoverACMESpec{}
		}
		cfg.CoverACME.AcceptTOS = cfg.CoverACME.AcceptTOS || *acmeTOS
		cfg.CoverACME.Email = *acmeEmail
		cfg.CoverACME.Challenge = *acmeChallenge
		cfg.CoverACME.DirectoryURL = *acmeDirectory
	}
	if err := needData(dataDir); err != nil {
		return err
	}
	phrase, err := InitLang(dataDir, cfg, lang)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s\n%s\n\n", tr(lang, "Фраза восстановления (показывается один раз; записать и хранить вне сервера):", "Recovery phrase (shown once; write it down and keep it offline):"), phrase)
	fmt.Fprintf(out, "%s %s\n%s %s\n%s %s\n", tr(lang, "данные:   ", "data:     "), dataDir, tr(lang, "роли:     ", "roles:    "), strings.Join(cfg.Roles, ","),
		tr(lang, "конфиг:   ", "config:   "), filepath.Join(dataDir, configFileName))
	for _, n := range notes {
		fmt.Fprintf(out, "[%s] %s\n", n.Level, Message(lang, n.Code))
	}
	if start && !*yes {
		return cmdRun([]string{"--data", dataDir}, env, out)
	}
	return nil
}

func cmdStatus(args []string, env func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	data := dataFlag(fs, env)
	asJSON := fs.Bool("json", false, "print the status as JSON")
	langFlag := fs.String("lang", "", "ru or en")
	_, pos, err := clientFor(fs, data, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	lang := langOf(*langFlag, env)
	c, err := newClient(*data)
	if err != nil {
		return err
	}
	var st StatusInfo
	if err := c.call("GET", "/v1/status", nil, &st); err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	}
	fmt.Fprintln(out, Message(lang, "status.title"))
	fmt.Fprintf(out, "%s %s\n%s %s\n%s %s\n%s %s\n%s %d\n%s %v\n%s %d\n%s %s\n",
		tr(lang, "версия (version):   ", "version:     "), st.Version,
		tr(lang, "работает:           ", "uptime:      "), (time.Duration(st.UptimeSeconds) * time.Second).String(),
		tr(lang, "идентификатор узла: ", "node id:     "), st.NodeID,
		tr(lang, "идентификатор устр.:", "device id:   "), st.DeviceID,
		tr(lang, "сессии:             ", "sessions:    "), st.Sessions,
		tr(lang, "публичная карточка: ", "public card: "), st.PublicCard,
		tr(lang, "приглашения:        ", "invitations: "), st.Invites,
		tr(lang, "роли:               ", "roles:       "), strings.Join(st.Roles, ","))
	for _, l := range st.Listen {
		fmt.Fprintln(out, tr(lang, "слушает:", "listen:  "), l)
	}
	if st.PublicLink != "" {
		fmt.Fprintln(out, tr(lang, "публичная ссылка:", "public link: "), st.PublicLink)
	}
	if st.ListEntry != "" {
		fmt.Fprintln(out, tr(lang, "строка для списка узлов входа:", "bootstrap list entry:            "), st.ListEntry)
	}
	for _, w := range st.Warnings {
		fmt.Fprintln(out, tr(lang, "предупреждение:", "warning:       "), w)
	}
	return nil
}

func cmdDoctor(args []string, env func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	data := dataFlag(fs, env)
	langFlag := fs.String("lang", "", "ru or en")
	dns := fs.Bool("dns", false, "look the cover domain up through the system resolver")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	if err := needData(*data); err != nil {
		return err
	}
	lang := langOf(*langFlag, env)
	deps := DoctorDeps{DataDir: *data, Version: BuildVersion, FreeBytes: FreeBytes}
	if *dns {
		deps.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if c, err := newClient(*data); err == nil {
		deps.NodeStatus = func(ctx context.Context) (*StatusInfo, error) {
			var st StatusInfo
			if err := c.call("GET", "/v1/status", nil, &st); err != nil {
				return nil, err
			}
			return &st, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results := RunDoctor(ctx, deps)
	warns, fails := 0, 0
	for _, r := range results {
		fmt.Fprintln(out, r.Text(lang))
		switch r.Level {
		case LevelWarn:
			warns++
		case LevelFail:
			fails++
		}
	}
	switch {
	case fails > 0:
		fmt.Fprintln(out, Message(lang, "doctor.summary.fail", fails))
	case warns > 0:
		fmt.Fprintln(out, Message(lang, "doctor.summary.warn", warns))
	default:
		fmt.Fprintln(out, Message(lang, "doctor.summary.ok"))
	}
	if DoctorExit(results) != 0 {
		return errReported
	}
	return nil
}

func cmdUpdate(args []string, env func(string) string, stdin io.Reader, out, errw io.Writer) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	_ = dataFlag(fs, env)
	manifest := fs.String("manifest-url", env("ECHOFOG_MANIFEST_URL"), "https URL of the signed release manifest")
	check := fs.Bool("check", false, "only look, install nothing")
	yes := fs.Bool("yes", false, "agree to the installation without asking")
	rollback := fs.Bool("allow-rollback", false, "allow installing a version below the running one (never below the signed min_version)")
	langFlag := fs.String("lang", "", "ru or en")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	lang := langOf(*langFlag, env)
	if len(ReleaseKeys) == 0 {
		return errors.New(Message(lang, "update.no-keys"))
	}
	if *manifest == "" {
		return usageError{errors.New("--manifest-url is required")}
	}
	exe := UpdateExePath
	if exe == "" {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			return errors.New(Message(lang, "update.docker"))
		}
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	hc := UpdateHTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second}}
	}
	mgr, err := release.Open(release.Config{Keys: ReleaseKeys, Clock: wallClock{}})
	if err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	deps := UpdateDeps{
		Manager: mgr, HTTP: hc, ManifestURL: *manifest, CurrentVersion: BuildVersion,
		Platform: runtime.GOOS + "-" + runtime.GOARCH, ExePath: exe, SelfTest: UpdateSelfTest, AllowRollback: *rollback,
		Confirm: func(info UpdateInfo) bool {
			if *yes {
				return true
			}
			fmt.Fprint(out, Message(lang, "update.confirm", info.Version)+" ")
			line, _ := in.ReadString('\n')
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes", "д", "да":
				return true
			}
			return false
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	info, err := CheckUpdate(ctx, deps)
	if err != nil {
		return err
	}
	if info == nil {
		fmt.Fprintln(out, Message(lang, "update.none"))
		return nil
	}
	fmt.Fprintln(out, Message(lang, "update.available", info.Version, info.Artifact, info.Size))
	if info.Rollback {
		fmt.Fprintln(out, Message(lang, "update.rollback-warning", BuildVersion, info.Version))
	}
	if *check {
		return nil
	}
	if err := ApplyUpdate(ctx, deps, *info); err != nil {
		if errors.Is(err, ErrNotConfirmed) {
			fmt.Fprintln(errw, Message(lang, "update.declined"))
		}
		return err
	}
	fmt.Fprintln(out, Message(lang, "update.done", info.Version))
	return nil
}

type wallClock struct{}

func (wallClock) Now() time.Time {
	if UpdateNow != nil {
		return UpdateNow()
	}
	return time.Now()
}

// envBool reads a boolean environment value: "1", "true", "yes" or "on" (any case).
func envBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
