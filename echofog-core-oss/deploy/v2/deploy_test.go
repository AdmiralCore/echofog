// Package v2deploy checks the deployment files of the v2 server node statically: Docker does not
// run on every development machine, and the properties that matter most (no root, no secrets in
// layers, no open debug ports, a closed container) are visible in the files.
package v2deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	dockerfile = "Dockerfile"
	compose    = "docker-compose.yml"
	envExample = ".env.example"
	entrypoint = "entrypoint.sh"
)

func read(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rel...))
	if err != nil {
		t.Fatalf("%v", err)
	}
	return string(b)
}

// instructions returns the Dockerfile instructions with continuation lines joined.
func instructions(t *testing.T) []string {
	t.Helper()
	text := strings.ReplaceAll(read(t, dockerfile), "\r\n", "\n")
	var out []string
	var cur string
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if cur == "" && (trim == "" || strings.HasPrefix(trim, "#")) {
			continue
		}
		if strings.HasSuffix(trim, "\\") {
			cur += strings.TrimSuffix(trim, "\\") + " "
			continue
		}
		out = append(out, strings.TrimSpace(cur+trim))
		cur = ""
	}
	return out
}

func stages(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	for _, in := range instructions(t) {
		if strings.HasPrefix(strings.ToUpper(in), "FROM ") {
			out = append(out, nil)
		}
		if len(out) > 0 {
			out[len(out)-1] = append(out[len(out)-1], in)
		}
	}
	if len(out) == 0 {
		t.Fatal("no FROM in the Dockerfile")
	}
	return out
}

func op(in string) string {
	f := strings.Fields(in)
	if len(f) == 0 {
		return ""
	}
	return strings.ToUpper(f[0])
}

