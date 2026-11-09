// Command efv2-release is the offline tool of the project maintainer for release manifests
// (objects.md §9): keygen, assemble, cosign, sign, verify. Release keys are made and used on
// offline media; the tool refuses to write a private key inside a git work tree.
//
//	efv2-release keygen   --out BASE                       BASE.key (private seed, hex), BASE.pub
//	efv2-release assemble --version V --released-at UNIX [--notes-url URL]
//	                      [--policy comp=min,rec[,deprecated_after]]...
//	                      [--artifact platform,name,sha256hex,size,url]...
//	                      [--next-key KEY]... [--revoke-key KEY]... [--min-version X.Y.Z] --out manifest.bin
//	efv2-release cosign   --manifest manifest.bin --key K2.key --out K2.cosig
//	efv2-release sign     --manifest manifest.bin --key K1.key [--cosig K2.cosig]...
//	                      --not-after UNIX --out release.bin
//	efv2-release verify   --keys PUB1,PUB2,PUB3 [--now UNIX] release.bin
//
// For the install script (T12.4e; see install_cmds.go): install-stamp, install-fingerprint,
// install-record, install-rotation.
//
// A manifest is valid with the envelope signature of one release key and the co-signature of
// another: the cosigner signs the manifest first, then the signer seals the object around it.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "efv2-release:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("a command is required: keygen, assemble, cosign, sign, verify, bootstrap-sign, bootstrap-verify, install-stamp, install-fingerprint, install-record or install-rotation")
	}
	switch args[0] {
	case "keygen":
		return cmdKeygen(args[1:], stdout)
	case "assemble":
		return cmdAssemble(args[1:], stdout)
	case "cosign":
		return cmdCosign(args[1:], stdout)
	case "sign":
		return cmdSign(args[1:], stdout)
	case "verify":
		return cmdVerify(args[1:], stdout)
	case "bootstrap-sign":
		return cmdBootstrapSign(args[1:], stdout)
	case "bootstrap-verify":
		return cmdBootstrapVerify(args[1:], stdout)
	case "install-stamp":
		return cmdInstallStamp(args[1:], stdout)
	case "install-fingerprint":
		return cmdInstallFingerprint(args[1:], stdout)
	case "install-record":
		return cmdInstallRecord(args[1:], stdout)
	case "install-rotation":
		return cmdInstallRotation(args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ";") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func need(fs *flag.FlagSet, names ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, n := range names {
		if !set[n] {
			return fmt.Errorf("--%s is required", n)
		}
	}
	return nil
}

// --- keygen ---

func cmdKeygen(args []string, stdout io.Writer) error {
	fs := newFlags("keygen")
	out := fs.String("out", "", "base path: writes BASE.key (private) and BASE.pub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "out"); err != nil {
		return err
	}
	abs, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if dir := inGitWorkTree(filepath.Dir(abs)); dir != "" {
		return fmt.Errorf("refusing to write a private key inside the git work tree %s: use offline media", dir)
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	signer, err := keys.NewEd25519Signer(seed)
	if err != nil {
		return err
	}
	pub := signer.Public().Text()
	priv, err := os.OpenFile(abs+".key", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := priv.WriteString(hex.EncodeToString(seed) + "\n")
	if cerr := priv.Close(); werr == nil {
		werr = cerr
	}
	for i := range seed {
		seed[i] = 0
	}
	if werr != nil {
		_ = os.Remove(abs + ".key")
		return werr
	}
	if err := os.WriteFile(abs+".pub", []byte(pub+"\n"), 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, pub)
	return err
}

// inGitWorkTree returns the work tree root that contains dir (the nearest ancestor with a .git
// entry), or "".
func inGitWorkTree(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func loadSigner(path string) (*keys.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, errors.New("key file is not a hex seed")
	}
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	return keys.NewEd25519Signer(seed)
}

// --- assemble ---

func cmdAssemble(args []string, stdout io.Writer) error {
	fs := newFlags("assemble")
	version := fs.String("version", "", "release version, e.g. 0.3.1")
	releasedAt := fs.Uint64("released-at", 0, "release time, unix seconds")
	notes := fs.String("notes-url", "", "https URL of the release notes")
	minVersion := fs.String("min-version", "", "oldest version that may be installed or kept, X.Y.Z (default: no bound)")
	out := fs.String("out", "", "manifest output file")
	var policies, artifacts, nextKeys, revoked multi
	fs.Var(&policies, "policy", "component=min_supported,recommended[,deprecated_after]")
	fs.Var(&artifacts, "artifact", "platform,name,sha256hex,size,url")
	fs.Var(&nextKeys, "next-key", "public key to add to the release key set")
	fs.Var(&revoked, "revoke-key", "public key to remove from the release key set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "version", "released-at", "out"); err != nil {
		return err
	}
	if *version == "" {
		return errors.New("--version is empty")
	}
	rel := &pb.Release{Version: *version, ReleasedAt: *releasedAt}
	if *minVersion != "" {
		mv, ok := objects.ParseSemver(*minVersion)
		if !ok {
			return fmt.Errorf("--min-version %q is not X.Y.Z", *minVersion)
		}
		if v, ok := objects.ParseSemver(*version); ok && objects.CompareSemver(mv, v) > 0 {
			return errors.New("--min-version is above --version")
		}
		rel.MinVersion = *minVersion
	}
	if *notes != "" {
		if err := httpsURL(*notes); err != nil {
			return fmt.Errorf("--notes-url: %w", err)
		}
		rel.NotesUrl = *notes
	}
	for _, p := range policies {
		pol, err := parsePolicy(p)
		if err != nil {
			return fmt.Errorf("--policy %q: %w", p, err)
		}
		rel.Policies = append(rel.Policies, pol)
	}
	for _, a := range artifacts {
		art, err := parseArtifact(a)
		if err != nil {
			return fmt.Errorf("--artifact %q: %w", a, err)
		}
		rel.Artifacts = append(rel.Artifacts, art)
	}
	for _, k := range nextKeys {
		key, err := keys.ParseText(k)
		if err != nil {
			return fmt.Errorf("--next-key: %w", err)
		}
		rel.NextKeys = append(rel.NextKeys, key)
	}
	if len(rel.NextKeys) > 3 {
		return errors.New("--next-key: at most 3 keys per manifest")
	}
	for _, k := range revoked {
		key, err := keys.ParseText(k)
		if err != nil {
			return fmt.Errorf("--revoke-key: %w", err)
		}
		rel.RevokedKeys = append(rel.RevokedKeys, key)
	}
	raw, err := objects.Marshal(rel)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "manifest %s (%d bytes)\n", *version, len(raw))
	return err
}

func httpsURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("https URL required")
	}
	return nil
}

