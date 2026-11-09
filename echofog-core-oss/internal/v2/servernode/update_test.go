package servernode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/release"
	"github.com/echofog/echofog-core/internal/v2/releasekeys"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

var updNow = time.Unix(1_800_000_000, 0)

type updClock struct{}

func (updClock) Now() time.Time { return updNow }

func relSigner(t testing.TB, n byte) *keys.Signer {
	t.Helper()
	seed := sha256.Sum256([]byte{'u', 'k', n})
	s, err := keys.NewEd25519Signer(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type updRig struct {
	t        *testing.T
	srv      *httptest.Server
	signers  []*keys.Signer
	artifact []byte
	served   atomic.Int64 // artifact downloads
	manifest atomic.Pointer[[]byte]
	exe      string
	deps     servernode.UpdateDeps
	mgr      *release.Manager
}

func newUpdRig(t *testing.T) *updRig {
	t.Helper()
	r := &updRig{t: t, artifact: []byte("#!new-binary v0.4.0\n"), signers: []*keys.Signer{relSigner(t, 1), relSigner(t, 2), relSigner(t, 3)}}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(*r.manifest.Load()) })
	mux.HandleFunc("/echofog-node", func(w http.ResponseWriter, _ *http.Request) {
		r.served.Add(1)
		_, _ = w.Write(r.artifact)
	})
	r.srv = httptest.NewTLSServer(mux)
	t.Cleanup(r.srv.Close)

	pubs := []keys.AKey{r.signers[0].Public(), r.signers[1].Public(), r.signers[2].Public()}
	mgr, err := release.Open(release.Config{Keys: pubs, Clock: updClock{}})
	if err != nil {
		t.Fatal(err)
	}
	r.mgr = mgr
	r.publish("0.4.0", r.signers[0], r.signers[1])

	r.exe = filepath.Join(t.TempDir(), "echofog-node")
	if err := os.WriteFile(r.exe, []byte("old binary v0.3.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.deps = servernode.UpdateDeps{
		Manager: mgr, HTTP: r.srv.Client(), ManifestURL: r.srv.URL + "/manifest", CurrentVersion: "0.3.0",
		Platform: "linux-amd64", ExePath: r.exe,
		Confirm:  func(servernode.UpdateInfo) bool { return true },
		SelfTest: func(context.Context, string) (string, error) { return "0.4.0", nil },
	}
	return r
}

// publish signs a manifest for the version with the given signers (the first signs, the rest co-sign).
func (r *updRig) publish(version string, signers ...*keys.Signer) {
	r.t.Helper()
	sum := sha256.Sum256(r.artifact)
	rel := &pb.Release{Version: version, ReleasedAt: uint64(updNow.Add(-time.Hour).Unix()), Artifacts: []*pb.Artifact{
		{Platform: "linux-amd64", Name: "echofog-node", Sha256: sum[:], Size: uint64(len(r.artifact)), Url: r.srv.URL + "/echofog-node"},
		{Platform: runtime.GOOS + "-" + runtime.GOARCH, Name: "echofog-node", Sha256: sum[:], Size: uint64(len(r.artifact)), Url: r.srv.URL + "/echofog-node"},
	}}
	raw, err := objects.SealRelease(signers[0], rel.ReleasedAt+399*86400, rel, signers[1:]...)
	if err != nil {
		r.t.Fatal(err)
	}
	r.manifest.Store(&raw)
}

func (r *updRig) exeContent() string {
	b, err := os.ReadFile(r.exe)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func mustInfo(t *testing.T, r *updRig) *servernode.UpdateInfo {
	t.Helper()
	info, err := servernode.CheckUpdate(context.Background(), r.deps)
	if err != nil || info == nil {
		t.Fatalf("CheckUpdate = %v, %v", info, err)
	}
	return info
}

func TestCheckUpdateFindsANewerVersion(t *testing.T) {
	r := newUpdRig(t)
	info, err := servernode.CheckUpdate(context.Background(), r.deps)
	if err != nil || info == nil {
		t.Fatalf("CheckUpdate = %v, %v", info, err)
	}
	if info.Version != "0.4.0" || info.Artifact != "echofog-node" || info.Size != uint64(len(r.artifact)) || !strings.HasPrefix(info.URL, "https://") {
		t.Fatalf("info %+v", info)
	}
	if r.served.Load() != 0 {
		t.Fatal("checking downloaded the artifact")
	}
	// Nothing newer: nothing offered.
	r.deps.CurrentVersion = "0.4.0"
	if info, err := servernode.CheckUpdate(context.Background(), r.deps); err != nil || info != nil {
		t.Fatalf("same version: %v, %v", info, err)
	}
}

// A key list parsed the way a release build receives it must reach CheckUpdate: the update is
// refused as "no release keys" only when the build has none. This is the fail-closed half of
// T12.4e-fix: without a build-time key list setup/update must refuse everything.
func TestBuildKeysGateUpdate(t *testing.T) {
	r := newUpdRig(t)
	ctx := context.Background()
	spec := strings.Join([]string{
		base64.StdEncoding.EncodeToString(r.signers[0].Public().Key()),
		base64.RawURLEncoding.EncodeToString(r.signers[1].Public().Key()),
		strings.ToUpper(hex.EncodeToString(r.signers[2].Public().Key())),
	}, " , ")
	ks, err := releasekeys.ParseSpec(spec)
	if err != nil || len(ks) != 3 {
		t.Fatalf("ParseSpec: %d keys, %v", len(ks), err)
	}
	mgr, err := release.Open(release.Config{Keys: ks, Clock: updClock{}})
	if err != nil {
		t.Fatalf("release.Open with the parsed build keys: %v", err)
	}
	deps := r.deps
	deps.Manager = mgr
	if _, err := servernode.CheckUpdate(ctx, deps); err != nil {
		t.Fatalf("CheckUpdate with the build keys: %v", err)
	}

	// No keys (an OSS build): the update refuses before it fetches anything.
	deps.Manager = nil
	if _, err := servernode.CheckUpdate(ctx, deps); !errors.Is(err, servernode.ErrNoReleaseKeys) {
		t.Fatalf("CheckUpdate without build keys = %v, want ErrNoReleaseKeys", err)
	}
}

func TestUpdateRefusesBadSources(t *testing.T) {
	r := newUpdRig(t)
	ctx := context.Background()

	// One signature of three is not a manifest.
	one := newUpdRig(t)
	one.mgr, _ = release.Open(release.Config{Keys: []keys.AKey{one.signers[0].Public(), one.signers[1].Public(), one.signers[2].Public()}, Clock: updClock{}})
	one.deps.Manager = one.mgr
	one.publish("0.5.0", one.signers[0])
	if info, err := servernode.CheckUpdate(ctx, one.deps); err == nil || info != nil {
		t.Errorf("a single signature was accepted: %v, %v", info, err)
	}
	// A manifest signed by keys the build does not know.
	other := newUpdRig(t)
	other.publish("0.5.0", relSigner(t, 7), relSigner(t, 8))
	if info, err := servernode.CheckUpdate(ctx, other.deps); err == nil || info != nil {
		t.Errorf("a manifest of foreign keys was accepted: %v, %v", info, err)
	}
	// Plain http is not a source.
	r.deps.ManifestURL = strings.Replace(r.srv.URL, "https://", "http://", 1) + "/manifest"
	if _, err := servernode.CheckUpdate(ctx, r.deps); err == nil {
		t.Error("an http manifest URL was accepted")
	}
	// A build without release keys cannot update.
	r.deps.ManifestURL = r.srv.URL + "/manifest"
	r.deps.Manager = nil
	if _, err := servernode.CheckUpdate(ctx, r.deps); !errors.Is(err, servernode.ErrNoReleaseKeys) {
		t.Errorf("no manager: %v", err)
	}
	// A development build is never updated.
	r = newUpdRig(t)
	r.deps.CurrentVersion = "dev"
	if _, err := servernode.CheckUpdate(ctx, r.deps); !errors.Is(err, servernode.ErrDevBuild) {
		t.Errorf("dev build: %v", err)
	}
	// No artifact for this platform.
	r = newUpdRig(t)
	r.deps.Platform = "plan9-386"
	if _, err := servernode.CheckUpdate(ctx, r.deps); !errors.Is(err, servernode.ErrNoArtifact) {
		t.Errorf("no artifact: %v", err)
	}
}

// Rolling back is not an update.
func TestUpdateNeverGoesBack(t *testing.T) {
	r := newUpdRig(t)
	r.deps.CurrentVersion = "0.5.0"
	if _, err := servernode.CheckUpdate(context.Background(), r.deps); !errors.Is(err, servernode.ErrDowngrade) {
		t.Fatalf("an older manifest: %v", err)
	}
	r = newUpdRig(t)
	err := servernode.ApplyUpdate(context.Background(), r.deps, servernode.UpdateInfo{Version: "0.2.0", Artifact: "x", URL: r.srv.URL + "/echofog-node", Size: 3, SHA256: make([]byte, 32)})
	if !errors.Is(err, servernode.ErrDowngrade) || r.served.Load() != 0 || r.exeContent() != "old binary v0.3.0" {
		t.Fatalf("ApplyUpdate of an older version: %v, downloads %d", err, r.served.Load())
	}
}

func TestApplyUpdateNeedsConsent(t *testing.T) {
	r := newUpdRig(t)
	info := mustInfo(t, r)
	r.deps.Confirm = func(servernode.UpdateInfo) bool { return false }
	if err := servernode.ApplyUpdate(context.Background(), r.deps, *info); !errors.Is(err, servernode.ErrNotConfirmed) {
		t.Fatalf("ApplyUpdate = %v", err)
	}
	if r.served.Load() != 0 || r.exeContent() != "old binary v0.3.0" {
		t.Fatal("something happened without consent")
	}
	r.deps.Confirm = nil
	if err := servernode.ApplyUpdate(context.Background(), r.deps, *info); !errors.Is(err, servernode.ErrNotConfirmed) {
		t.Fatalf("ApplyUpdate without a Confirm = %v: no consent callback means no consent", err)
	}
}

func TestApplyUpdateInstallsAndKeepsThePrevious(t *testing.T) {
	r := newUpdRig(t)
	info := mustInfo(t, r)
	var asked servernode.UpdateInfo
	r.deps.Confirm = func(i servernode.UpdateInfo) bool { asked = i; return true }
	if err := servernode.ApplyUpdate(context.Background(), r.deps, *info); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if asked.Version != "0.4.0" {
		t.Errorf("the operator was asked about %+v", asked)
	}
	if got := r.exeContent(); got != string(r.artifact) {
		t.Fatalf("binary %q", got)
	}
	prev, err := os.ReadFile(r.exe + ".previous")
	if err != nil || string(prev) != "old binary v0.3.0" {
		t.Fatalf("previous binary: %q, %v", prev, err)
	}
	if fi, _ := os.Stat(r.exe); fi.Mode().Perm()&0o100 == 0 && os.PathSeparator == '/' {
		t.Error("the new binary is not executable")
	}
	left, _ := filepath.Glob(r.exe + ".tmp*")
	if len(left) != 0 {
		t.Errorf("leftovers %v", left)
	}
}

func TestApplyUpdateRollsBack(t *testing.T) {
	ctx := context.Background()
	tests := map[string]func(*updRig){
		"the hash does not match": func(r *updRig) { r.artifact = []byte("tampered") },
		"the new binary does not start": func(r *updRig) {
			r.deps.SelfTest = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
		},
		"the new binary reports another version": func(r *updRig) {
			r.deps.SelfTest = func(context.Context, string) (string, error) { return "0.3.0", nil }
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := newUpdRig(t)
			info := mustInfo(t, r)
			mutate(r)
			err := servernode.ApplyUpdate(ctx, r.deps, *info)
			if err == nil {
				t.Fatal("a bad update was installed")
			}
			if r.exeContent() != "old binary v0.3.0" {
				t.Fatalf("the old binary was not kept: %q", r.exeContent())
			}
			left, _ := filepath.Glob(r.exe + ".*")
			for _, f := range left {
				if strings.Contains(f, ".tmp") {
					t.Errorf("leftover %s", f)
				}
			}
		})
	}
	// A start failure after the swap restores the old binary and says so.
	r := newUpdRig(t)
	info := mustInfo(t, r)
	calls := 0
	r.deps.SelfTest = func(_ context.Context, exe string) (string, error) {
		calls++
		if calls == 1 {
			return "0.4.0", nil // the downloaded file, before the swap
		}
		return "", errors.New("crashed")
	}
	if err := servernode.ApplyUpdate(ctx, r.deps, *info); !errors.Is(err, servernode.ErrUpdateFailed) {
		t.Fatalf("ApplyUpdate = %v", err)
	}
	if r.exeContent() != "old binary v0.3.0" {
		t.Fatalf("not restored: %q", r.exeContent())
	}
}

func TestApplyUpdateBoundsTheDownload(t *testing.T) {
	ctx := context.Background()
	r := newUpdRig(t)
	info := mustInfo(t, r)

	big := *info
	big.Size = 512 << 20
	if err := servernode.ApplyUpdate(ctx, r.deps, big); err == nil {
		t.Error("a 512 MiB artifact was accepted")
	}
	plain := *info
	plain.URL = strings.Replace(info.URL, "https://", "http://", 1)
	if err := servernode.ApplyUpdate(ctx, r.deps, plain); err == nil {
		t.Error("an http artifact URL was accepted")
	}
	// The server sends more than the manifest says.
	r.artifact = bytes.Repeat([]byte("x"), len(r.artifact)+1000)
	if err := servernode.ApplyUpdate(ctx, r.deps, *info); err == nil {
		t.Error("an oversized body was accepted")
	}
	if r.exeContent() != "old binary v0.3.0" {
		t.Error("the binary changed")
	}
}

// The CLI never installs without being told to.
func TestCLIUpdate(t *testing.T) {
	r := newUpdRig(t)
	servernode.ReleaseKeys = []keys.AKey{r.signers[0].Public(), r.signers[1].Public(), r.signers[2].Public()}
	servernode.UpdateHTTPClient = r.srv.Client()
	servernode.UpdateExePath = r.exe
	servernode.BuildVersion = "0.3.0"
	servernode.UpdateNow = func() time.Time { return updNow }
	t.Cleanup(func() {
		servernode.ReleaseKeys, servernode.UpdateHTTPClient, servernode.UpdateExePath, servernode.BuildVersion = nil, nil, "", "dev"
		servernode.UpdateNow = nil
	})

	dir, _ := profile(t, false)
	var out, errb bytes.Buffer
	args := []string{"update", "--data", dir, "--manifest-url", r.srv.URL + "/manifest"}

	// --check only reports.
	if code := servernode.MainIO(append(args, "--check"), nil, strings.NewReader(""), &out, &errb); code != 0 || !strings.Contains(out.String(), "0.4.0") {
		t.Fatalf("update --check: %d %s %s", code, out.String(), errb.String())
	}
	if r.exeContent() != "old binary v0.3.0" || r.served.Load() != 0 {
		t.Fatal("--check changed something")
	}
	// Without --yes the operator is asked; "n" stops it.
	out.Reset()
	if code := servernode.MainIO(args, nil, strings.NewReader("n\n"), &out, &errb); code == 0 || r.exeContent() != "old binary v0.3.0" {
		t.Fatalf("declined update: code %d, binary %q", code, r.exeContent())
	}
	// End of input is not consent either.
	if code := servernode.MainIO(args, nil, strings.NewReader(""), &out, &errb); code == 0 || r.exeContent() != "old binary v0.3.0" {
		t.Fatalf("update without an answer: code %d", code)
	}
	// An explicit yes installs (the self-test runs the real file in production; here it cannot).
	servernode.UpdateSelfTest = func(context.Context, string) (string, error) { return "0.4.0", nil }
	t.Cleanup(func() { servernode.UpdateSelfTest = nil })
	out.Reset()
	if code := servernode.MainIO(append(args, "--yes"), nil, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("update --yes: %d %s", code, errb.String())
	}
	if r.exeContent() != string(r.artifact) || !strings.Contains(out.String(), "restart") {
		t.Fatalf("binary %q, output %q", r.exeContent(), out.String())
	}
}

func TestCLIUpdateWithoutKeys(t *testing.T) {
	servernode.ReleaseKeys = nil
	dir, _ := profile(t, false)
	var out, errb bytes.Buffer
	if code := servernode.MainIO([]string{"update", "--data", dir, "--manifest-url", "https://example.invalid/m"}, nil, strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "release keys") {
		t.Fatalf("update in an unofficial build: %d %s", code, errb.String())
	}
}