func TestBaseImagesArePinned(t *testing.T) {
	goMod := read(t, "..", "..", "go.mod")
	want := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`).FindStringSubmatch(goMod)
	if want == nil {
		t.Fatal("no go version in go.mod")
	}
	sawBuilder := false
	for _, in := range instructions(t) {
		if op(in) != "FROM" {
			continue
		}
		f := strings.Fields(in)
		img := f[1]
		if f[1] == "--platform=$BUILDPLATFORM" {
			img = f[2]
		}
		name, tag, ok := strings.Cut(img, ":")
		if !ok || tag == "" || tag == "latest" || strings.Contains(tag, "latest") || name == "scratch" {
			t.Errorf("base image %q is not pinned to a version", img)
		}
		if strings.HasPrefix(img, "golang:") {
			sawBuilder = true
			if !strings.HasPrefix(tag, want[1]) {
				t.Errorf("builder %q does not match go %s of go.mod: the build must use the toolchain the code is tested with", img, want[1])
			}
		}
	}
	if !sawBuilder {
		t.Error("no golang builder stage")
	}
}

func TestImageRunsAsAnUnprivilegedUser(t *testing.T) {
	st := stages(t)
	final := st[len(st)-1]
	user := ""
	for _, in := range final {
		if op(in) == "USER" {
			user = strings.Fields(in)[1]
		}
	}
	if user == "" || user == "root" || strings.HasPrefix(user, "0") {
		t.Fatalf("the final stage runs as %q", user)
	}
	if !regexp.MustCompile(`^[1-9]\d{3,}(:[1-9]\d{3,})?$|^[a-z][a-z0-9_-]*$`).MatchString(user) {
		t.Fatalf("user %q", user)
	}
	// The data directory belongs to that user and nobody else can write it.
	text := strings.Join(final, "\n")
	if !strings.Contains(text, "chown") || !strings.Contains(text, "/data") {
		t.Error("/data is not handed to the unprivileged user")
	}
	if strings.Contains(text, "chmod 777") || strings.Contains(text, "chmod -R 777") {
		t.Error("world-writable paths")
	}
}

func TestNoSecretsInLayers(t *testing.T) {
	secretName := regexp.MustCompile(`(?i)(secret|token|passw|mnemonic|private|seed|api[_-]?key)`)
	for _, in := range instructions(t) {
		switch op(in) {
		case "ENV", "ARG":
			for _, kv := range strings.Fields(strings.TrimSpace(in[len(op(in)):])) {
				name, _, _ := strings.Cut(kv, "=")
				if secretName.MatchString(name) {
					t.Errorf("%s declares %q: a secret in an image layer", op(in), name)
				}
			}
		case "COPY", "ADD":
			for _, bad := range []string{".env", "identity", ".key", ".pem", "operator.token", "recovery"} {
				if strings.Contains(strings.ToLower(in), bad) && !strings.Contains(in, "--from=") {
					t.Errorf("%q copies something named like a secret", in)
				}
			}
			if op(in) == "ADD" {
				t.Errorf("ADD %q: use COPY (ADD fetches URLs and unpacks archives)", in)
			}
		case "RUN":
			if regexp.MustCompile(`(curl|wget)[^|;&]*\|\s*(sh|bash)`).MatchString(in) {
				t.Errorf("RUN pipes a download into a shell: %q", in)
			}
			if strings.Contains(in, "apk add") && !strings.Contains(in, "--no-cache") {
				t.Errorf("apk add without --no-cache: %q", in)
			}
		}
	}
	ign := read(t, "..", "..", ".dockerignore")
	for _, must := range []string{".env", "server-identity.json", "operator.token", "*.key", "*.pem", ".git"} {
		if !strings.Contains(ign, must) {
			t.Errorf(".dockerignore does not exclude %s", must)
		}
	}
}

func TestBuildIsReproducibleAndStatic(t *testing.T) {
	var build string
	for _, in := range instructions(t) {
		if op(in) == "RUN" && strings.Contains(in, "go build") {
			build = in
		}
	}
	if build == "" {
		t.Fatal("no go build")
	}
	for _, flag := range []string{"CGO_ENABLED=0", "-trimpath", "-buildvcs=false", "-s -w", "./cmd/echofog-node"} {
		if !strings.Contains(build, flag) {
			t.Errorf("go build lacks %q: %s", flag, build)
		}
	}
	// go.mod and go.sum before the sources: a source change does not redownload the modules.
	text := read(t, dockerfile)
	if i, j := strings.Index(text, "COPY go.mod go.sum"), strings.Index(text, "COPY . ."); i < 0 || j < 0 || i > j {
		t.Error("go.mod and go.sum must be copied before the sources")
	}
	if !strings.Contains(text, "COPY third_party") {
		t.Error("third_party (the local quic-go fork) is needed before go mod download")
	}
}

func TestEntrypointAndHealthcheck(t *testing.T) {
	var ep, hc, cmd string
	for _, in := range instructions(t) {
		switch op(in) {
		case "ENTRYPOINT":
			ep = in
		case "HEALTHCHECK":
			hc = in
		case "CMD":
			cmd = in
		}
	}
	if !strings.Contains(ep, `["`) || !strings.Contains(ep, entrypoint) {
		t.Errorf("ENTRYPOINT must be the exec form of %s: %q", entrypoint, ep)
	}
	if !strings.Contains(cmd, `"v2"`) || !strings.Contains(cmd, `"run"`) {
		t.Errorf("CMD must run the v2 node: %q", cmd)
	}
	for _, want := range []string{"--interval", "--start-period", "--retries", "v2", "status", "/data"} {
		if !strings.Contains(hc, want) {
			t.Errorf("HEALTHCHECK lacks %q: %q", want, hc)
		}
	}
	// Only the node's own UDP port is exposed: no TCP, no operator API, no debug port.
	for _, in := range instructions(t) {
		if op(in) != "EXPOSE" {
			continue
		}
		for _, p := range strings.Fields(in)[1:] {
			if p != "443/udp" {
				t.Errorf("EXPOSE %s", p)
			}
		}
	}
	if !regexp.MustCompile(`(?m)^VOLUME\s+\["/data"\]`).MatchString(read(t, dockerfile)) {
		t.Error(`VOLUME ["/data"] missing`)
	}
}

