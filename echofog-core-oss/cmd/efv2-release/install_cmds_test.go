package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/installkit"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

type installRig struct {
	dir   string
	privs []string
	pubs  []string
}

func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	r := &installRig{dir: t.TempDir()}
	for _, n := range []string{"k1", "k2", "k3"} {
		priv, pub := keygen(t, r.dir, n)
		r.privs = append(r.privs, priv)
		r.pubs = append(r.pubs, pub)
	}
	return r
}

func (r *installRig) keyList() string { return strings.Join(r.pubs, ",") }

func (r *installRig) manifest(t *testing.T, extra ...string) string {
	t.Helper()
	sum := sha256.Sum256([]byte("bin"))
	path := filepath.Join(r.dir, "manifest.bin")
	args := []string{"assemble", "--version", "2.0.1", "--released-at", "1800000000", "--out", path,
		"--artifact", "linux-amd64,echofog-node-linux-amd64," + hex.EncodeToString(sum[:]) + ",3,https://example.org/a"}
	mustDo(t, append(args, extra...)...)
	return path
}

func TestAssembleMinVersion(t *testing.T) {
	r := newInstallRig(t)
	path := r.manifest(t, "--min-version", "2.0.0")
	_, rel, err := readManifest(path)
	if err != nil || rel.GetMinVersion() != "2.0.0" {
		t.Fatalf("manifest: %v %v", rel, err)
	}
	for _, bad := range []string{"x", "2.0", "2.0.2", "03.0.0", "2.0.0\n"} {
		if _, err := do(t, "assemble", "--version", "2.0.1", "--released-at", "1800000000", "--min-version", bad, "--out", filepath.Join(r.dir, "bad.bin")); err == nil {
			t.Errorf("--min-version %q accepted", bad)
		}
	}
}

func TestInstallRecord(t *testing.T) {
	r := newInstallRig(t)
	manifest := r.manifest(t, "--min-version", "2.0.0")
	out := filepath.Join(r.dir, "pub")
	_ = os.Mkdir(out, 0o755)
	mustDo(t, "install-record", "--manifest", manifest, "--key", r.privs[0], "--key", r.privs[1], "--key-epoch", "1", "--out-dir", out)
	rec, err := os.ReadFile(filepath.Join(out, "release.txt"))
	if err != nil || !strings.HasPrefix(string(rec), "echofog-release 1\nversion 2.0.1\nmin_version 2.0.0\nkey_epoch 1\n") {
		t.Fatalf("release.txt: %q %v", rec, err)
	}
	sigs, err := os.ReadFile(filepath.Join(out, "release.txt.sigs"))
	if err != nil || strings.Count(string(sigs), "BEGIN SSH SIGNATURE") != 2 {
		t.Fatalf("release.txt.sigs: %q %v", sigs, err)
	}
	// A single signer is not a release: the tool will not write a record that no script accepts.
	if _, err := do(t, "install-record", "--manifest", manifest, "--key", r.privs[0], "--key-epoch", "1", "--out-dir", out+"2"); err == nil {
		t.Error("a record with one signature was written")
	}
	// The same key twice is one signer.
	if _, err := do(t, "install-record", "--manifest", manifest, "--key", r.privs[0], "--key", r.privs[0], "--key-epoch", "1", "--out-dir", out+"3"); err == nil {
		t.Error("a record signed twice by one key was written")
	}
	if tool, err := exec.LookPath("ssh-keygen"); err == nil {
		k, _ := keys.ParseText(r.pubs[0])
		line, _ := installkit.PublicLine(k)
		allowed := filepath.Join(r.dir, "allowed")
		_ = os.WriteFile(allowed, []byte("echofog-release namespaces=\""+installkit.NSRelease+"\" "+line+"\n"), 0o600)
		first := strings.SplitAfter(string(sigs), "-----END SSH SIGNATURE-----\n")[0]
		sigf := filepath.Join(r.dir, "first.sig")
		_ = os.WriteFile(sigf, []byte(first), 0o600)
		cmd := exec.Command(tool, "-Y", "verify", "-f", allowed, "-I", "echofog-release", "-n", installkit.NSRelease, "-s", sigf)
		cmd.Stdin = strings.NewReader(string(rec))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("ssh-keygen -Y verify of the first block: %v %s", err, b)
		}
	}
}