func parsePolicy(s string) (*pb.Policy, error) {
	comp, nums, ok := strings.Cut(s, "=")
	if !ok || comp == "" {
		return nil, errors.New("want component=min,recommended[,deprecated_after]")
	}
	parts := strings.Split(nums, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, errors.New("want component=min,recommended[,deprecated_after]")
	}
	min, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return nil, errors.New("min_supported is not a number")
	}
	rec, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return nil, errors.New("recommended is not a number")
	}
	p := &pb.Policy{Component: comp, MinSupported: uint32(min), Recommended: uint32(rec)}
	if len(parts) == 3 {
		if p.DeprecatedAfter, err = strconv.ParseUint(parts[2], 10, 64); err != nil {
			return nil, errors.New("deprecated_after is not a number")
		}
	}
	return p, nil
}

func parseArtifact(s string) (*pb.Artifact, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 5 {
		return nil, errors.New("want platform,name,sha256hex,size,url")
	}
	sum, err := hex.DecodeString(parts[2])
	if err != nil || len(sum) != 32 {
		return nil, errors.New("sha256 must be 64 hex digits")
	}
	size, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return nil, errors.New("size is not a number")
	}
	if err := httpsURL(parts[4]); err != nil {
		return nil, err
	}
	if parts[0] == "" || parts[1] == "" {
		return nil, errors.New("platform and name are required")
	}
	return &pb.Artifact{Platform: parts[0], Name: parts[1], Sha256: sum, Size: size, Url: parts[4]}, nil
}