func TestEntrypointScript(t *testing.T) {
	sh := read(t, entrypoint)
	if !strings.HasPrefix(sh, "#!/bin/sh\n") {
		t.Error("the entrypoint is a POSIX sh script")
	}
	for _, must := range []string{"set -eu", "umask 077", "exec echofog-node v2 run", "ECHOFOG_ADVERTISE_ADDR", "recovery-phrase.txt"} {
		if !strings.Contains(sh, must) {
			t.Errorf("entrypoint lacks %q", must)
		}
	}
	for _, bad := range []string{"set -x", "eval ", "curl ", "wget ", "chmod 777"} {
		if strings.Contains(sh, bad) {
			t.Errorf("entrypoint contains %q", bad)
		}
	}
	// The recovery phrase goes to a private file, never to the container log.
	if !regexp.MustCompile(`init[^\n]*>\s*"?\$?\{?\w*[^\n]*recovery-phrase\.txt`).MatchString(sh) &&
		!strings.Contains(sh, `> "$PHRASE_FILE"`) {
		t.Error("the output of init (it contains the recovery phrase) must be redirected to a file")
	}
	// A missing or malformed advertised address stops the container with a message.
	if !strings.Contains(sh, "exit 1") || !strings.Contains(sh, "ECHOFOG_ADVERTISE_ADDR is required") {
		t.Error("the entrypoint must refuse to start without ECHOFOG_ADVERTISE_ADDR")
	}
	if p, err := exec.LookPath("sh"); err == nil {
		if out, err := exec.Command(p, "-n", entrypoint).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	}
}

type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		User        string            `yaml:"user"`
		ReadOnly    bool              `yaml:"read_only"`
		CapDrop     []string          `yaml:"cap_drop"`
		CapAdd      []string          `yaml:"cap_add"`
		SecurityOpt []string          `yaml:"security_opt"`
		Privileged  bool              `yaml:"privileged"`
		NetworkMode string            `yaml:"network_mode"`
		PidMode     string            `yaml:"pid"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
		Tmpfs       []string          `yaml:"tmpfs"`
		PidsLimit   int               `yaml:"pids_limit"`
		MemLimit    string            `yaml:"mem_limit"`
		Restart     string            `yaml:"restart"`
		StopGrace   string            `yaml:"stop_grace_period"`
		Environment map[string]string `yaml:"environment"`
		EnvFile     any               `yaml:"env_file"`
		Logging     struct {
			Options map[string]string `yaml:"options"`
		} `yaml:"logging"`
		Devices []string `yaml:"devices"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

