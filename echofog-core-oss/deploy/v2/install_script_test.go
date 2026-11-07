package v2deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/installkit"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// These tests run install.sh itself, in a sandbox: a fake curl that serves files from a folder, a
// fake "binary" that only writes a marker when it is executed, and test keys. They need Linux, root
// (the script refuses to run otherwise) and the real ssh-keygen: the point is to prove that the script
// refuses before the downloaded file is ever executed.

func sk(t testing.TB, n byte) *keys.Signer {
	t.Helper()
	seed := sha256.Sum256([]byte{'s', 'c', n})
	s, err := keys.NewEd25519Signer(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type scriptRig struct {
	t         *testing.T
	dir       string // everything of the test
	serve     string // what the fake curl serves
	bin       string // fake curl and helpers
	tmp       string // TMPDIR of the script: must be empty after a run
	marker    string
	signers   []*keys.Signer // the release keys
	arch      string
	installed string // the "installed" binary, if any
}

func newScriptRig(t *testing.T) *scriptRig {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("install.sh is Linux only")
	}
	if os.Geteuid() != 0 {
		t.Skip("install.sh requires root; run in a container or WSL as root")
	}
	for _, tool := range []string{"ssh-keygen", "sha256sum", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	r := &scriptRig{t: t, dir: t.TempDir()}
	r.serve, r.bin, r.tmp = filepath.Join(r.dir, "serve"), filepath.Join(r.dir, "bin"), filepath.Join(r.dir, "tmp")
	for _, d := range []string{r.serve, r.bin, r.tmp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.marker = filepath.Join(r.dir, "marker")
	r.signers = []*keys.Signer{sk(t, 1), sk(t, 2), sk(t, 3)}
	r.arch = map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if r.arch == "" {
		t.Skip("unsupported processor")
	}
	// The fake curl: the last argument is the URL, "-o FILE" the target; the file is the base name
	// in the serve folder. It keeps a log of the URLs.
	curl := "#!/bin/sh\nurl=\"\"; out=\"\"\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) out=\"$2\"; shift 2;; *) url=\"$1\"; shift;; esac; done\n" +
		"echo \"$url\" >> \"" + filepath.Join(r.dir, "urls") + "\"\n" +
		"case \"$url\" in https://*) ;; *) exit 1;; esac\n" +
		"f=\"" + r.serve + "/${url##*/}\"\n[ -f \"$f\" ] || exit 22\ncp \"$f\" \"$out\"\n"
	if err := os.WriteFile(filepath.Join(r.bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *scriptRig) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.serve, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func fakeBinary(marker, tag string) string {
	return "#!/bin/sh\n# " + tag + "\necho \"$@\" > " + marker + "\n"
}

// release serves a signed release: the binary, the record, its signatures and a manifest file.
type relOpts struct {
	version, min string
	epoch        uint32
	binary       string // what is served as the binary (the record signs binaryForRecord)
	signedBinary string // what the record is made for; default = binary
	signers      []*keys.Signer
	mutateRecord func(string) string // after signing
	sigsNS       string
}

func (r *scriptRig) release(o relOpts) {
	r.t.Helper()
	if o.version == "" {
		o.version = "2.0.1"
	}
	if o.epoch == 0 {
		o.epoch = 1
	}
	if o.binary == "" {
		o.binary = fakeBinary(r.marker, "genuine")
	}
	if o.signedBinary == "" {
		o.signedBinary = o.binary
	}
	if o.signers == nil {
		o.signers = r.signers[:2]
	}
	if o.sigsNS == "" {
		o.sigsNS = installkit.NSRelease
	}
	sum := sha256.Sum256([]byte(o.signedBinary))
	// The record is built by hand where the Go renderer would refuse it (a version below the bound).
	rec := fmt.Sprintf("echofog-release 1\nversion %s\nmin_version %s\nkey_epoch %d\nreleased_at 1800000000\nartifact linux-%s %s %d\n",
		o.version, orDefault(o.min, "0.0.0"), o.epoch, r.arch, hex.EncodeToString(sum[:]), len(o.signedBinary))
	sigs, err := installkit.SignRecord([]byte(rec), o.sigsNS, o.signers...)
	if err != nil {
		r.t.Fatal(err)
	}
	if o.mutateRecord != nil {
		rec = o.mutateRecord(rec)
	}
	r.write("release.txt", rec)
	r.write("release.txt.sigs", string(sigs))
	r.write("manifest.bin", "manifest")
	r.write("echofog-node-linux-"+r.arch, o.binary)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// script returns the install.sh stamped with the rig's keys.
func (r *scriptRig) script(epoch uint32, floor string) string {
	r.t.Helper()
	tpl, err := os.ReadFile("install.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	var pubs []keys.AKey
	for _, s := range r.signers {
		pubs = append(pubs, s.Public())
	}
	out, err := installkit.Stamp(string(tpl), pubs, epoch, floor)
	if err != nil {
		r.t.Fatalf("stamp install.sh: %v", err)
	}
	p := filepath.Join(r.dir, "install.sh")
	if err := os.WriteFile(p, []byte(out), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *scriptRig) installedVersion(v string) {
	r.t.Helper()
	r.installed = filepath.Join(r.dir, "installed-node")
	body := "#!/bin/sh\n[ \"$1\" = version ] && echo \"echofog-node v2 " + v + "\"\n"
	if err := os.WriteFile(r.installed, []byte(body), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

type result struct {
	out  string
	code int
}

// run starts the script with the fake curl in front of the system PATH.
func (r *scriptRig) run(script string, args ...string) result {
	r.t.Helper()
	return r.runPath(script, r.bin+":"+os.Getenv("PATH"), args...)
}

// runPath starts the script with the given PATH.
func (r *scriptRig) runPath(script, path string, args ...string) result {
	r.t.Helper()
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	env := []string{"PATH=" + path, "TMPDIR=" + r.tmp, "ECHOFOG_RELEASE_URL=https://example.org/releases/v2", "HOME=" + r.dir}
	if r.installed != "" {
		env = append(env, "ECHOFOG_INSTALLED_BIN="+r.installed)
	}
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("run: %v", err)
	}
	return result{buf.String(), code}
}

func (r *scriptRig) executed() bool {
	_, err := os.Stat(r.marker)
	return err == nil
}

func (r *scriptRig) refused(res result, why string) {
	r.t.Helper()
	if res.code == 0 {
		r.t.Fatalf("the script succeeded, want a refusal (%s):\n%s", why, res.out)
	}
	if r.executed() {
		r.t.Fatalf("the downloaded file was executed before the refusal (%s):\n%s", why, res.out)
	}
	if left, _ := os.ReadDir(r.tmp); len(left) != 0 {
		r.t.Errorf("the refused download was left on disk: %v", left)
	}
	if why != "" && !strings.Contains(strings.ToLower(res.out), strings.ToLower(why)) {
		r.t.Errorf("the refusal does not say %q:\n%s", why, res.out)
	}
}

func TestScriptInstallsAVerifiedRelease(t *testing.T) {
	r := newScriptRig(t)
	r.release(relOpts{})
	res := r.run(r.script(1, "2.0.0"), "--preset", "relay", "--advertise", "203.0.113.10")
	if res.code != 0 {
		t.Fatalf("exit %d:\n%s", res.code, res.out)
	}
	b, err := os.ReadFile(r.marker)
	if err != nil {
		t.Fatalf("the verified binary was not run: %v", err)
	}
	got := string(b)
	for _, want := range []string{"v2 install", "--binary", "--manifest", "--platform linux-" + r.arch, "--preset relay", "--advertise 203.0.113.10"} {
		if !strings.Contains(got, want) {
			t.Errorf("installer arguments %q lack %q", got, want)
		}
	}
	if left, _ := os.ReadDir(r.tmp); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	urls, _ := os.ReadFile(filepath.Join(r.dir, "urls"))
	for _, u := range strings.Fields(string(urls)) {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("non-https download %s", u)
		}
	}
}

func TestScriptRefusesASwappedBinary(t *testing.T) {
	r := newScriptRig(t)
	genuine := fakeBinary(r.marker, "genuine")
	evil := fakeBinary(r.marker, "EVIL!!!") // the same length as the genuine one
	if len(evil) != len(genuine) {
		t.Fatal("test bug: the swap must keep the size")
	}
	r.release(relOpts{binary: evil, signedBinary: genuine})
	r.refused(r.run(r.script(1, "2.0.0")), "does not match")
	// A different size is refused too.
	r.release(relOpts{binary: evil + "more", signedBinary: genuine})
	r.refused(r.run(r.script(1, "2.0.0")), "")
	// An empty download.
	r.release(relOpts{binary: "", signedBinary: genuine})
	r.write("echofog-node-linux-"+r.arch, "")
	r.refused(r.run(r.script(1, "2.0.0")), "")
}

func TestScriptRefusesASwappedSignature(t *testing.T) {
	r := newScriptRig(t)
	attacker := []*keys.Signer{sk(t, 40), sk(t, 41)}
	r.release(relOpts{signers: attacker})
	r.refused(r.run(r.script(1, "2.0.0")), "signature")

	// A valid record whose text was changed after signing (a higher version, another hash).
	r.release(relOpts{mutateRecord: func(s string) string { return strings.Replace(s, "version 2.0.1", "version 2.0.9", 1) }})
	r.refused(r.run(r.script(1, "2.0.0")), "signature")

	// One release key is not enough, and the same key twice is one key.
	r.release(relOpts{signers: r.signers[:1]})
	r.refused(r.run(r.script(1, "2.0.0")), "signature")
	r.release(relOpts{signers: []*keys.Signer{r.signers[0], r.signers[0]}})
	r.refused(r.run(r.script(1, "2.0.0")), "signature")
	// A genuine key and a foreign one: still one genuine key.
	r.release(relOpts{signers: []*keys.Signer{r.signers[0], attacker[0]}})
	r.refused(r.run(r.script(1, "2.0.0")), "signature")

	// A signature made for another purpose (the rotation namespace) is not a release signature.
	r.release(relOpts{sigsNS: installkit.NSRotation})
	r.refused(r.run(r.script(1, "2.0.0")), "signature")

	// No signature file at all.
	r.release(relOpts{})
	_ = os.Remove(filepath.Join(r.serve, "release.txt.sigs"))
	r.refused(r.run(r.script(1, "2.0.0")), "")
}

func TestScriptRefusesAnOldSignedRelease(t *testing.T) {
	r := newScriptRig(t)
	r.release(relOpts{version: "2.0.1"})
	r.installedVersion("2.1.0")
	s := r.script(1, "2.0.0")
	r.refused(r.run(s), "older")

	// The explicit flag allows it, with a warning.
	res := r.run(s, "--allow-rollback", "--preset", "relay")
	if res.code != 0 || !r.executed() || !strings.Contains(strings.ToLower(res.out), "rollback") {
		t.Fatalf("--allow-rollback: exit %d, executed %v:\n%s", res.code, r.executed(), res.out)
	}
	if b, _ := os.ReadFile(r.marker); strings.Contains(string(b), "allow-rollback") && !strings.Contains(string(b), "--preset relay") {
		t.Errorf("arguments were lost: %s", b)
	}
	_ = os.Remove(r.marker)

	// The flag never goes below the signed min_version ...
	r.release(relOpts{version: "2.0.1", min: "2.0.5"})
	r.refused(r.run(s, "--allow-rollback"), "min_version")
	// ... nor below the floor stamped into the script.
	r.release(relOpts{version: "2.0.1"})
	r.refused(r.run(r.script(1, "2.0.5"), "--allow-rollback"), "floor")
	// Without an installed binary a fresh install of a version below the floor is refused as well.
	r.installed = ""
	r.refused(r.run(r.script(1, "2.0.5")), "floor")

	// The same version is not a rollback.
	r.installedVersion("2.0.1")
	r.release(relOpts{version: "2.0.1"})
	if res := r.run(r.script(1, "2.0.0")); res.code != 0 {
		t.Fatalf("the same version: %d\n%s", res.code, res.out)
	}
}

func TestScriptRefusesWithoutTheVerificationTool(t *testing.T) {
	r := newScriptRig(t)
	r.release(relOpts{})
	s := r.script(1, "2.0.0")
	// A PATH with the usual tools and the fake curl, but no ssh-keygen (and no openssl either).
	only := filepath.Join(r.dir, "tools")
	_ = os.Mkdir(only, 0o755)
	for _, tool := range []string{"sh", "uname", "id", "mktemp", "rm", "cat", "sed", "awk", "tr", "grep", "sha256sum", "wc", "head", "cut", "chmod", "mkdir", "sort", "dirname", "basename", "cp", "printf", "date", "ls", "dd"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(only, tool))
		}
	}
	res := r.runPath(s, r.bin+":"+only)
	r.refused(res, "ssh-keygen")
	// Not a tool that merely exists: an ssh-keygen that cannot verify (too old) is a refusal too.
	old := filepath.Join(r.dir, "oldssh")
	_ = os.Mkdir(old, 0o755)
	_ = os.WriteFile(filepath.Join(old, "ssh-keygen"), []byte("#!/bin/sh\necho 'unknown option -- Y' >&2\nexit 1\n"), 0o755)
	r.refused(r.runPath(s, old+":"+r.bin+":"+only), "")
}

func TestScriptTemplateIsNotRunnable(t *testing.T) {
	r := newScriptRig(t)
	r.release(relOpts{})
	tpl, _ := filepath.Abs("install.sh")
	r.refused(r.run(tpl), "template")
}

func TestScriptKeyEpochAndRotation(t *testing.T) {
	r := newScriptRig(t)
	// A script with the new keys (epoch 2) before the first release under them: it exits.
	r.release(relOpts{epoch: 1})
	r.refused(r.run(r.script(2, "2.0.0")), "epoch")

	// A script of the old epoch, a release of the old epoch, and a rotation record signed by the old
	// keys: the script stops and sends the operator to a new install.sh and its fingerprint.
	next := []keys.AKey{sk(t, 50).Public(), sk(t, 51).Public(), sk(t, 52).Public()}
	rot, err := installkit.Rotation(next, 2)
	if err != nil {
		t.Fatal(err)
	}
	sigs, err := installkit.SignRecord([]byte(rot), installkit.NSRotation, r.signers[0], r.signers[1])
	if err != nil {
		t.Fatal(err)
	}
	r.release(relOpts{epoch: 1})
	r.write("rotation.txt", rot)
	r.write("rotation.txt.sigs", string(sigs))
	fp, _ := installkit.Fingerprint(next, 2)
	res := r.run(r.script(1, "2.0.0"))
	r.refused(res, "install.sh")
	if !strings.Contains(res.out, fp) {
		t.Errorf("the refusal does not show the fingerprint of the new key set:\n%s", res.out)
	}

	// A rotation record signed by anybody else is ignored: an attacker cannot stop an install with it
	// (and cannot start one either).
	bad, _ := installkit.SignRecord([]byte(rot), installkit.NSRotation, sk(t, 60), sk(t, 61))
	r.write("rotation.txt.sigs", string(bad))
	if res := r.run(r.script(1, "2.0.0")); res.code != 0 || !r.executed() {
		t.Fatalf("a forged rotation record blocked the install:\n%s", res.out)
	}
}

// The fingerprint of the key block is the same three ways: the script's own flag, an independent
// command of the guide, and the release tool.
func TestScriptFingerprint(t *testing.T) {
	r := newScriptRig(t)
	s := r.script(1, "2.0.0")
	res := r.run(s, "--fingerprint")
	got := strings.TrimSpace(res.out)
	if res.code != 0 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("--fingerprint: exit %d: %q", res.code, res.out)
	}
	body, _ := os.ReadFile(s)
	i, j := bytes.Index(body, []byte("# BEGIN RELEASE KEYS\n")), bytes.Index(body, []byte("# END RELEASE KEYS\n"))
	sum := sha256.Sum256(body[i : j+len("# END RELEASE KEYS\n")])
	if got != hex.EncodeToString(sum[:]) {
		t.Errorf("the script prints %s, the block hashes to %x", got, sum)
	}
	var pubs []keys.AKey
	for _, sg := range r.signers {
		pubs = append(pubs, sg.Public())
	}
	if want, _ := installkit.Fingerprint(pubs, 1); want != got {
		t.Errorf("installkit.Fingerprint %s, script %s", want, got)
	}
	// The command that the guide teaches gives the same answer without running the script.
	if out, err := exec.Command("sh", "-c", `sed -n '/^# BEGIN RELEASE KEYS$/,/^# END RELEASE KEYS$/p' "$1" | sha256sum | cut -d' ' -f1`, "sh", s).Output(); err != nil || strings.TrimSpace(string(out)) != got {
		t.Errorf("the guide's command: %q %v", out, err)
	}
	// It needs no root, no curl and no network: nothing is downloaded.
	if _, err := os.Stat(filepath.Join(r.dir, "urls")); err == nil {
		t.Error("--fingerprint downloaded something")
	}
}
