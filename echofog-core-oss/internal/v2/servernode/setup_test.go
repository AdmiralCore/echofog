package servernode_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// setupSeams loads the release keys and the command seams the setup command uses.
func setupSeams(t *testing.T, r *instRig) {
	t.Helper()
	prevTTY := servernode.SetupIsTerminal
	servernode.ReleaseKeys = []keys.AKey{r.signers[0].Public(), r.signers[1].Public(), r.signers[2].Public()}
	servernode.InstallRunner = r.runner
	servernode.InstallEuid = func() int { return 0 }
	servernode.UpdateNow = func() time.Time { return updNow }
	servernode.UpdateSelfTest = func(context.Context, string) (string, error) { return "0.4.0", nil }
	t.Cleanup(func() {
		servernode.ReleaseKeys, servernode.InstallRunner, servernode.InstallEuid = nil, nil, nil
		servernode.UpdateNow, servernode.UpdateSelfTest = nil, nil
		servernode.SetupIsTerminal = prevTTY
		servernode.SetupFirewallProbe, servernode.SetupHealth = nil, nil
	})
}

func manifestFile(t *testing.T, r *instRig) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.bin")
	if err := os.WriteFile(p, r.deps.ManifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setupArgs(r *instRig, mfile string, extra ...string) []string {
	args := []string{"setup", "--root", r.root, "--binary", r.bin, "--manifest", mfile, "--platform", "linux-amd64"}
	return append(args, extra...)
}

// The plain mode is the mode without a terminal: the line wizard, no escape sequences, and the
// phrase and the token stay in files instead of the output.
func TestSetupNoTTYUsesPlainMode(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	servernode.SetupIsTerminal = func() bool { return false }
	mfile := manifestFile(t, r)
	in := "en\n1\n\n\n443\n203.0.113.10\n/var/lib/echofog\nn\ny\n"
	var out, errb bytes.Buffer
	code := servernode.MainIO(setupArgs(r, mfile), nil, strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("setup: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("the plain mode printed escape sequences")
	}
	if regexp.MustCompile(`(?m)^([a-z]+( [a-z]+){11})$`).MatchString(out.String()) {
		t.Error("the plain mode printed the recovery phrase")
	}
	if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(out.String() + errb.String()) {
		t.Error("the plain mode printed the operator token")
	}
	data := filepath.Join(r.root, "var/lib/echofog")
	if _, err := os.Stat(filepath.Join(data, "recovery-phrase.txt")); err != nil {
		t.Errorf("no recovery-phrase.txt: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(data, "recovery-phrase.txt")); err == nil && os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the phrase file is open to others: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(data, "server.json")); err != nil {
		t.Errorf("no server.json: %v", err)
	}
}

// --plain has the same meaning with a terminal present.
func TestSetupPlainFlagForcesPlainMode(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	servernode.SetupIsTerminal = func() bool { return true }
	mfile := manifestFile(t, r)
	var out, errb bytes.Buffer
	code := servernode.MainIO(setupArgs(r, mfile, "--preset", "relay", "--advertise", "203.0.113.10", "--yes", "--plain"),
		nil, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("setup --plain: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("--plain printed escape sequences")
	}
}

// The screen and the flags give the same files: the wizard path and --yes are compared on disk.
func TestSetupPlainWizardAndFlagsAgree(t *testing.T) {
	for _, preset := range []string{"relay", "exit-free", "home-entry"} {
		t.Run(preset, func(t *testing.T) {
			// The wizard path: preset number, no domain, port, address, data folder.
			a := newInstRig(t)
			setupSeams(t, a)
			servernode.SetupIsTerminal = func() bool { return false }
			numbers := map[string]string{"relay": "1", "exit-free": "2", "home-entry": "3"}
			in := "en\n" + numbers[preset] + "\n\n443\n203.0.113.10\n/var/lib/echofog\nn\ny\n"
			var out, errb bytes.Buffer
			if code := servernode.MainIO(setupArgs(a, manifestFile(t, a)), nil, strings.NewReader(in), &out, &errb); code != 0 {
				t.Fatalf("wizard: %d\n%s\n%s", code, out.String(), errb.String())
			}
			// The flags path.
			b := newInstRig(t)
			setupSeams(t, b)
			var out2, errb2 bytes.Buffer
			if code := servernode.MainIO(setupArgs(b, manifestFile(t, b), "--preset", preset, "--advertise", "203.0.113.10", "--yes"),
				nil, strings.NewReader(""), &out2, &errb2); code != 0 {
				t.Fatalf("flags: %d\n%s\n%s", code, out2.String(), errb2.String())
			}
			cmpFiles(t, a.root, b.root)
		})
	}
}

func cmpFiles(t *testing.T, rootA, rootB string) {
	t.Helper()
	dirA := filepath.Join(rootA, "var/lib/echofog")
	dirB := filepath.Join(rootB, "var/lib/echofog")
	for _, name := range []string{"server.json"} {
		a, err := os.ReadFile(filepath.Join(dirA, name))
		if err != nil {
			t.Fatalf("%s in A: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatalf("%s in B: %v", name, err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs:\n--- screen\n%s\n--- flags\n%s", name, a, b)
		}
	}
	ua, err := os.ReadFile(filepath.Join(rootA, "etc/systemd/system/echofog-node.service"))
	if err != nil {
		t.Fatalf("unit A: %v", err)
	}
	ub, err := os.ReadFile(filepath.Join(rootB, "etc/systemd/system/echofog-node.service"))
	if err != nil {
		t.Fatalf("unit B: %v", err)
	}
	if string(ua) != string(ub) {
		t.Errorf("units differ:\n--- screen\n%s\n--- flags\n%s", ua, ub)
	}
}

// A failure at any step takes back everything and names the step.
func TestSetupRollsBackOnEveryStep(t *testing.T) {
	steps := []struct {
		fail  string
		extra []string
		seam  func(r *instRig)
	}{
		{fail: "useradd"},
		{fail: "chown"},
		{fail: "systemctl daemon-reload"},
		{fail: "systemctl enable"},
		{fail: "ufw allow", extra: []string{"--open-ports"}, seam: func(r *instRig) {
			servernode.SetupFirewallProbe = func(string) (string, error) { return "/usr/sbin/ufw", nil }
		}},
		{fail: "", seam: func(r *instRig) {}},
	}
	for i, step := range steps {
		name := step.fail
		if name == "" {
			name = "health"
		}
		t.Run(name, func(t *testing.T) {
			r := newInstRig(t)
			setupSeams(t, r)
			fr := &failRunner{failOn: step.fail}
			servernode.InstallRunner = fr
			if step.fail == "systemctl enable" || step.fail == "" {
				servernode.SetupHealth = func(context.Context, string) error { return errors.New("node is down") }
				t.Cleanup(func() { servernode.SetupHealth = nil })
			}
			if step.seam != nil {
				step.seam(r)
			}
			args := append(setupArgs(r, manifestFile(t, r), "--preset", "relay", "--advertise", "203.0.113.10", "--yes"), step.extra...)
			var out, errb bytes.Buffer
			code := servernode.MainIO(args, nil, strings.NewReader(""), &out, &errb)
			if code == 0 {
				t.Fatalf("step %d succeeded although %s failed", i, name)
			}
			if e := entries(t, r.root); len(e) != 0 {
				t.Errorf("leftovers after the rollback: %v", e)
			}
		})
	}
}

func TestSystemChecks(t *testing.T) {
	checks := servernode.RunSystemChecks(servernode.SystemCheckDeps{
		Root: "/srv/root",
		Euid: func() int { return 0 },
		LookPath: func(name string) (string, error) {
			if name == "ufw" || name == "timedatectl" {
				return "/usr/bin/" + name, nil
			}
			return "", os.ErrNotExist
		},
		Runner:    &recRunner{},
		FreeBytes: func(string) (uint64, error) { return 2 << 30, nil },
		ListenUDP: func(string) error { return nil },
		ListenTCP: func(string) error { return errors.New("busy") },
	})
	byID := map[string]servernode.Level{}
	for _, c := range checks {
		byID[c.ID] = c.Level
	}
	if byID["root"] != servernode.LevelOK || byID["systemd"] != servernode.LevelOK {
		t.Errorf("root/systemd: %v", byID)
	}
	if byID["ports"] != servernode.LevelFail {
		t.Errorf("a busy TCP port must stop the setup: %v", byID)
	}
	if byID["firewall"] != servernode.LevelOK || byID["clock"] != servernode.LevelInfo {
		t.Errorf("firewall/clock: %v", byID)
	}
	if byID["disk"] != servernode.LevelOK || byID["docker"] != servernode.LevelInfo {
		t.Errorf("disk/docker: %v", byID)
	}
	for _, c := range checks {
		if c.Text("en") == "" || c.Text("ru") == "" {
			t.Errorf("%s has no text", c.ID)
		}
	}
}

func TestUninstallKeepsAndRemovesData(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	if _, err := servernode.Install(context.Background(), r.deps); err != nil {
		t.Fatal(err)
	}
	servernode.InstallRunner = r.runner
	var out, errb bytes.Buffer
	if code := servernode.MainIO([]string{"uninstall", "--root", r.root, "--data", "/var/lib/echofog", "--yes"}, nil, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("uninstall: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(r.root, "etc/systemd/system/echofog-node.service")); !os.IsNotExist(err) {
		t.Error("the unit is still there")
	}
	if _, err := os.Stat(filepath.Join(r.root, "usr/local/bin/echofog-node")); !os.IsNotExist(err) {
		t.Error("the binary is still there")
	}
	if _, err := os.Stat(filepath.Join(r.root, "var/lib/echofog/server-identity.json")); err != nil {
		t.Error("the data was removed without --purge")
	}
	if !strings.Contains(r.runner.all(), "systemctl disable --now echofog-node") {
		t.Errorf("the service was not disabled:\n%s", r.runner.all())
	}

	// --purge needs the word delete.
	var out2, errb2 bytes.Buffer
	if code := servernode.MainIO([]string{"uninstall", "--root", r.root, "--data", "/var/lib/echofog", "--purge"}, nil, strings.NewReader("no\n"), &out2, &errb2); code != 0 {
		t.Fatalf("declined purge: %d %s", code, errb2.String())
	}
	if _, err := os.Stat(filepath.Join(r.root, "var/lib/echofog/server-identity.json")); err != nil {
		t.Error("the data was removed without the word delete")
	}
	if code := servernode.MainIO([]string{"uninstall", "--root", r.root, "--data", "/var/lib/echofog", "--purge", "--yes"}, nil, strings.NewReader(""), &out2, &errb2); code != 0 {
		t.Fatalf("purge: %d %s", code, errb2.String())
	}
	if _, err := os.Stat(filepath.Join(r.root, "var/lib/echofog")); !os.IsNotExist(err) {
		t.Error("--purge kept the data")
	}
}

func TestStatusPanelFitsWidth(t *testing.T) {
	st := servernode.StatusInfo{
		Version: "0.3.0", Roles: []string{"relay", "mailbox"}, Sessions: 3,
		Listen:       []string{"reality2-quic 203.0.113.10 (0.0.0.0:443)", "noise3 203.0.113.10 (0.0.0.0:444)"},
		TrafficDayIn: 12 << 20, TrafficDayOut: 45 << 20, TrafficIn: 1 << 30, TrafficOut: 2 << 30,
		CoverCertNotAfter: updNow.Unix() + 60*86400, PublicCard: true, PublicLink: "echofog://public/abc",
		Limits: servernode.LimitsSpec{Sessions: 20}, Visibility: "visible",
	}
	upd := &servernode.UpdateInfo{Version: "0.4.0", MinVersion: "0.3.0"}
	for _, lang := range []string{"ru", "en"} {
		for _, width := range []int{60, 80, 120} {
			panel := servernode.RenderStatusPanel(st, upd, nil, lang, width, false)
			for _, line := range strings.Split(panel, "\n") {
				if len([]rune(line)) > width {
					t.Errorf("%s/%d: line of %d runes: %q", lang, width, len([]rune(line)), line)
				}
			}
			for _, must := range []string{"0.3.0", "0.4.0", "relay", "3"} {
				if !strings.Contains(panel, must) {
					t.Errorf("%s/%d: no %q in the panel:\n%s", lang, width, must, panel)
				}
			}
		}
	}
	// Without an update check the panel says how to ask for one, and never asks by itself.
	panel := servernode.RenderStatusPanel(st, nil, nil, "en", 80, false)
	if !strings.Contains(panel, "update --check") {
		t.Errorf("no update hint:\n%s", panel)
	}
}