func TestComposeIsClosed(t *testing.T) {
	var c composeFile
	if err := yaml.Unmarshal([]byte(read(t, compose)), &c); err != nil {
		t.Fatalf("compose: %v", err)
	}
	n, ok := c.Services["node"]
	if !ok || len(c.Services) != 1 {
		t.Fatalf("want exactly one service \"node\", got %v", c.Services)
	}
	if !n.ReadOnly {
		t.Error("read_only: true is missing")
	}
	if len(n.CapDrop) != 1 || n.CapDrop[0] != "ALL" || len(n.CapAdd) != 0 {
		t.Errorf("capabilities: drop %v add %v; drop ALL and add none (UDP above 1024 needs none)", n.CapDrop, n.CapAdd)
	}
	if !contains(n.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("security_opt %v lacks no-new-privileges:true", n.SecurityOpt)
	}
	if n.Privileged || n.NetworkMode == "host" || n.PidMode != "" || len(n.Devices) != 0 {
		t.Errorf("privileged=%v network_mode=%q pid=%q devices=%v", n.Privileged, n.NetworkMode, n.PidMode, n.Devices)
	}
	if !regexp.MustCompile(`^[1-9]\d{3,}:[1-9]\d{3,}$`).MatchString(n.User) {
		t.Errorf("user %q: set a numeric non-root uid:gid", n.User)
	}
	if n.PidsLimit <= 0 || n.MemLimit == "" {
		t.Errorf("pids_limit %d, mem_limit %q: a server node needs both", n.PidsLimit, n.MemLimit)
	}
	if n.Restart != "unless-stopped" || n.StopGrace == "" {
		t.Errorf("restart %q, stop_grace_period %q", n.Restart, n.StopGrace)
	}
	if n.Logging.Options["max-size"] == "" || n.Logging.Options["max-file"] == "" {
		t.Error("log rotation is not set: the log would fill the disk")
	}
	if len(n.Tmpfs) == 0 {
		t.Error("a read-only container needs a tmpfs for /tmp")
	}
	// Ports: the node's public TCP and UDP port (443, the camouflaged pair) and the noise3 spare
	// port; none for the operator API, nothing published on loopback by accident, no host-wide bind.
	tcp, udp := 0, 0
	for _, p := range n.Ports {
		if strings.HasSuffix(p, "/udp") {
			udp++
		} else {
			tcp++
		}
		if strings.HasPrefix(p, "127.") || strings.HasPrefix(p, "0.0.0.0") || strings.Contains(p, "8090") || strings.Contains(p, "6060") {
			t.Errorf("published port %q", p)
		}
	}
	if tcp != 1 || udp != 2 {
		t.Errorf("%d TCP and %d UDP published ports, want 443/tcp, 443/udp and the noise3 spare port", tcp, udp)
	}
	// Storage: one named volume. No host paths, no Docker socket.
	if len(n.Volumes) != 1 || !strings.HasSuffix(n.Volumes[0], ":/data") || strings.HasPrefix(n.Volumes[0], "/") || strings.HasPrefix(n.Volumes[0], ".") {
		t.Errorf("volumes %v: want one named volume on /data", n.Volumes)
	}
	// The image tag follows the v2 integration line.
	if !strings.Contains(n.Image, "v2-dev") || strings.Contains(n.Image, ":latest") {
		t.Errorf("image %q: default to the v2-dev tag", n.Image)
	}
	for k := range n.Environment {
		if regexp.MustCompile(`(?i)(secret|token|passw|mnemonic)`).MatchString(k) {
			t.Errorf("environment %s: secrets do not belong in the compose file", k)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestEnvExample(t *testing.T) {
	env := read(t, envExample)
	set := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(env, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("line %q", line)
			continue
		}
		set[k] = v
	}
	for _, k := range []string{"ECHOFOG_IMAGE", "ECHOFOG_ADVERTISE_ADDR", "ECHOFOG_ROLES", "ECHOFOG_PORT"} {
		if _, ok := set[k]; !ok {
			t.Errorf("%s is missing from %s", k, envExample)
		}
	}
	if set["ECHOFOG_ADVERTISE_ADDR"] != "" {
		t.Error("ECHOFOG_ADVERTISE_ADDR must be empty: the operator's own address is not ours to ship")
	}
	if strings.Contains(set["ECHOFOG_ROLES"], "exit") {
		t.Error("exit must be off in the example")
	}
	// No addresses but documentation ranges, no keys, no e-mails, no tokens.
	for _, ip := range regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`).FindAllString(env, -1) {
		if !strings.HasPrefix(ip, "203.0.113.") && !strings.HasPrefix(ip, "198.51.100.") && !strings.HasPrefix(ip, "192.0.2.") &&
			ip != "0.0.0.0" && ip != "127.0.0.1" {
			t.Errorf("address %s in %s: only documentation ranges (RFC 5737) are allowed", ip, envExample)
		}
	}
	if regexp.MustCompile(`[0-9a-fA-F]{32,}`).MatchString(env) || regexp.MustCompile(`\S+@\S+\.\S+`).MatchString(env) {
		t.Errorf("%s contains a key-like string or an e-mail address", envExample)
	}
	for k, v := range set {
		if regexp.MustCompile(`(?i)(secret|token|passw|mnemonic)`).MatchString(k) && v != "" {
			t.Errorf("%s has a value", k)
		}
	}
	// Every ECHOFOG_* variable the compose file and the entrypoint read is described here.
	used := map[string]bool{}
	for _, f := range []string{read(t, compose), read(t, entrypoint)} {
		for _, m := range regexp.MustCompile(`\$\{?(ECHOFOG_[A-Z_]+)`).FindAllStringSubmatch(f, -1) {
			used[m[1]] = true
		}
	}
	for k := range used {
		if _, ok := set[k]; !ok && !strings.Contains(env, "# "+k+"=") {
			t.Errorf("%s is used but not described in %s", k, envExample)
		}
	}
}

func TestWorkflowBuildsTheV2Tag(t *testing.T) {
	wf := read(t, "..", "..", "..", ".github", "workflows", "node-image.yml")
	for _, want := range []string{"v2-dev", "refs/heads/v2", "deploy/v2/Dockerfile"} {
		if !strings.Contains(wf, want) {
			t.Errorf("node-image.yml lacks %q", want)
		}
	}
	if !regexp.MustCompile(`branches:\s*\[[^\]]*\bv2\b`).MatchString(wf) {
		t.Error("node-image.yml does not run on pushes to v2")
	}
	if !strings.Contains(wf, "type=raw,value=latest,enable={{is_default_branch}}") {
		t.Error("latest must stay on the default branch only")
	}
	if strings.Contains(wf, "secrets.") && !strings.Contains(wf, "secrets.GITHUB_TOKEN") {
		t.Error("the workflow uses a secret other than GITHUB_TOKEN")
	}
	if _, err := os.Stat("Dockerfile"); err != nil {
		t.Error(err)
	}
}
