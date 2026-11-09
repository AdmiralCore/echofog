package servernode_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

type recRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *recRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if name == "id" {
		return nil, errors.New("no such user") // the service user does not exist yet
	}
	return nil, nil
}

func (r *recRunner) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, "\n")
}

type instRig struct {
	*updRig
	root   string
	runner *recRunner
	deps   servernode.InstallDeps
	bin    string
}

func newInstRig(t *testing.T) *instRig {
	t.Helper()
	u := newUpdRig(t)
	bin := filepath.Join(t.TempDir(), "echofog-node")
	if err := os.WriteFile(bin, u.artifact, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := servernode.BuildPreset(servernode.PresetRelay, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	r := &instRig{updRig: u, root: t.TempDir(), runner: &recRunner{}, bin: bin}
	r.deps = servernode.InstallDeps{
		Root: r.root, User: "echofog", Binary: bin, ManifestRaw: append([]byte(nil), *u.manifest.Load()...), Manager: u.mgr,
		Platform: "linux-amd64", Config: cfg, Lang: "en", Runner: r.runner, Euid: 1000,
		SelfTest: func(context.Context, string) (string, error) { return "0.4.0", nil },
	}
	return r
}

func entries(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestInstallHappyPath(t *testing.T) {
	r := newInstRig(t)
	res, err := servernode.Install(context.Background(), r.deps)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Version != "0.4.0" || len(strings.Fields(res.Phrase)) != 12 {
		t.Fatalf("result version %q, phrase of %d words", res.Version, len(strings.Fields(res.Phrase)))
	}
	bin := filepath.Join(r.root, "usr/local/bin/echofog-node")
	if b, err := os.ReadFile(bin); err != nil || !bytes.Equal(b, r.artifact) {
		t.Fatalf("installed binary: %v", err)
	}
	data := filepath.Join(r.root, "var/lib/echofog")
	for _, name := range []string{"server-identity.json", "server.json", "operator.token"} {
		fi, err := os.Stat(filepath.Join(data, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v", name, fi.Mode())
		}
	}
	if fi, _ := os.Stat(data); os.PathSeparator == '/' && fi.Mode().Perm() != 0o700 {
		t.Errorf("data directory mode %v", fi.Mode())
	}
	raw, err := os.ReadFile(filepath.Join(data, "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := servernode.ParseConfig(raw); err != nil {
		t.Fatalf("saved config: %v", err)
	}
	unit, err := os.ReadFile(filepath.Join(r.root, "etc/systemd/system/echofog-node.service"))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/usr/local/bin/echofog-node v2 run --data /var/lib/echofog") {
		t.Fatalf("unit: %v\n%s", err, unit)
	}
	calls := r.runner.all()
	for _, want := range []string{"useradd", "chown -R echofog:echofog", "systemctl daemon-reload"} {
		if !strings.Contains(calls, want) {
			t.Errorf("the installer did not run %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "enable") || strings.Contains(calls, "start") {
		t.Errorf("the service was started without --start:\n%s", calls)
	}
	// Secrets never reach a command line, the advice or the data of the result other than Phrase.
	tok, _ := os.ReadFile(filepath.Join(data, "operator.token"))
	all := calls + strings.Join(res.Advice, "\n")
	if strings.Contains(all, strings.TrimSpace(string(tok))) || strings.Contains(all, res.Phrase) || strings.Contains(all, strings.Fields(res.Phrase)[0]+" "+strings.Fields(res.Phrase)[1]) {
		t.Error("a secret leaked into a command or the advice")
	}
	if len(res.Advice) == 0 {
		t.Error("no advice (ports, firewall, checking reachability)")
	}
}

func TestInstallStartsOnRequest(t *testing.T) {
	r := newInstRig(t)
	r.deps.Start = true
	if _, err := servernode.Install(context.Background(), r.deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.runner.all(), "systemctl enable --now echofog-node") {
		t.Errorf("calls:\n%s", r.runner.all())
	}
}

// Verification comes first: when it fails nothing exists afterwards and no command ran.
func TestInstallVerifiesBeforeAnySideEffect(t *testing.T) {
	cases := map[string]struct {
		mutate func(*instRig)
		want   error
	}{
		"binary differs from the manifest": {func(r *instRig) {
			_ = os.WriteFile(r.deps.Binary, []byte("#!trojan\n"), 0o755)
		}, servernode.ErrNotVerified},
		"one signature of three": {func(r *instRig) {
			r.publish("0.5.0", r.signers[0])
			r.deps.ManifestRaw = append([]byte(nil), *r.manifest.Load()...)
		}, nil},
		"manifest of foreign keys": {func(r *instRig) {
			r.publish("0.5.0", relSigner(t, 8), relSigner(t, 9))
			r.deps.ManifestRaw = append([]byte(nil), *r.manifest.Load()...)
		}, nil},
		"garbage manifest":             {func(r *instRig) { r.deps.ManifestRaw = []byte("not a manifest") }, nil},
		"no release keys in the build": {func(r *instRig) { r.deps.Manager = nil }, servernode.ErrNoReleaseKeys},
		"no artifact for the platform": {func(r *instRig) { r.deps.Platform = "plan9-386" }, servernode.ErrNoArtifact},
		"the binary reports another version": {func(r *instRig) {
			r.deps.SelfTest = func(context.Context, string) (string, error) { return "0.3.0", nil }
		}, servernode.ErrNotVerified},
		"the binary does not start": {func(r *instRig) {
			r.deps.SelfTest = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
		}, servernode.ErrNotVerified},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newInstRig(t)
			c.mutate(r)
			_, err := servernode.Install(context.Background(), r.deps)
			if err == nil {
				t.Fatal("an unverified install succeeded")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if e := entries(t, r.root); len(e) != 0 {
				t.Errorf("files exist after a failed verification: %v", e)
			}
			if calls := r.runner.all(); calls != "" {
				t.Errorf("commands ran after a failed verification:\n%s", calls)
			}
		})
	}
}

func TestInstallNeedsRootAndRefusesAReinstall(t *testing.T) {
	r := newInstRig(t)
	r.deps.Root = ""
	if _, err := servernode.Install(context.Background(), r.deps); !errors.Is(err, servernode.ErrNotRoot) {
		t.Fatalf("not root: %v", err)
	}
	if r.runner.all() != "" {
		t.Error("commands ran without root")
	}

	r = newInstRig(t)
	if _, err := servernode.Install(context.Background(), r.deps); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(r.root, "var/lib/echofog/server-identity.json"))
	if _, err := servernode.Install(context.Background(), r.deps); !errors.Is(err, servernode.ErrAlreadyInstalled) {
		t.Fatalf("second install: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(r.root, "var/lib/echofog/server-identity.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("a second install replaced the identity")
	}
}

func TestInstallRefusesUnsafeUserNames(t *testing.T) {
	for _, u := range []string{"", "root", "Echo Fog", "a;b", "-x", "../x", strings.Repeat("a", 40), "echofog\n"} {
		r := newInstRig(t)
		r.deps.User = u
		if _, err := servernode.Install(context.Background(), r.deps); !errors.Is(err, servernode.ErrBadUser) {
			t.Errorf("user %q: %v", u, err)
		}
		if len(entries(t, r.root)) != 0 || r.runner.all() != "" {
			t.Errorf("user %q: side effects", u)
		}
	}
}

func TestUnitFileIsHardened(t *testing.T) {
	u, err := servernode.UnitFile("echofog", "/var/lib/echofog", "/usr/local/bin/echofog-node")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"User=echofog", "Group=echofog", "NoNewPrivileges=true", "ProtectSystem=strict", "ProtectHome=true", "PrivateTmp=true", "PrivateDevices=true",
		"ReadWritePaths=/var/lib/echofog", "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", "AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX", "RestrictNamespaces=true", "LockPersonality=true", "UMask=0077",
		"Restart=on-failure", "WantedBy=multi-user.target", "ExecStart=/usr/local/bin/echofog-node v2 run --data /var/lib/echofog",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	for _, bad := range []string{"User=root", "Environment=", "--token", "--mnemonic", "PermissionsStartOnly"} {
		if strings.Contains(u, bad) {
			t.Errorf("unit contains %q", bad)
		}
	}
	for _, args := range [][3]string{{"a b", "/d", "/b"}, {"echofog", "/d\n[Service]", "/b"}, {"echofog", "/d", "relative"}, {"echofog", "d", "/b"}} {
		if _, err := servernode.UnitFile(args[0], args[1], args[2]); err == nil {
			t.Errorf("UnitFile%v accepted", args)
		}
	}
}

func TestFirewallAdvice(t *testing.T) {
	// The own-cover preset: reality2-quic (UDP) and reality2-tcp (TCP) on 443, noise3 on 444.
	cfg, _, _ := servernode.BuildPreset(servernode.PresetRelay, servernode.PresetOptions{
		Advertise: "203.0.113.10", Port: 443, Domain: "example.org", SiteDir: "/srv/site",
	})
	for _, lang := range []string{"en", "ru"} {
		adv := strings.Join(servernode.FirewallAdvice(lang, cfg), "\n")
		for _, want := range []string{"ufw allow 443/udp", "ufw allow 443/tcp", "ufw allow 444/udp", "firewall-cmd", "443/udp", "443/tcp", "doctor"} {
			if !strings.Contains(adv, want) {
				t.Errorf("%s advice lacks %q:\n%s", lang, want, adv)
			}
		}
		if strings.Contains(adv, "444/tcp") {
			t.Errorf("%s advice opens TCP 444, which no listener uses:\n%s", lang, adv)
		}
		if hasCyrillic(adv) != (lang == "ru") {
			t.Errorf("%s advice language", lang)
		}
	}
	// ws2 is plain HTTP behind a CDN: its bind is a TCP port too.
	cfg.Listen = append(cfg.Listen, servernode.ListenSpec{Suite: "ws2", Bind: "0.0.0.0:8443", Advertise: "203.0.113.10"})
	adv := strings.Join(servernode.FirewallAdvice("en", cfg), "\n")
	if !strings.Contains(adv, "ufw allow 8443/tcp") || strings.Contains(adv, "8443/udp") {
		t.Errorf("ws2 advice is not TCP-only:\n%s", adv)
	}
	// The operator API port is never advised: it is loopback only.
	cfg.Operator.Addr = "127.0.0.1:9999"
	if strings.Contains(strings.Join(servernode.FirewallAdvice("en", cfg), "\n"), "9999") {
		t.Error("the loopback operator port is in the firewall advice")
	}
}

func TestCLIInstall(t *testing.T) {
	r := newInstRig(t)
	servernode.ReleaseKeys = []keys.AKey{r.signers[0].Public(), r.signers[1].Public(), r.signers[2].Public()}
	servernode.InstallRunner = r.runner
	servernode.InstallEuid = func() int { return 0 }
	servernode.UpdateNow = func() time.Time { return updNow }
	servernode.UpdateSelfTest = func(context.Context, string) (string, error) { return "0.4.0", nil }
	t.Cleanup(func() {
		servernode.ReleaseKeys, servernode.InstallRunner, servernode.InstallEuid, servernode.UpdateNow, servernode.UpdateSelfTest = nil, nil, nil, nil, nil
	})
	mfile := filepath.Join(t.TempDir(), "manifest.bin")
	if err := os.WriteFile(mfile, r.deps.ManifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"install", "--root", r.root, "--binary", r.bin, "--manifest", mfile, "--preset", "relay", "--advertise", "203.0.113.10", "--yes", "--lang", "ru", "--platform", "linux-amd64"}
	var out, errb bytes.Buffer
	if code := servernode.MainIO(args, nil, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if !regexp.MustCompile(`(?m)^([a-z]+( [a-z]+){11})$`).MatchString(out.String()) || !hasCyrillic(out.String()) {
		t.Errorf("output:\n%s", out.String())
	}
	if strings.Contains(errb.String(), "mnemonic") {
		t.Error("the phrase went to stderr")
	}
	// A tampered binary exits nonzero and prints no phrase.
	r2 := newInstRig(t)
	_ = os.WriteFile(r2.bin, []byte("evil"), 0o755)
	out.Reset()
	args2 := append([]string(nil), args...)
	args2[2], args2[4] = r2.root, r2.bin
	if code := servernode.MainIO(args2, nil, strings.NewReader(""), &out, &errb); code == 0 || regexp.MustCompile(`(?m)^([a-z]+( [a-z]+){11})$`).MatchString(out.String()) {
		t.Errorf("a tampered install: code %d\n%s", code, out.String())
	}
	if len(entries(t, r2.root)) != 0 {
		t.Error("files written for a tampered binary")
	}
	// An unofficial build (no release keys) installs nothing.
	servernode.ReleaseKeys = nil
	r3 := newInstRig(t)
	args3 := append([]string(nil), args...)
	args3[2], args3[4] = r3.root, r3.bin
	if code := servernode.MainIO(args3, nil, strings.NewReader(""), &out, &errb); code != 1 || len(entries(t, r3.root)) != 0 {
		t.Errorf("no release keys: code %d", code)
	}
}

// A (TOCTOU): what is installed is what was verified, however the files change in between.
func TestInstallInstallsTheVerifiedBytes(t *testing.T) {
	// The self-test overwrites the file it was given: the installer must notice and refuse.
	r := newInstRig(t)
	r.deps.SelfTest = func(_ context.Context, exe string) (string, error) {
		_ = os.WriteFile(exe, []byte("EVIL, not signed"), 0o755)
		return "0.4.0", nil
	}
	_, err := servernode.Install(context.Background(), r.deps)
	if err == nil || !errors.Is(err, servernode.ErrNotVerified) {
		t.Fatalf("a file swapped during the self-test was installed: %v", err)
	}
	if e := entries(t, r.root); len(e) != 0 {
		t.Errorf("files exist after the refusal: %v", e)
	}

	// The operator's own file is swapped after the check: the installed bytes still are the verified ones.
	r = newInstRig(t)
	orig := r.deps.Binary
	r.deps.SelfTest = func(context.Context, string) (string, error) {
		_ = os.WriteFile(orig, []byte("EVIL, swapped in the original path"), 0o755)
		return "0.4.0", nil
	}
	if _, err := servernode.Install(context.Background(), r.deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(r.root, "usr/local/bin/echofog-node"))
	if !bytes.Equal(got, r.artifact) {
		t.Fatalf("installed bytes %q are not the signed artifact", got)
	}
}

// C: one changed byte with the same size is caught by the hash, not by the size check.
func TestInstallCatchesAnEqualSizeSubstitution(t *testing.T) {
	r := newInstRig(t)
	evil := append([]byte(nil), r.artifact...)
	evil[len(evil)/2] ^= 0xff
	if len(evil) != len(r.artifact) {
		t.Fatal("test setup")
	}
	if err := os.WriteFile(r.deps.Binary, evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := servernode.Install(context.Background(), r.deps); !errors.Is(err, servernode.ErrNotVerified) {
		t.Fatalf("Install = %v", err)
	}
	if len(entries(t, r.root)) != 0 || r.runner.all() != "" {
		t.Error("side effects")
	}
}

type failRunner struct {
	recRunner
	failOn string
}

func (f *failRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if f.failOn != "" && strings.HasPrefix(line, f.failOn) {
		f.mu.Lock()
		f.calls = append(f.calls, line+" (failed)")
		f.mu.Unlock()
		return []byte("boom"), errors.New("command failed")
	}
	return f.recRunner.Run(ctx, name, args...)
}

// B: a failure at any step leaves nothing of ours behind, says which step failed, and a second
// attempt is not blocked.
func TestInstallRollsBackOnEveryFailure(t *testing.T) {
	for _, step := range []string{"useradd", "chown", "systemctl daemon-reload", "systemctl enable"} {
		t.Run(step, func(t *testing.T) {
			r := newInstRig(t)
			fr := &failRunner{failOn: step}
			r.deps.Runner = fr
			r.deps.Start = true
			_, err := servernode.Install(context.Background(), r.deps)
			if err == nil {
				t.Fatal("Install succeeded although a step failed")
			}
			if !strings.Contains(err.Error(), strings.Fields(step)[0]) {
				t.Errorf("the error does not name the failed step: %v", err)
			}
			if e := entries(t, r.root); len(e) != 0 {
				t.Errorf("leftovers after a failed install: %v", e)
			}
			r.deps.Runner = &recRunner{}
			if _, err := servernode.Install(context.Background(), r.deps); err != nil {
				t.Fatalf("a second install is blocked: %v", err)
			}
		})
	}
}

// H1: the manifest comes over https only, also through redirects.
func TestCLIInstallManifestURLIsHTTPSOnly(t *testing.T) {
	r := newInstRig(t)
	servernode.ReleaseKeys = []keys.AKey{r.signers[0].Public(), r.signers[1].Public(), r.signers[2].Public()}
	servernode.InstallRunner = r.runner
	servernode.InstallEuid = func() int { return 0 }
	servernode.UpdateNow = func() time.Time { return updNow }
	servernode.UpdateSelfTest = func(context.Context, string) (string, error) { return "0.4.0", nil }
	servernode.UpdateHTTPClient = r.srv.Client()
	t.Cleanup(func() {
		servernode.ReleaseKeys, servernode.InstallRunner, servernode.InstallEuid, servernode.UpdateNow, servernode.UpdateSelfTest, servernode.UpdateHTTPClient = nil, nil, nil, nil, nil, nil
	})
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(r.deps.ManifestRaw) }))
	defer plain.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, plain.URL+"/manifest", http.StatusFound)
	}))
	defer redirect.Close()
	servernode.UpdateHTTPClient = redirect.Client()

	base := []string{"install", "--root", r.root, "--binary", r.bin, "--preset", "relay", "--advertise", "203.0.113.10", "--yes", "--platform", "linux-amd64"}
	for name, u := range map[string]string{"plain http": plain.URL + "/manifest", "redirect to http": redirect.URL + "/manifest"} {
		var out, errb bytes.Buffer
		code := servernode.MainIO(append(append([]string(nil), base...), "--manifest-url", u), nil, strings.NewReader(""), &out, &errb)
		if code == 0 {
			t.Errorf("%s: install succeeded", name)
		}
		if e := entries(t, r.root); len(e) != 0 || r.runner.all() != "" {
			t.Errorf("%s: side effects %v", name, e)
		}
	}
}
