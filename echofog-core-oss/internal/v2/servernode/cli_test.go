package servernode_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

func runCLI(env map[string]string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := servernode.Main(args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIUsage(t *testing.T) {
	if code, _, errs := runCLI(nil); code != 2 || !strings.Contains(errs, "init") {
		t.Fatalf("no arguments: %d %q", code, errs)
	}
	if code, _, _ := runCLI(nil, "bogus"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code, out, _ := runCLI(nil, "help"); code != 0 || !strings.Contains(out, "rotate-keys") {
		t.Fatalf("help: %d %q", code, out)
	}
}

func TestCLIInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node")
	args := []string{"init", "--data", dir, "--listen", fmt.Sprintf("noise3=127.0.0.1:%d", freeUDPPort(t)),
		"--advertise", "127.0.0.1", "--roles", "relay,mailbox"}
	code, out, errs := runCLI(nil, args...)
	if code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	words := regexp.MustCompile(`(?m)^([a-z]+( [a-z]+){11})$`).FindString(out)
	if words == "" {
		t.Fatalf("no 12-word phrase in the output:\n%s", out)
	}
	if strings.Contains(errs, words) {
		t.Fatal("the phrase went to stderr as well")
	}
	if code, _, _ := runCLI(nil, args...); code != 1 {
		t.Fatalf("second init: %d", code)
	}
	for name, a := range map[string][]string{
		"no data":      {"init", "--listen", "noise3=0.0.0.0:443", "--advertise", "203.0.113.10"},
		"no listener":  {"init", "--data", filepath.Join(t.TempDir(), "a")},
		"bad role":     {"init", "--data", filepath.Join(t.TempDir(), "b"), "--listen", "noise3=0.0.0.0:443", "--advertise", "203.0.113.10", "--roles", "admin"},
		"bad listener": {"init", "--data", filepath.Join(t.TempDir(), "c"), "--listen", "noise3", "--advertise", "203.0.113.10"},
	} {
		if code, _, _ := runCLI(nil, a...); code != 2 && code != 1 {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}

func TestCLIRunWithoutProfile(t *testing.T) {
	if code, _, errs := runCLI(nil, "run", "--data", filepath.Join(t.TempDir(), "none")); code != 1 || errs == "" {
		t.Fatalf("run without a profile: %d %q", code, errs)
	}
}

func TestCLIOperatorCommands(t *testing.T) {
	dir, _ := profile(t, true)
	startServer(t, dir)

	code, out, errs := runCLI(nil, "status", "--data", dir)
	if code != 0 || !strings.Contains(out, "no-cover") {
		t.Fatalf("status: %d %q %q", code, out, errs)
	}
	code, out, errs = runCLI(nil, "invite", "create", "--data", dir, "--access", "30d")
	if code != 0 || !strings.Contains(out, "echofog://") {
		t.Fatalf("invite create: %d %q %q", code, out, errs)
	}
	id := regexp.MustCompile(`[0-9a-f]{16}`).FindString(out)
	if id == "" {
		t.Fatalf("no invite id in %q", out)
	}
	if code, out, _ := runCLI(nil, "invite", "list", "--data", dir); code != 0 || !strings.Contains(out, id) {
		t.Fatalf("invite list: %d %q", code, out)
	}
	if code, out, errs := runCLI(nil, "invite", "extend", id, "--data", dir, "--access", "7d"); code != 0 {
		t.Fatalf("invite extend: %d %q %q", code, out, errs)
	}
	if code, out, errs := runCLI(nil, "rotate-keys", "--data", dir); code != 0 {
		t.Fatalf("rotate-keys: %d %q %q", code, out, errs)
	}
	// The token is never printed by any command.
	for _, o := range []string{out, errs} {
		if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(o) {
			t.Errorf("a 64-hex secret in the output: %q", o)
		}
	}
	// --out writes the same link and nothing else: no id, no term.
	linkFile := filepath.Join(t.TempDir(), "invite.txt")
	code, out, errs = runCLI(nil, "invite", "create", "--data", dir, "--access", "30d", "--out", linkFile)
	if code != 0 {
		t.Fatalf("invite create --out: %d %q %q", code, out, errs)
	}
	link := regexp.MustCompile(`echofog://\S+`).FindString(out)
	if link == "" {
		t.Fatalf("no link in the output: %q", out)
	}
	raw, err := os.ReadFile(linkFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != link {
		t.Fatalf("file holds %q, want the link only %q", raw, link)
	}
	if fi, err := os.Stat(linkFile); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("link file mode %o, want 600", fi.Mode().Perm())
	}
	if code, _, errs := runCLI(nil, "invite", "create", "--data", dir, "--out", filepath.Join(t.TempDir(), "missing", "invite.txt")); code != 1 || !strings.Contains(errs, "missing") {
		t.Fatalf("unwritable --out: %d %q", code, errs)
	}
	// Bad terms are refused before any request.
	if code, _, _ := runCLI(nil, "invite", "create", "--data", dir, "--access", "soon"); code != 2 {
		t.Fatalf("bad term: %d", code)
	}
	if code, _, _ := runCLI(nil, "invite", "extend", "--data", dir, "--access", "7d"); code != 2 {
		t.Fatalf("extend without an id: %d", code)
	}
}

func TestCLIStatusWhenNotRunning(t *testing.T) {
	dir, _ := profile(t, false)
	code, _, errs := runCLI(nil, "status", "--data", dir)
	if code != 1 || !strings.Contains(errs, "running") {
		t.Fatalf("status of a stopped node: %d %q", code, errs)
	}
}

func TestCLIEmptyRolesAreKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node")
	code, _, errs := runCLI(nil, "init", "--data", dir, "--listen", "noise3=0.0.0.0:443", "--advertise", "203.0.113.10", "--roles", "")
	if code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := servernode.ParseConfig(raw)
	if err != nil || len(c.Roles) != 0 {
		t.Fatalf("saved config: %v, %v; --roles \"\" must switch every role off", c, err)
	}
}