// --- cosign / sign ---

func readManifest(path string) ([]byte, *pb.Release, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	rel := &pb.Release{}
	if err := proto.Unmarshal(raw, rel); err != nil {
		return nil, nil, fmt.Errorf("%s is not a manifest: %w", path, err)
	}
	return raw, rel, nil
}

func cmdCosign(args []string, stdout io.Writer) error {
	fs := newFlags("cosign")
	manifest := fs.String("manifest", "", "manifest file (from assemble)")
	key := fs.String("key", "", "private key file of the cosigner")
	out := fs.String("out", "", "co-signature output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "manifest", "key", "out"); err != nil {
		return err
	}
	raw, _, err := readManifest(*manifest)
	if err != nil {
		return err
	}
	s, err := loadSigner(*key)
	if err != nil {
		return err
	}
	cs, err := objects.Marshal(objects.Cosign(s, raw))
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, cs, 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "cosigned by %s\n", s.Public().Text())
	return err
}

func cmdSign(args []string, stdout io.Writer) error {
	fs := newFlags("sign")
	manifest := fs.String("manifest", "", "manifest file (from assemble)")
	key := fs.String("key", "", "private key file of the envelope signer")
	notAfter := fs.Uint64("not-after", 0, "expiry of the object, unix seconds")
	out := fs.String("out", "", "release object output file")
	var cosigs multi
	fs.Var(&cosigs, "cosig", "co-signature file (from cosign)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "manifest", "key", "not-after", "out"); err != nil {
		return err
	}
	raw, rel, err := readManifest(*manifest)
	if err != nil {
		return err
	}
	s, err := loadSigner(*key)
	if err != nil {
		return err
	}
	obj := &pb.ReleaseObject{Manifest: raw}
	for _, f := range cosigs {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		cs := &pb.CoSig{}
		if err := proto.Unmarshal(b, cs); err != nil {
			return fmt.Errorf("%s is not a co-signature: %w", f, err)
		}
		obj.Cosigs = append(obj.Cosigs, cs)
	}
	body, err := objects.Marshal(obj)
	if err != nil {
		return err
	}
	sealed, err := objects.Seal(objects.LabelRelease, s, rel.GetReleasedAt(), *notAfter, body)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, sealed, 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "sealed %s by %s with %d co-signature(s)\n", rel.GetVersion(), s.Public().Text(), len(obj.Cosigs))
	return err
}

// --- verify ---

func cmdVerify(args []string, stdout io.Writer) error {
	fs := newFlags("verify")
	keyList := fs.String("keys", "", "comma-separated release public keys")
	nowUnix := fs.Int64("now", 0, "verification time, unix seconds (default: now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, "keys"); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: efv2-release verify --keys PUB1,PUB2,PUB3 release.bin")
	}
	var set []keys.AKey
	for _, k := range strings.Split(*keyList, ",") {
		key, err := keys.ParseText(strings.TrimSpace(k))
		if err != nil {
			return fmt.Errorf("--keys: %w", err)
		}
		set = append(set, key)
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	st, err := objects.NewReleaseState(set)
	if err != nil {
		return err
	}
	now := time.Now()
	if *nowUnix != 0 {
		now = time.Unix(*nowUnix, 0)
	}
	m, err := st.Check(raw, now)
	if err != nil {
		return fmt.Errorf("NOT VALID: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "valid: version %s released_at %d, %d distinct signers, key set after: %d\n",
		m.Release.GetVersion(), m.Release.GetReleasedAt(), len(m.Signers), len(m.Keys))
	return err
}
