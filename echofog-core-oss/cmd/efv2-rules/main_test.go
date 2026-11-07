package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

const now = 1_800_000_000

func do(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out)
	return out.String(), err
}

type rig struct {
	dir, key, pub string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "rules")
	out, err := do(t, "keygen", "--out", base)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return &rig{dir: dir, key: base + ".key", pub: strings.TrimSpace(out)}
}

func (r *rig) body(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(r.dir, "body.json")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const bodyJSON = `{"version":"2026-11","allowed_cidrs":["198.51.100.0/24"],"neighbors":{"RU":["KZ","BY"]},"tcp_limit":20000,"udp_denied":["RU"]}`

func at(sec int64) func() time.Time { return func() time.Time { return time.Unix(sec, 0) } }

func TestKeygen(t *testing.T) {
	r := newRig(t)
	if len(r.pub) != 64 {
		t.Fatalf("public key %q", r.pub)
	}
	raw, err := os.ReadFile(r.key)
	if err != nil || len(strings.TrimSpace(string(raw))) != 64 {
		t.Fatalf("key file: %v", err)
	}
	if fi, _ := os.Stat(r.key); os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode())
	}
	if _, err := do(t, "keygen", "--out", strings.TrimSuffix(r.key, ".key")); err == nil {
		t.Error("keygen overwrote an existing key")
	}
	// Not inside a git work tree: the private key belongs on offline media.
	repo := t.TempDir()
	_ = os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	if _, err := do(t, "keygen", "--out", filepath.Join(repo, "k")); err == nil {
		t.Error("keygen wrote a key inside a git work tree")
	}
	if _, err := os.Stat(filepath.Join(repo, "k.key")); err == nil {
		t.Error("the refused key exists")
	}
}

func TestSignAndVerify(t *testing.T) {
	r := newRig(t)
	out := filepath.Join(r.dir, "rules.bin")
	if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--out", out, "--now", strconv.Itoa(now)); err != nil {
		t.Fatalf("sign: %v", err)
	}
	data, _ := os.ReadFile(out)
	pubRaw, _ := hex.DecodeString(r.pub)
	rules, err := rp.ParseAt(data, ed25519.PublicKey(pubRaw), 0, at(now))
	if err != nil {
		t.Fatalf("the signed file does not parse: %v", err)
	}
	if rules.Seq != 1 {
		t.Errorf("a first file has seq 1, got %d", rules.Seq)
	}
	if !rules.ValidUntil().Equal(time.Unix(now, 0).Add(30 * 24 * time.Hour)) {
		t.Errorf("valid_until %v: want 30 days", rules.ValidUntil())
	}
	if !rules.HasVerifiedEntries() {
		t.Error("the entries of the body are lost")
	}
	sum, err := do(t, "verify", "--pub", r.pub, "--in", out, "--now", strconv.Itoa(now))
	if err != nil || !strings.Contains(sum, "seq 1") || !strings.Contains(sum, "30 days") {
		t.Fatalf("verify: %v\n%s", err, sum)
	}
	if strings.Contains(sum, "198.51.100") {
		t.Error("verify prints the addresses of the file")
	}

	// Damage, a foreign key, an old seq, an expired file.
	bad := append([]byte(nil), data...)
	bad[len(bad)-3] ^= 1
	badPath := filepath.Join(r.dir, "bad.bin")
	_ = os.WriteFile(badPath, bad, 0o644)
	if _, err := do(t, "verify", "--pub", r.pub, "--in", badPath, "--now", strconv.Itoa(now)); err == nil {
		t.Error("a damaged file verified")
	}
	other := newRig(t)
	if _, err := do(t, "verify", "--pub", other.pub, "--in", out, "--now", strconv.Itoa(now)); err == nil {
		t.Error("a file verified under a foreign key")
	}
	if _, err := do(t, "verify", "--pub", r.pub, "--in", out, "--min-seq", "2", "--now", strconv.Itoa(now)); err == nil {
		t.Error("an old seq verified")
	}
	late := strconv.Itoa(now + 31*24*3600)
	expired, err := do(t, "verify", "--pub", r.pub, "--in", out, "--now", late)
	if err != nil || !strings.Contains(strings.ToLower(expired), "expired") {
		t.Errorf("an expired file must be reported as expired (the signature is still good): %v\n%s", err, expired)
	}
}

