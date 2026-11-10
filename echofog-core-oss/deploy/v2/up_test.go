package v2deploy

import (
	"os"
	"os/exec"
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
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, append([]string{upScript}, args...)...)
	// A docker that always fails: a dry run must not call it at all.
	cmd.Env = append(os.Environ(), "ECHOFOG_DOCKER=false")
	out, err := cmd.CombinedOutput()
	return string(out), err
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
	}
}

// The plan of a test run names the throwaway container and volume, uses ports from 8443 and shows
// the commands that remove everything afterwards.
func TestUpScriptTestRunPlan(t *testing.T) {
	out, err := runUp(t, "--advertise", "203.0.113.10", "--test", "--dry-run")
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
		"invite create", "--out /data/invite.txt",
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
