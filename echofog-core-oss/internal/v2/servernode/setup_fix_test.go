package servernode_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// withSetupTUI replaces the full-screen setup for the duration of one test.
func withSetupTUI(t *testing.T, fn func(context.Context, servernode.UIOptions, servernode.UIDeps) (*servernode.SetupTUIResult, error)) {
	t.Helper()
	prev := servernode.SetupTUI
	servernode.SetupTUI = fn
	t.Cleanup(func() { servernode.SetupTUI = prev })
}

// The interactive pass owns the apply: the command must not apply the confirmed plan again. On
// the returned branch a second Install sees server-identity.json and fails with ErrAlreadyInstalled.
func TestSetupInteractiveAppliesOnce(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	servernode.SetupIsTerminal = func() bool { return true }
	applies := 0
	withSetupTUI(t, func(ctx context.Context, opts servernode.UIOptions, deps servernode.UIDeps) (*servernode.SetupTUIResult, error) {
		p, err := servernode.PlanFromAnswers(opts.Seed)
		if err != nil {
			return nil, err
		}
		applies++
		info, err := deps.Apply(ctx, &p, func(string) {})
		if err != nil {
			return nil, err
		}
		return &servernode.SetupTUIResult{Plan: &p, Info: info, Applied: true}, nil
	})
	var out, errb bytes.Buffer
	code := servernode.MainIO(setupArgs(r, manifestFile(t, r), "--preset", "relay", "--advertise", "203.0.113.10"),
		nil, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("interactive setup: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if applies != 1 {
		t.Fatalf("the plan was applied %d times, want exactly once", applies)
	}
	if _, err := os.Stat(filepath.Join(r.root, "var/lib/echofog/server-identity.json")); err != nil {
		t.Errorf("the node profile is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.root, "etc/systemd/system/echofog-node.service")); err != nil {
		t.Errorf("the unit is missing: %v", err)
	}
	// The screen removed the phrase file after the operator confirmed it; the text after the
	// full-screen pass must not point at a file that is already gone.
	if strings.Contains(out.String(), "Recovery phrase:") {
		t.Errorf("the interactive pass prints a removed phrase file:\n%s", out.String())
	}
}

// The screen offers the address of the machine, as the plain mode already does: an empty seed
// would ask the operator for an address the installer could have proposed itself.
func TestSetupTUISuggestsDetectedAddress(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	servernode.SetupIsTerminal = func() bool { return true }
	prevAdvertise := servernode.SetupAdvertise
	servernode.SetupAdvertise = func() string { return "198.51.100.7" }
	t.Cleanup(func() { servernode.SetupAdvertise = prevAdvertise })
	var got string
	withSetupTUI(t, func(_ context.Context, opts servernode.UIOptions, _ servernode.UIDeps) (*servernode.SetupTUIResult, error) {
		got = opts.Seed.Advertise
		return nil, servernode.ErrSetupCancelled
	})
	var out, errb bytes.Buffer
	if code := servernode.MainIO(setupArgs(r, manifestFile(t, r)), nil, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("cancelled setup: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if got != "198.51.100.7" {
		t.Fatalf("the screen was seeded with %q, want the detected address", got)
	}
}

// The plain mode is the same installer: the flags win over the wizard's answers, and the answer
// to "start the node now?" is used instead of being ignored.
func TestSetupPlainModeHonorsFlagsAndStartAnswer(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	servernode.SetupIsTerminal = func() bool { return false }
	// language, preset, domain, port, address, data, start = n, confirm = y
	in := "en\n1\n\n\n\n\nn\ny\n"
	var out, errb bytes.Buffer
	code := servernode.MainIO(setupArgs(r, manifestFile(t, r),
		"--preset", "exit-free", "--domain", "example.org", "--site-dir", "/srv/site",
		"--carriers", "noise3", "--invite", "--port", "8443", "--advertise", "203.0.113.20"),
		nil, strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("plain setup: %d\n%s\n%s", code, out.String(), errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(r.root, "var/lib/echofog/server.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := servernode.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cover != "example.org:8443" || cfg.SiteDir != "/srv/site" {
		t.Errorf("the cover flags were ignored: cover %q site %q", cfg.Cover, cfg.SiteDir)
	}
	if len(cfg.Listen) != 1 || cfg.Listen[0].Suite != "noise3" {
		t.Errorf("the carrier flag was ignored: %+v", cfg.Listen)
	}
	if cfg.InviteListen == nil {
		t.Error("the --invite flag was ignored")
	}
	if strings.Contains(r.runner.all(), "systemctl enable --now") {
		t.Errorf("the node was started although the wizard answered no:\n%s", r.runner.all())
	}
}

// A failed write of the recovery-phrase file happens after Install succeeded. It is reported as
// its own warning: the node stays installed, and the operator is never told it was rolled back.
func TestSetupPhraseWriteFailureIsNotRolledBack(t *testing.T) {
	r := newInstRig(t)
	setupSeams(t, r)
	// A directory in place of the phrase file makes the private write fail after Install.
	data := filepath.Join(r.root, "var/lib/echofog")
	if err := os.MkdirAll(filepath.Join(data, "recovery-phrase.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := servernode.MainIO(setupArgs(r, manifestFile(t, r), "--preset", "relay", "--advertise", "203.0.113.10", "--yes"),
		nil, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("setup after a phrase-file failure: %d\n%s\n%s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(data, "server-identity.json")); err != nil {
		t.Errorf("the successful installation was rolled back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.root, "etc/systemd/system/echofog-node.service")); err != nil {
		t.Errorf("the unit is missing: %v", err)
	}
	combined := out.String() + errb.String()
	if !strings.Contains(combined, "recovery phrase") {
		t.Errorf("the failed phrase write was not reported:\n%s", combined)
	}
	if strings.Contains(strings.ToLower(combined), "rolled back") {
		t.Errorf("the output claims a rollback that did not happen:\n%s", combined)
	}
}
