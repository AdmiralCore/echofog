package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/release"
)

func do(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out)
	return out.String(), err
}

func mustDo(t *testing.T, args ...string) string {
	t.Helper()
	out, err := do(t, args...)
	if err != nil {
		t.Fatalf("efv2-release %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// keygen makes a key and returns its private file and its public text.
func keygen(t *testing.T, dir, name string) (priv, pub string) {
	t.Helper()
	base := filepath.Join(dir, name)
	out := mustDo(t, "keygen", "--out", base)
	pub = strings.TrimSpace(out)
	if _, err := keys.ParseText(pub); err != nil {
		t.Fatalf("keygen printed %q, not a public key: %v", out, err)
	}
	return base + ".key", pub
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestKeygen(t *testing.T) {
	dir := t.TempDir()
	priv, pub := keygen(t, dir, "k1")
	seed, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(seed)) == 0 {
		t.Fatal("empty private key file")
	}
	// The seed never goes to stdout, and the .pub file holds the public text.
	out := mustDo(t, "keygen", "--out", filepath.Join(dir, "k2"))
	k2, _ := os.ReadFile(filepath.Join(dir, "k2.key"))
	if len(bytes.TrimSpace(k2)) == 0 || strings.Contains(out, strings.TrimSpace(string(k2))) {
		t.Fatal("the private seed was printed or not written")
	}
	if p, err := os.ReadFile(filepath.Join(dir, "k1.pub")); err != nil || strings.TrimSpace(string(p)) != pub {
		t.Fatalf("k1.pub: %q, %v", p, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(priv); fi.Mode().Perm() != 0o600 {
			t.Fatalf("private key mode %v", fi.Mode().Perm())
		}
	}
	// Never overwrites a key.
	if _, err := do(t, "keygen", "--out", filepath.Join(dir, "k1")); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
}

func TestKeygenRefusesAGitWorkTree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, deep} {
		if _, err := do(t, "keygen", "--out", filepath.Join(d, "k")); err == nil {
			t.Fatalf("keygen wrote a private key inside a git work tree (%s)", d)
		}
	}
	if _, err := os.Stat(filepath.Join(deep, "k.key")); err == nil {
		t.Fatal("the key file exists")
	}
}

// TestSignedManifestIsAcceptedByTheNode runs the owner's whole offline flow and feeds the result
// to the node's manager: 2 of 3 signatures pass, 1 does not.
func TestSignedManifestIsAcceptedByTheNode(t *testing.T) {
	dir := t.TempDir()
	var privs, pubs []string
	for _, n := range []string{"k1", "k2", "k3"} {
		p, u := keygen(t, dir, n)
		privs, pubs = append(privs, p), append(pubs, u)
	}
	releasedAt := time.Now().Add(-time.Hour).Unix()
	notAfter := time.Now().Add(30 * 24 * time.Hour).Unix()
	manifest := filepath.Join(dir, "m.bin")
	mustDo(t, "assemble", "--version", "0.3.1", "--released-at", itoa(releasedAt),
		"--notes-url", "https://example.org/notes",
		"--policy", "proto=200,210",
		"--artifact", "linux-amd64,efv2.tar.gz,"+strings.Repeat("ab", 32)+",1234,https://example.org/efv2.tar.gz",
		"--out", manifest)

	cos := filepath.Join(dir, "k2.cosig")
	mustDo(t, "cosign", "--manifest", manifest, "--key", privs[1], "--out", cos)
	signed := filepath.Join(dir, "release.bin")
	mustDo(t, "sign", "--manifest", manifest, "--key", privs[0], "--cosig", cos, "--not-after", itoa(notAfter), "--out", signed)

	out := mustDo(t, "verify", "--keys", strings.Join(pubs, ","), signed)
	if !strings.Contains(out, "0.3.1") {
		t.Fatalf("verify printed %q", out)
	}
	// The node accepts it.
	var compiled []keys.AKey
	for _, p := range pubs {
		k, _ := keys.ParseText(p)
		compiled = append(compiled, k)
	}
	raw, err := os.ReadFile(signed)
	if err != nil {
		t.Fatal(err)
	}
	m, err := release.Open(release.Config{Keys: compiled, KV: nil, Clock: wallClock{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(raw, time.Now()); err != nil {
		t.Fatalf("the node refuses the tool's output: %v", err)
	}

	// Without the co-signature the object has one signature: verify fails, the tool does not hide it.
	one := filepath.Join(dir, "one.bin")
	mustDo(t, "sign", "--manifest", manifest, "--key", privs[0], "--not-after", itoa(notAfter), "--out", one)
	if _, err := do(t, "verify", "--keys", strings.Join(pubs, ","), one); err == nil {
		t.Fatal("verify accepts one signature")
	}
	// Another key set does not verify either.
	_, otherPub := keygen(t, dir, "k9")
	if _, err := do(t, "verify", "--keys", otherPub+","+pubs[0]+","+pubs[2], signed); err == nil {
		t.Fatal("verify accepts a manifest signed by keys outside the set")
	}
}

func TestAssembleRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "m.bin")
	base := []string{"assemble", "--version", "0.3.1", "--released-at", "1800000000", "--out", out}
	for name, extra := range map[string][]string{
		"http notes":      {"--notes-url", "http://example.org/n"},
		"http artifact":   {"--artifact", "linux-amd64,a," + strings.Repeat("ab", 32) + ",1,http://example.org/a"},
		"short hash":      {"--artifact", "linux-amd64,a,abcd,1,https://example.org/a"},
		"policy no equal": {"--policy", "proto"},
		"policy numbers":  {"--policy", "proto=x,y"},
		"bad next key":    {"--next-key", "nonsense"},
	} {
		if _, err := do(t, append(append([]string{}, base...), extra...)...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := do(t, "assemble", "--version", "", "--released-at", "1", "--out", out); err == nil {
		t.Error("empty version accepted")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a refused assemble left a file")
	}
}

func TestUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{}, {"nothing"}, {"sign"}, {"verify"}} {
		if _, err := do(t, args...); err == nil {
			t.Errorf("run(%v) succeeded", args)
		}
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }
