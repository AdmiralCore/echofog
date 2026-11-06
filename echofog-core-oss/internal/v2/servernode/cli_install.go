package servernode

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/echofog/echofog-core/internal/v2/release"
)

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// fetchManifest downloads the signed manifest over https, bounded by the manifest size limit.
func fetchManifest(ctx context.Context, hc *http.Client, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w: https URL required", release.ErrSource)
	}
	c := *hc
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 3 {
			return release.ErrSource
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", release.ErrSource, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, release.MaxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > release.MaxManifestSize {
		return nil, release.ErrTooLarge
	}
	return b, nil
}

// cmdInstall is `echofog-node v2 install`: the downloaded binary is verified against the signed
// manifest first, then installed as a systemd service with the chosen preset.
func cmdInstall(args []string, env func(string) string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	root := fs.String("root", "", "install below this folder (image builds and tests)")
	binary := fs.String("binary", "", "the downloaded echofog-node binary")
	manifestFile := fs.String("manifest", "", "file with the signed release manifest")
	manifestURL := fs.String("manifest-url", env("ECHOFOG_MANIFEST_URL"), "https URL of the signed release manifest")
	user := fs.String("user", "echofog", "service user")
	presetName := fs.String("preset", "", "relay, exit-free or home-entry (no flag: ask)")
	domain := fs.String("domain", "", "domain of the node's own cover")
	siteDir := fs.String("site-dir", "", "folder with the site of the cover")
	port := fs.Int("port", 443, "UDP port of the node")
	advertise := fs.String("advertise", "", "public IP address of this server")
	_ = fs.Bool("yes", false, "do not ask")
	start := fs.Bool("start", false, "enable and start the service")
	langFlag := fs.String("lang", "", "ru or en")
	platform := fs.String("platform", runtime.GOOS+"-"+runtime.GOARCH, "platform of the artifact in the manifest")
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
	if *binary == "" || (*manifestFile == "" && *manifestURL == "") {
		return usageError{errors.New("--binary and --manifest (or --manifest-url) are required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var raw []byte
	if *manifestFile != "" {
		if raw, err = readLimited(*manifestFile, release.MaxManifestSize); err != nil {
			return err
		}
	} else {
		hc := UpdateHTTPClient
		if hc == nil {
			hc = &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second}}
		}
		if raw, err = fetchManifest(ctx, hc, *manifestURL); err != nil {
			return err
		}
	}

	var (
		cfg   Config
		notes []Note
	)
	if *presetName != "" {
		if *advertise == "" {
			return usageError{errors.New("--advertise is required with --preset: the public IP address of this server")}
		}
		if cfg, notes, err = BuildPreset(Preset(*presetName), PresetOptions{Advertise: *advertise, Port: *port, Domain: *domain, SiteDir: *siteDir}); err != nil {
			return usageError{err}
		}
	} else {
		res, err := RunWizard(stdin, out, WizardDefaults{Lang: lang, Advertise: detectAdvertise(), Port: 443, DataDir: installData})
		if err != nil {
			return err
		}
		cfg, notes, lang = res.Config, res.Notes, res.Lang
	}

	mgr, err := release.Open(release.Config{Keys: ReleaseKeys, Clock: wallClock{}})
	if err != nil {
		return err
	}
	euid := os.Geteuid()
	if InstallEuid != nil {
		euid = InstallEuid()
	}
	var runner Runner = execRunner{}
	if InstallRunner != nil {
		runner = InstallRunner
	}
	res, err := Install(ctx, InstallDeps{
		Root: *root, User: *user, Binary: *binary, ManifestRaw: raw, Manager: mgr, Platform: *platform, Config: cfg, Lang: lang,
		Runner: runner, Euid: euid, SelfTest: UpdateSelfTest, Start: *start,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s\n%s\n\n", tr(lang, "Фраза восстановления (показывается один раз; записать и хранить вне сервера):", "Recovery phrase (shown once; write it down and keep it offline):"), res.Phrase)
	fmt.Fprintf(out, "%s %s\n%s %s\n", tr(lang, "версия:  ", "version: "), res.Version, tr(lang, "данные:  ", "data:    "), res.DataDir)
	for _, n := range notes {
		fmt.Fprintf(out, "[%s] %s\n", n.Level, Message(lang, n.Code))
	}
	for _, a := range res.Advice {
		fmt.Fprintln(out, a)
	}
	return nil
}
