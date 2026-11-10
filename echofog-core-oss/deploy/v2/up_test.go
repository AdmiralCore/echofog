package v2deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const upScript = "up.sh"

// upEnvBlock returns the part of the dry-run plan between the .env markers: the file the script
// would write, printed so the operator sees every value before anything is created.
func upEnvBlock(t *testing.T, out string) string {
	t.Helper()
	const begin, end = "# --- deploy/v2/.env begin ---", "# --- deploy/v2/.env end ---"
	_, rest, ok := strings.Cut(out, begin)
	if !ok {
		t.Fatalf("the plan has no %q:\n%s", begin, out)
	}
	body, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("the plan has no %q:\n%s", end, out)
	}
	return body
}

func upEnvValue(t *testing.T, block, key string) string {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	t.Fatalf("the planned .env has no %s:\n%s", key, block)
	return ""
}

func runUp(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runUpEnv(t, nil, args...)
}

// runUpEnv runs the script with extra environment values. A missing sh is not a silent pass: the
// script is what these tests check, so say plainly what was not checked and why.
func runUpEnv(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not in PATH: the up.sh tests run the real script under a POSIX shell (Git bash on Windows); install one and add it to PATH, or the script is not checked at all")
	}
	cmd := exec.Command(sh, append([]string{upScript}, args...)...)
	// A docker that always fails: a dry run must not call it at all.
	cmd.Env = append(append(os.Environ(), "ECHOFOG_DOCKER=false"), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// fakeDocker reports a healthy node and accepts every compose call, so the real write path can be
// exercised without a Docker daemon.
const fakeDocker = `#!/bin/sh
if [ "$1" = "inspect" ]; then
	printf 'healthy\n'
fi
exit 0
`

// upRoot copies the script into a throwaway tree together with a fake docker, so the real write
// path can be exercised without a Docker daemon and a real deploy/v2 is never touched.
func upRoot(t *testing.T) (dir, docker string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "deploy", "v2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("temp tree: %v", err)
	}
	script, err := os.ReadFile(upScript)
	if err != nil {
		t.Fatalf("read %s: %v", upScript, err)
	}
	if err := os.WriteFile(filepath.Join(dir, upScript), script, 0o755); err != nil {
		t.Fatalf("copy %s: %v", upScript, err)
	}
	docker = filepath.Join(root, "fake-docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return dir, docker
}

// runUpAt starts a copy made by upRoot with the fake docker.
func runUpAt(t *testing.T, dir, docker string, args ...string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not in PATH: the up.sh tests run the real script under a POSIX shell (Git bash on Windows); install one and add it to PATH, or the script is not checked at all")
	}
	cmd := exec.Command(sh, append([]string{filepath.Join(dir, upScript)}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ECHOFOG_DOCKER="+docker)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("up.sh: %v\n%s", err, out)
	}
	return string(out)
}

// absentBefore says whether name did not exist before the plan was made: a dry run must not create it.
func absentBefore(name string) bool {
	_, err := os.Stat(name)
	return os.IsNotExist(err)
}

// TestUpScript checks the one-command path: a POSIX sh script with the documented options, no
// download piped into a shell, and a dry run that shows the commands and the .env it would write
// without touching Docker or the disk.
func TestUpScript(t *testing.T) {
	sh := read(t, upScript)
	if !strings.HasPrefix(sh, "#!/bin/sh\n") {
		t.Error("up.sh is a POSIX sh script")
	}
	for _, must := range []string{
		"set -eu", "umask 077",
		"--advertise", "--image", "--port", "--invites", "--cover", "--site", "--test", "--name", "--dry-run",
		"ECHOFOG_ADVERTISE_ADDR", "ECHOFOG_IMAGE", "ECHOFOG_PORT", "ECHOFOG_NOISE_PORT",
		"ECHOFOG_INVITES", "ECHOFOG_COVER", "ECHOFOG_SITE_DIR",
		"deploy/v2/docker-compose.yml", "--env-file", "compose", "pull", "up -d",
		"status --data /data --plain", "invite create", "--out /data/invite.txt", "/data/recovery-phrase.txt",
		"ef-test", "ef-test-data", "8443", "down -v",
	} {
		if !strings.Contains(sh, must) {
			t.Errorf("up.sh lacks %q", must)
		}
	}
	// Nothing is downloaded, nothing is evaluated, and no download reaches a shell.
	for _, bad := range []string{"curl", "wget", "set -x", "eval ", "http://", "https://", "chmod 777"} {
		if strings.Contains(sh, bad) {
			t.Errorf("up.sh contains %q", bad)
		}
	}
	if pipeToShell.MatchString(sh) {
		t.Error("up.sh pipes into a shell")
	}
	if p, err := exec.LookPath("sh"); err == nil {
		if out, err := exec.Command(p, "-n", upScript).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	} else {
		t.Log("sh is not in PATH: sh -n on up.sh was not run")
	}
}

// The plan of a test run names the throwaway container and volume, uses ports from 8443 and shows
// the commands that remove everything afterwards.
func TestUpScriptTestRunPlan(t *testing.T) {
	out, err := runUp(t, "--advertise", "203.0.113.10", "--image", "sha-abc1234", "--invites", "--test", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, must := range []string{"[dry-run]", "ef-test", "ef-test-data", "up -d", "invite create", "down -v"} {
		if !strings.Contains(out, must) {
			t.Errorf("the plan lacks %q:\n%s", must, out)
		}
	}
	env := upEnvBlock(t, out)
	if got := upEnvValue(t, env, "ECHOFOG_ADVERTISE_ADDR"); got != "203.0.113.10" {
		t.Errorf("ECHOFOG_ADVERTISE_ADDR=%q", got)
	}
	if got := upEnvValue(t, env, "ECHOFOG_IMAGE"); !strings.Contains(got, "echofog-node:") {
		t.Errorf("ECHOFOG_IMAGE=%q", got)
	}
	port, err := strconv.Atoi(upEnvValue(t, env, "ECHOFOG_PORT"))
	if err != nil || port < 8443 || port > 65534 {
		t.Fatalf("ECHOFOG_PORT=%q: a test run starts at 8443", upEnvValue(t, env, "ECHOFOG_PORT"))
	}
	if got := upEnvValue(t, env, "ECHOFOG_NOISE_PORT"); got != strconv.Itoa(port+1) {
		t.Errorf("ECHOFOG_NOISE_PORT=%q, want %d", got, port+1)
	}
	if strings.Contains(env, "ECHOFOG_ADVERTISE_ADDR=\n") {
		t.Error("the planned .env leaves the required address empty")
	}
}

// The default plan points at the production port and the documented name, and prints the docker
// commands instead of running them (ECHOFOG_DOCKER=false would fail the script if it did).
func TestUpScriptProductionPlan(t *testing.T) {
	envAbsent, overrideAbsent := absentBefore(".env"), absentBefore("docker-compose.up.yml")
	out, err := runUp(t, "--advertise", "203.0.113.10", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, must := range []string{
		"-p v2", "echofog-v2-node", "deploy/v2/docker-compose.yml", "deploy/v2/.env",
		"--env-file", "pull", "up -d", "status --data /data --plain", "/data/recovery-phrase.txt",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("the plan lacks %q:\n%s", must, out)
		}
	}
	if got := upEnvValue(t, upEnvBlock(t, out), "ECHOFOG_ADVERTISE_ADDR"); got != "203.0.113.10" {
		t.Errorf("ECHOFOG_ADVERTISE_ADDR=%q", got)
	}
	// Dry runs write nothing: no .env and no generated override next to the compose file. A
	// developer machine may already carry a real .env, so only a file that appears is a failure.
	for name, absent := range map[string]bool{".env": envAbsent, "docker-compose.up.yml": overrideAbsent} {
		if absent {
			if _, err := os.Stat(name); err == nil {
				t.Errorf("the dry run wrote %s", name)
			}
		}
	}
}

// Bad input is refused with a message about the flag, before any docker command or file write.
func TestUpScriptRefusesBadInput(t *testing.T) {
	if out, err := runUp(t, "--dry-run"); err == nil {
		t.Errorf("no --advertise: %v\n%s", err, out)
	}
	if out, err := runUp(t, "--advertise", "node.example.org", "--dry-run"); err == nil {
		t.Errorf("a name as --advertise: %v\n%s", err, out)
	} else if !regexp.MustCompile(`(?i)literal|address`).MatchString(out) {
		t.Errorf("the refusal does not explain the address: %s", out)
	}
	if out, err := runUp(t, "--advertise", "203.0.113.10", "--cover", "node.example.org:443", "--dry-run"); err == nil {
		t.Errorf("a cover without a site: %v\n%s", err, out)
	}
	if out, err := runUp(t, "--advertise", "203.0.113.10", "--port", "forty", "--dry-run"); err == nil {
		t.Errorf("a bad port: %v\n%s", err, out)
	}
}

// An invitation command only works on a node that opened the invitation listener at its first
// start. Without --invites the plan names that instead of printing a command that fails.
func TestUpScriptInvites(t *testing.T) {
	out, err := runUp(t, "--advertise", "203.0.113.10", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if strings.Contains(out, "invite create") {
		t.Errorf("the plan prints an invitation command though the node starts without --invites:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "invitations are off") {
		t.Errorf("the plan does not say that invitations are off:\n%s", out)
	}
	if block := upEnvBlock(t, out); strings.Contains(block, "ECHOFOG_INVITES") {
		t.Errorf("the .env switches invitations on without --invites:\n%s", block)
	}

	out, err = runUp(t, "--advertise", "203.0.113.10", "--invites", "--dry-run")
	if err != nil {
		t.Fatalf("dry run with --invites: %v\n%s", err, out)
	}
	for _, must := range []string{"invite create", "--out /data/invite.txt", "cat /data/invite.txt"} {
		if !strings.Contains(out, must) {
			t.Errorf("the plan with --invites lacks %q:\n%s", must, out)
		}
	}
	if got := upEnvValue(t, upEnvBlock(t, out), "ECHOFOG_INVITES"); got != "true" {
		t.Errorf("ECHOFOG_INVITES=%q with --invites", got)
	}

	// ECHOFOG_INVITES=true in the environment is the same switch as the flag.
	out, err = runUpEnv(t, []string{"ECHOFOG_INVITES=true"}, "--advertise", "203.0.113.10", "--dry-run")
	if err != nil {
		t.Fatalf("dry run with ECHOFOG_INVITES=true: %v\n%s", err, out)
	}
	if !strings.Contains(out, "invite create") {
		t.Errorf("ECHOFOG_INVITES=true did not open the invitation listener:\n%s", out)
	}
}

// The .env carries only the values that differ from .env.example.
func TestUpScriptWritesOnlyDifferences(t *testing.T) {
	out, err := runUp(t, "--advertise", "203.0.113.10", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	block := upEnvBlock(t, out)
	for _, key := range []string{"ECHOFOG_IMAGE", "ECHOFOG_PORT", "ECHOFOG_NOISE_PORT", "ECHOFOG_INVITES", "ECHOFOG_COVER", "ECHOFOG_SITE_DIR"} {
		if strings.Contains(block, key+"=") {
			t.Errorf("the .env repeats the example value %s:\n%s", key, block)
		}
	}
	out, err = runUp(t, "--advertise", "203.0.113.10", "--image", "sha-abc1234", "--port", "8443", "--dry-run")
	if err != nil {
		t.Fatalf("dry run with a pinned image: %v\n%s", err, out)
	}
	block = upEnvBlock(t, out)
	if got := upEnvValue(t, block, "ECHOFOG_IMAGE"); got != "ghcr.io/admiralcore/echofog-node:sha-abc1234" {
		t.Errorf("ECHOFOG_IMAGE=%q", got)
	}
	if got := upEnvValue(t, block, "ECHOFOG_PORT"); got != "8443" {
		t.Errorf("ECHOFOG_PORT=%q", got)
	}
	if got := upEnvValue(t, block, "ECHOFOG_NOISE_PORT"); got != "8444" {
		t.Errorf("ECHOFOG_NOISE_PORT=%q", got)
	}
}

// IPv6 is checked strictly: one "::" at most, hex groups of 1-4 digits, no dotted tail.
func TestUpScriptIPv6(t *testing.T) {
	good := []string{"2001:db8::1", "::1", "::", "2001:db8:0:1:2:3:4:5", "fe80::1", "1::"}
	for _, addr := range good {
		if out, err := runUp(t, "--advertise", addr, "--dry-run"); err != nil {
			t.Errorf("--advertise %s refused: %v\n%s", addr, err, out)
		}
	}
	bad := []string{"1::2::3", "2001:db8::1::2", ":::", "1:2:3:4:5:6:7", "2001:db8:0:1:2:3:4:5:6", "2001:db8:0:1:2:3:4:5:6:7", "2001:db8::1:2:3:4:5:6:7:8", "12345::1"}
	for _, addr := range bad {
		if out, err := runUp(t, "--advertise", addr, "--dry-run"); err == nil {
			t.Errorf("--advertise %s accepted: %v\n%s", addr, err, out)
		}
	}
}

// tempSite makes a folder that the script accepts as an absolute site path: a POSIX path from the
// same shell, not a Windows path with backslashes.
func tempSite(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not in PATH: the up.sh tests run the real script under a POSIX shell (Git bash on Windows); install one and add it to PATH, or the script is not checked at all")
	}
	out, err := exec.Command(sh, "-c", "mktemp -d").Output()
	if err != nil {
		t.Fatalf("mktemp -d: %v", err)
	}
	site := strings.TrimSpace(string(out))
	if site == "" {
		t.Fatal("mktemp -d printed nothing")
	}
	t.Cleanup(func() { _ = exec.Command(sh, "-c", `rm -rf -- "$1"`, "cleanup", site).Run() })
	return site
}

// A port near the end of the range leaves no room for the ports the script derives, and the write
// is refused before the .env names an impossible port.
func TestUpScriptRefusesPortsPastTheEnd(t *testing.T) {
	if out, err := runUp(t, "--advertise", "203.0.113.10", "--port", "65535", "--dry-run"); err == nil {
		t.Errorf("--port 65535 leaves noise3 at 65536: %v\n%s", err, out)
	}
	if out, err := runUp(t, "--advertise", "203.0.113.10", "--port", "65534", "--dry-run"); err != nil {
		t.Errorf("--port 65534 is the last usable port: %v\n%s", err, out)
	}
	site := tempSite(t)
	if out, err := runUp(t, "--advertise", "203.0.113.10", "--port", "65534", "--cover", "node.example.org:443", "--site", site, "--invites", "--dry-run"); err == nil {
		t.Errorf("--port 65534 with a cover and --invites leaves the invitations at 65536: %v\n%s", err, out)
	}
	if out, err := runUp(t, "--advertise", "203.0.113.10", "--port", "65533", "--cover", "node.example.org:443", "--site", site, "--invites", "--dry-run"); err != nil {
		t.Errorf("--port 65533 with a cover and --invites fits: %v\n%s", err, out)
	}
}

// A run without --invites says so plainly, and every run says that the flags shape the profile only
// at the first start.
func TestUpScriptProfileNotes(t *testing.T) {
	out, err := runUp(t, "--advertise", "203.0.113.10", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, must := range []string{"invitations are off", "first start", "empty volume"} {
		if !strings.Contains(strings.ToLower(out), must) {
			t.Errorf("the plan lacks %q:\n%s", must, out)
		}
	}
}

// An existing .env is copied to .env.bak.<date> before it is replaced, so a rerun does not throw
// away the operator's edits (roles, ACME, a certificate path).
func TestUpScriptKeepsPreviousEnv(t *testing.T) {
	dir, docker := upRoot(t)
	const previous = "ECHOFOG_ADVERTISE_ADDR=203.0.113.10\nECHOFOG_ROLES=relay,exit\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(previous), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}
	out := runUpAt(t, dir, docker, "--advertise", "203.0.113.10", "--invites")
	backups, err := filepath.Glob(filepath.Join(dir, ".env.bak.*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups of .env: %v\n%s", backups, out)
	}
	kept, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(kept) != previous {
		t.Errorf("the backup holds %q, want the previous .env", kept)
	}
	if !strings.Contains(out, ".env.bak.") {
		t.Errorf("the run does not say where the previous .env went:\n%s", out)
	}
	written, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(written), "ECHOFOG_INVITES=true") {
		t.Errorf("the new .env does not carry the run's values:\n%s", written)
	}
}

// firstShBlock returns the first fenced sh block of a guide: the quick-start commands.
func firstShBlock(t *testing.T, doc string) string {
	t.Helper()
	const open = "```sh\n"
	i := strings.Index(doc, open)
	if i < 0 {
		t.Fatal("the guide has no sh block")
	}
	rest := doc[i+len(open):]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatal("the sh block is not closed")
	}
	return rest[:j]
}

// quickStartSection returns the part before section 1: the quick start with its explanation.
func quickStartSection(t *testing.T, doc string) string {
	t.Helper()
	if i := strings.Index(doc, "\n## 1."); i >= 0 {
		return doc[:i]
	}
	t.Fatal("the guide has no section 1")
	return ""
}

// The quick start works as printed: the second command opens the invitation listener, the third
// only exists with it, and the compose command is the one up.sh builds (project, override and env).
func TestVPSGuidesQuickStart(t *testing.T) {
	for name, file := range map[string]string{"en": "deploy-vps-v2.md", "ru": "deploy-vps-v2.ru.md"} {
		doc := strings.ReplaceAll(read(t, "..", "..", "..", "docs", file), "\r\n", "\n")
		block := firstShBlock(t, doc)
		for _, must := range []string{
			"up.sh --advertise", "--invites", "invite create", "--out /data/invite.txt",
			"-p v2", "-f deploy/v2/docker-compose.yml", "-f deploy/v2/docker-compose.up.yml",
			"--env-file deploy/v2/.env", "exec -T node",
		} {
			if !strings.Contains(block, must) {
				t.Errorf("%s quick start lacks %q:\n%s", name, must, block)
			}
		}
		section := quickStartSection(t, doc)
		firstStart, emptyVolume := "first start", "empty volume"
		if name == "ru" {
			firstStart, emptyVolume = "первом запуске", "пустой том"
		}
		for _, must := range []string{firstStart, emptyVolume} {
			if !strings.Contains(section, must) {
				t.Errorf("%s quick start does not warn that the flags shape the profile: lacks %q", name, must)
			}
		}
	}
}
