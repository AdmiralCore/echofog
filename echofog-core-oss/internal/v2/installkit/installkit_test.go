package installkit_test

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
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func signer(t testing.TB, n byte) *keys.Signer {
	t.Helper()
	seed := sha256.Sum256([]byte{'i', 'k', n})
	s, err := keys.NewEd25519Signer(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pubs(ss ...*keys.Signer) []keys.AKey {
	var out []keys.AKey
	for _, s := range ss {
		out = append(out, s.Public())
	}
	return out
}

// sshVerify runs the base-system tool, the one install.sh uses.
func sshVerify(t *testing.T, key keys.AKey, ns string, msg, sig []byte) bool {
	t.Helper()
	tool, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	line, err := installkit.PublicLine(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed")
	sigf := filepath.Join(dir, "sig")
	_ = os.WriteFile(allowed, []byte("echofog-release namespaces=\""+ns+"\" "+line+"\n"), 0o600)
	_ = os.WriteFile(sigf, sig, 0o600)
	cmd := exec.Command(tool, "-Y", "verify", "-f", allowed, "-I", "echofog-release", "-n", ns, "-s", sigf)
	cmd.Stdin = strings.NewReader(string(msg))
	return cmd.Run() == nil
}

func TestSSHSigVerifiesWithTheBaseTool(t *testing.T) {
	s := signer(t, 1)
	msg := []byte("echofog-release 1\nversion 2.0.1\n")
	sig, err := installkit.SignSSH(s, installkit.NSRelease, msg)
	if err != nil {
		t.Fatalf("SignSSH: %v", err)
	}
	if !strings.HasPrefix(string(sig), "-----BEGIN SSH SIGNATURE-----\n") || !strings.HasSuffix(strings.TrimSpace(string(sig)), "-----END SSH SIGNATURE-----") {
		t.Fatalf("not an armored signature: %q", sig)
	}
	if !sshVerify(t, s.Public(), installkit.NSRelease, msg, sig) {
		t.Fatal("ssh-keygen -Y verify refused a good signature")
	}
	if sshVerify(t, s.Public(), installkit.NSRelease, append(msg, 'x'), sig) {
		t.Error("a changed message verified")
	}
	if sshVerify(t, signer(t, 2).Public(), installkit.NSRelease, msg, sig) {
		t.Error("a foreign key verified")
	}
	if sshVerify(t, s.Public(), installkit.NSRotation, msg, sig) {
		t.Error("a release signature verified as a rotation signature: the namespaces must separate them")
	}
}

func TestKeyBlockAndFingerprint(t *testing.T) {
	a, b, c := signer(t, 1), signer(t, 2), signer(t, 3)
	blk1, err := installkit.KeyBlock(pubs(a, b, c), 1)
	if err != nil {
		t.Fatal(err)
	}
	blk2, _ := installkit.KeyBlock(pubs(c, a, b), 1)
	if blk1 != blk2 {
		t.Error("the block depends on the order of the keys")
	}
	if strings.Count(blk1, "ssh-ed25519 ") != 3 || !strings.HasPrefix(blk1, "# BEGIN RELEASE KEYS\n") || !strings.HasSuffix(blk1, "# END RELEASE KEYS\n") {
		t.Fatalf("block:\n%s", blk1)
	}
	fp, err := installkit.Fingerprint(pubs(a, b, c), 1)
	sum := sha256.Sum256([]byte(blk1))
	if err != nil || fp != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %q, want sha256 of the block: %v", fp, err)
	}
	if fp2, _ := installkit.Fingerprint(pubs(a, b, c), 2); fp2 == fp {
		t.Error("the epoch is not in the fingerprint")
	}
	if fp3, _ := installkit.Fingerprint(pubs(a, b, signer(t, 4)), 1); fp3 == fp {
		t.Error("the key set is not in the fingerprint")
	}
}

const template = "#!/bin/sh\necho head\n# BEGIN RELEASE KEYS\nPLACEHOLDER\n# END RELEASE KEYS\nMIN_VERSION_FLOOR=0.0.0\necho tail\n"

func TestStamp(t *testing.T) {
	ks := pubs(signer(t, 1), signer(t, 2), signer(t, 3))
	out, err := installkit.Stamp(template, ks, 1, "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := installkit.KeyBlock(ks, 1)
	if !strings.Contains(out, blk) || strings.Contains(out, "PLACEHOLDER") || !strings.Contains(out, "MIN_VERSION_FLOOR=2.0.0\n") {
		t.Fatalf("stamped script:\n%s", out)
	}
	if !strings.HasPrefix(out, "#!/bin/sh\necho head\n") || !strings.HasSuffix(out, "echo tail\n") {
		t.Error("the text around the block changed")
	}
	// A stamped script is not a template any more: a second stamp is refused (no silent key swap).
	if _, err := installkit.Stamp(out, ks, 1, "2.0.0"); err == nil {
		t.Error("a stamped script was stamped again")
	}
	for name, bad := range map[string]string{"no markers": "#!/bin/sh\n", "no floor": "# BEGIN RELEASE KEYS\nPLACEHOLDER\n# END RELEASE KEYS\n",
		"two blocks": template + template} {
		if _, err := installkit.Stamp(bad, ks, 1, "2.0.0"); err == nil {
			t.Errorf("%s: stamped", name)
		}
	}
	if _, err := installkit.Stamp(template, ks, 1, "2.0"); err == nil {
		t.Error("a bad floor was accepted")
	}
	if _, err := installkit.Stamp(template, ks[:1], 1, "2.0.0"); err == nil {
		t.Error("a key set below the threshold was stamped")
	}
	if _, err := installkit.Stamp(template, ks, 0, "2.0.0"); err == nil {
		t.Error("epoch 0 was accepted")
	}
}

func TestRecord(t *testing.T) {
	sum := sha256.Sum256([]byte("bin"))
	rel := &pb.Release{Version: "2.0.1", MinVersion: "2.0.0", ReleasedAt: 1_800_000_000, Artifacts: []*pb.Artifact{
		{Platform: "linux-amd64", Name: "echofog-node-linux-amd64", Sha256: sum[:], Size: 3, Url: "https://example.org/a"},
		{Platform: "linux-arm64", Name: "echofog-node-linux-arm64", Sha256: sum[:], Size: 3, Url: "https://example.org/b"},
	}}
	got, err := installkit.Record(rel, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := "echofog-release 1\nversion 2.0.1\nmin_version 2.0.0\nkey_epoch 1\nreleased_at 1800000000\n" +
		"artifact linux-amd64 " + hex.EncodeToString(sum[:]) + " 3\nartifact linux-arm64 " + hex.EncodeToString(sum[:]) + " 3\n"
	if got != want {
		t.Fatalf("record:\n%s\nwant:\n%s", got, want)
	}
	// No min_version in the manifest: the line says 0.0.0 (no bound), never absent.
	rel.MinVersion = ""
	got, _ = installkit.Record(rel, 1)
	if !strings.Contains(got, "min_version 0.0.0\n") {
		t.Errorf("record without a bound:\n%s", got)
	}
	// Only well-formed values reach a file that the shell parses.
	for name, mut := range map[string]func(){
		"bad version":  func() { rel.Version = "2.0.1\nartifact x" },
		"bad min":      func() { rel.Version = "2.0.1"; rel.MinVersion = "x" },
		"bad platform": func() { rel.MinVersion = ""; rel.Artifacts[0].Platform = "linux amd64" },
		"bad hash":     func() { rel.Artifacts[0].Platform = "linux-amd64"; rel.Artifacts[0].Sha256 = []byte{1} },
	} {
		mut()
		if _, err := installkit.Record(rel, 1); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestSignRecordAndRotation(t *testing.T) {
	a, b := signer(t, 1), signer(t, 2)
	data := []byte("echofog-release 1\n")
	sigs, err := installkit.SignRecord(data, installkit.NSRelease, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(sigs), "-----BEGIN SSH SIGNATURE-----") != 2 {
		t.Fatalf("want two blocks:\n%s", sigs)
	}
	next := pubs(signer(t, 5), signer(t, 6), signer(t, 7))
	rot, err := installkit.Rotation(next, 2)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := installkit.Fingerprint(next, 2)
	if rot != "echofog-rotation 1\nnew_epoch 2\nnew_keys_fingerprint "+fp+"\n" {
		t.Fatalf("rotation:\n%s", rot)
	}
	if _, err := installkit.Rotation(next, 0); err == nil {
		t.Error("epoch 0 rotation")
	}
}