func TestSeqIsMonotonic(t *testing.T) {
	r := newRig(t)
	first := filepath.Join(r.dir, "r1.bin")
	if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--out", first, "--now", strconv.Itoa(now)); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(r.dir, "r2.bin")
	later := int64(now + 30*24*3600)
	if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--prev", first, "--out", second, "--now", strconv.FormatInt(later, 10)); err != nil {
		t.Fatalf("second file: %v", err)
	}
	data, _ := os.ReadFile(second)
	pubRaw, _ := hex.DecodeString(r.pub)
	rules, err := rp.ParseAt(data, ed25519.PublicKey(pubRaw), 0, at(later))
	if err != nil || rules.Seq != 2 {
		t.Fatalf("the update after --prev must be seq 2: %v %v", err, rules)
	}
	for name, args := range map[string][]string{
		"same seq":         {"--prev", first, "--seq", "1"},
		"lower seq":        {"--prev", second, "--seq", "1"},
		"seq jump to zero": {"--seq", "0"},
	} {
		a := append([]string{"sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--out", filepath.Join(r.dir, "x.bin"), "--now", strconv.Itoa(now)}, args...)
		if _, err := do(t, a...); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	// The previous file must be genuine: a foreign one is not a base for the next seq.
	other := newRig(t)
	foreign := filepath.Join(other.dir, "f.bin")
	if _, err := do(t, "sign", "--key", other.key, "--body", other.body(t, bodyJSON), "--out", foreign, "--now", strconv.Itoa(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--prev", foreign, "--out", filepath.Join(r.dir, "y.bin"), "--now", strconv.Itoa(now)); err == nil {
		t.Error("a --prev of another key was accepted")
	}
}

func TestValidityBounds(t *testing.T) {
	r := newRig(t)
	for _, days := range []string{"0", "-1", "46", "365", "abc"} {
		if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--out", filepath.Join(r.dir, "v.bin"), "--valid-days", days, "--now", strconv.Itoa(now)); err == nil {
			t.Errorf("--valid-days %s was accepted", days)
		}
	}
	if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, bodyJSON), "--out", filepath.Join(r.dir, "v2.bin"), "--valid-days", "45", "--now", strconv.Itoa(now)); err != nil {
		t.Errorf("45 days: %v", err)
	}
}

// The tool sets seq and valid_until itself; a body cannot smuggle its own, and a bad body is refused
// before anything is signed.
func TestBodyIsCheckedAndOwnFieldsAreIgnored(t *testing.T) {
	r := newRig(t)
	out := filepath.Join(r.dir, "o.bin")
	if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, `{"seq":99,"valid_until":1,"allowed_cidrs":[]}`), "--out", out, "--now", strconv.Itoa(now)); err != nil {
		t.Fatalf("sign: %v", err)
	}
	data, _ := os.ReadFile(out)
	var b map[string]any
	if err := json.Unmarshal(data[64:], &b); err != nil {
		t.Fatal(err)
	}
	if b["seq"].(float64) != 1 || int64(b["valid_until"].(float64)) != now+30*24*3600 {
		t.Errorf("body fields: %v", b)
	}
	for name, body := range map[string]string{
		"bad cidr": `{"allowed_cidrs":["not-a-prefix"]}`, "bad country": `{"neighbors":{"russia":["KZ"]}}`, "not json": `{`,
		"negative limit": `{"tcp_limit":-1}`, "huge": `{"version":"` + strings.Repeat("a", 70000) + `"}`,
	} {
		target := filepath.Join(r.dir, "b.bin")
		if _, err := do(t, "sign", "--key", r.key, "--body", r.body(t, body), "--out", target, "--now", strconv.Itoa(now)); err == nil {
			t.Errorf("%s was signed", name)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("%s: an output file exists", name)
			_ = os.Remove(target)
		}
	}
}

// The first file of the repository is a template: it signs, and it carries no address at all.
func TestFirstFileTemplate(t *testing.T) {
	tpl := filepath.Join("..", "..", "deploy", "rules", "first-rules.example.json")
	raw, err := os.ReadFile(tpl)
	if err != nil {
		t.Fatalf("the template: %v", err)
	}
	if regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`).Match(raw) {
		t.Error("the template contains an address")
	}
	r := newRig(t)
	out := filepath.Join(r.dir, "first.bin")
	if _, err := do(t, "sign", "--key", r.key, "--body", tpl, "--out", out, "--now", strconv.Itoa(now)); err != nil {
		t.Fatalf("the template does not sign: %v", err)
	}
	data, _ := os.ReadFile(out)
	pubRaw, _ := hex.DecodeString(r.pub)
	rules, err := rp.ParseAt(data, ed25519.PublicKey(pubRaw), 0, at(now))
	if err != nil {
		t.Fatal(err)
	}
	if rules.HasVerifiedEntries() {
		t.Error("the first file claims verified entries: none are verified before T15.2a")
	}
	if got := rules.Classify("RU", "KZ"); got != rp.DestNeighbor {
		t.Errorf("RU to KZ is %v: the neighbours of the table belong in the first file", got)
	}
}