func TestInstallStampAndFingerprint(t *testing.T) {
	r := newInstallRig(t)
	tpl := filepath.Join(r.dir, "install.sh")
	if err := os.WriteFile(tpl, []byte("#!/bin/sh\n# BEGIN RELEASE KEYS\nPLACEHOLDER\n# END RELEASE KEYS\nMIN_VERSION_FLOOR=0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(r.dir, "stamped.sh")
	got := mustDo(t, "install-stamp", "--template", tpl, "--keys", r.keyList(), "--key-epoch", "1", "--floor", "2.0.0", "--out", out)
	fp := mustDo(t, "install-fingerprint", "--keys", r.keyList(), "--key-epoch", "1")
	if strings.TrimSpace(fp) == "" || !strings.Contains(got, strings.TrimSpace(fp)) {
		t.Fatalf("stamp prints %q, fingerprint %q", got, fp)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "ssh-ed25519 ") || strings.Contains(string(b), "PLACEHOLDER") {
		t.Fatalf("stamped script:\n%s", b)
	}
	// The same keys in another order give the same fingerprint; a changed epoch does not.
	rev := r.pubs[2] + "," + r.pubs[0] + "," + r.pubs[1]
	if mustDo(t, "install-fingerprint", "--keys", rev, "--key-epoch", "1") != fp || mustDo(t, "install-fingerprint", "--keys", r.keyList(), "--key-epoch", "2") == fp {
		t.Error("fingerprint is not order-free or not epoch-bound")
	}
	if _, err := do(t, "install-stamp", "--template", out, "--keys", r.keyList(), "--key-epoch", "1", "--floor", "2.0.0", "--out", out+"2"); err == nil {
		t.Error("a stamped script was stamped again")
	}
}

func TestInstallRotation(t *testing.T) {
	old := newInstallRig(t)
	next := newInstallRig(t)
	out := filepath.Join(old.dir, "rot")
	_ = os.Mkdir(out, 0o755)
	mustDo(t, "install-rotation", "--new-keys", next.keyList(), "--new-epoch", "2", "--key", old.privs[0], "--key", old.privs[1], "--out-dir", out)
	rec, err := os.ReadFile(filepath.Join(out, "rotation.txt"))
	if err != nil || !strings.HasPrefix(string(rec), "echofog-rotation 1\nnew_epoch 2\nnew_keys_fingerprint ") {
		t.Fatalf("rotation.txt: %q %v", rec, err)
	}
	fp := strings.TrimSpace(mustDo(t, "install-fingerprint", "--keys", next.keyList(), "--key-epoch", "2"))
	if !strings.Contains(string(rec), fp) {
		t.Error("the rotation record does not name the fingerprint of the new key block")
	}
	sigs, _ := os.ReadFile(filepath.Join(out, "rotation.txt.sigs"))
	if strings.Count(string(sigs), "BEGIN SSH SIGNATURE") != 2 {
		t.Errorf("rotation.txt.sigs:\n%s", sigs)
	}
	// The epoch must go up: epoch 1 -> "1" is not a rotation.
	if _, err := do(t, "install-rotation", "--new-keys", next.keyList(), "--new-epoch", "0", "--key", old.privs[0], "--key", old.privs[1], "--out-dir", out+"x"); err == nil {
		t.Error("epoch 0 rotation written")
	}
}

// M1: install-stamp prints the SHA-256 of the whole stamped file (what users compare) and the key
// block value; install-fingerprint --script prints the whole-file value of a script.
func TestInstallStampPrintsTheFileFingerprint(t *testing.T) {
	r := newInstallRig(t)
	tpl := filepath.Join(r.dir, "install.sh")
	if err := os.WriteFile(tpl, []byte("#!/bin/sh\n# BEGIN RELEASE KEYS\nPLACEHOLDER\n# END RELEASE KEYS\nMIN_VERSION_FLOOR=0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(r.dir, "stamped.sh")
	got := mustDo(t, "install-stamp", "--template", tpl, "--keys", r.keyList(), "--key-epoch", "1", "--floor", "2.0.0", "--out", out)
	raw, _ := os.ReadFile(out)
	sum := sha256.Sum256(raw)
	file := hex.EncodeToString(sum[:])
	keyFP := strings.TrimSpace(mustDo(t, "install-fingerprint", "--keys", r.keyList(), "--key-epoch", "1"))
	if !strings.Contains(got, "file "+file) || !strings.Contains(got, "keys "+keyFP) {
		t.Fatalf("install-stamp output:\n%s\nwant file %s and keys %s", got, file, keyFP)
	}
	if v := strings.TrimSpace(mustDo(t, "install-fingerprint", "--script", out)); v != file {
		t.Errorf("install-fingerprint --script %s, want %s", v, file)
	}
	if _, err := do(t, "install-fingerprint", "--script", out, "--keys", r.keyList(), "--key-epoch", "1"); err == nil {
		t.Error("--script together with --keys is ambiguous")
	}
}
