package servernode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// publishMin signs a manifest with a lower bound for the version.
func (r *updRig) publishMin(version, min string) {
	r.t.Helper()
	sum := sha256.Sum256(r.artifact)
	rel := &pb.Release{Version: version, MinVersion: min, ReleasedAt: uint64(updNow.Add(-time.Minute).Unix()), Artifacts: []*pb.Artifact{
		{Platform: "linux-amd64", Name: "echofog-node", Sha256: sum[:], Size: uint64(len(r.artifact)), Url: r.srv.URL + "/echofog-node"},
		{Platform: runtime.GOOS + "-" + runtime.GOARCH, Name: "echofog-node", Sha256: sum[:], Size: uint64(len(r.artifact)), Url: r.srv.URL + "/echofog-node"},
	}}
	raw, err := objects.SealRelease(r.signers[0], rel.ReleasedAt+399*86400, rel, r.signers[1])
	if err != nil {
		r.t.Fatal(err)
	}
	r.manifest.Store(&raw)
}

// T12.4e: the update never installs a version below the manifest's min_version, and a rollback below
// the installed version needs the explicit flag; the flag never goes below min_version.
func TestUpdateMinVersion(t *testing.T) {
	ctx := context.Background()

	t.Run("a newer version at or above min_version installs", func(t *testing.T) {
		r := newUpdRig(t)
		r.publishMin("0.4.0", "0.3.0")
		if info, err := servernode.CheckUpdate(ctx, r.deps); err != nil || info == nil || info.Version != "0.4.0" || info.MinVersion != "0.3.0" {
			t.Fatalf("CheckUpdate = %+v, %v", info, err)
		}
	})

	t.Run("an older version is refused without the flag", func(t *testing.T) {
		r := newUpdRig(t)
		r.publishMin("0.4.0", "0.1.0")
		r.deps.CurrentVersion = "0.5.0"
		if _, err := servernode.CheckUpdate(ctx, r.deps); !errors.Is(err, servernode.ErrDowngrade) {
			t.Fatalf("rollback without the flag: %v", err)
		}
	})

	t.Run("the flag allows a rollback above min_version, marked as a rollback", func(t *testing.T) {
		r := newUpdRig(t)
		r.publishMin("0.4.0", "0.4.0")
		r.deps.CurrentVersion = "0.5.0"
		r.deps.AllowRollback = true
		info, err := servernode.CheckUpdate(ctx, r.deps)
		if err != nil || info == nil || !info.Rollback {
			t.Fatalf("CheckUpdate with the flag = %+v, %v", info, err)
		}
		if err := servernode.ApplyUpdate(ctx, r.deps, *info); err != nil {
			t.Fatalf("ApplyUpdate rollback: %v", err)
		}
		if r.exeContent() != string(r.artifact) {
			t.Error("the rollback was not installed")
		}
	})

	t.Run("the flag never goes below min_version", func(t *testing.T) {
		r := newUpdRig(t)
		// The signed manifest says nothing older than 0.9.0 may be installed; it carries 0.4.0, which
		// the manifest check already refuses (version below its own bound).
		r.publishMin("0.4.0", "0.3.0")
		r.deps.CurrentVersion = "0.5.0"
		r.deps.AllowRollback = true
		info := mustInfo(t, r)
		info.MinVersion = "0.5.0" // a bound above the offered version, as an older manifest of the same file would carry
		before := r.served.Load()
		if err := servernode.ApplyUpdate(ctx, r.deps, *info); !errors.Is(err, servernode.ErrBelowMin) {
			t.Fatalf("ApplyUpdate below min_version: %v", err)
		}
		if r.served.Load() != before || r.exeContent() != "old binary v0.3.0" {
			t.Error("a version below min_version was downloaded or installed")
		}
	})

	t.Run("apply refuses a rollback that was not allowed", func(t *testing.T) {
		r := newUpdRig(t)
		r.publishMin("0.4.0", "0.1.0")
		r.deps.CurrentVersion = "0.5.0"
		info := servernode.UpdateInfo{Version: "0.4.0", Artifact: "x", URL: r.srv.URL + "/echofog-node", Size: 3, SHA256: make([]byte, 32), Rollback: true}
		if err := servernode.ApplyUpdate(ctx, r.deps, info); !errors.Is(err, servernode.ErrDowngrade) {
			t.Fatalf("a Rollback info without AllowRollback: %v", err)
		}
	})
}

func TestCLIUpdateRollbackFlag(t *testing.T) {
	r := newUpdRig(t)
	r.publishMin("0.4.0", "0.4.0")
	servernode.ReleaseKeys = []keys.AKey{r.signers[0].Public(), r.signers[1].Public(), r.signers[2].Public()}
	servernode.UpdateHTTPClient = r.srv.Client()
	servernode.UpdateExePath = r.exe
	servernode.BuildVersion = "0.5.0"
	servernode.UpdateNow = func() time.Time { return updNow }
	servernode.UpdateSelfTest = func(context.Context, string) (string, error) { return "0.4.0", nil }
	t.Cleanup(func() {
		servernode.ReleaseKeys, servernode.UpdateHTTPClient, servernode.UpdateExePath, servernode.BuildVersion = nil, nil, "", "dev"
		servernode.UpdateNow, servernode.UpdateSelfTest = nil, nil
	})
	dir, _ := profile(t, false)
	var out, errb bytes.Buffer
	args := []string{"update", "--data", dir, "--manifest-url", r.srv.URL + "/manifest", "--yes"}
	if code := servernode.MainIO(args, nil, strings.NewReader(""), &out, &errb); code == 0 || r.exeContent() != "old binary v0.3.0" {
		t.Fatalf("rollback without the flag: code %d, %q", code, r.exeContent())
	}
	out.Reset()
	errb.Reset()
	if code := servernode.MainIO(append(args, "--allow-rollback"), nil, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("update --allow-rollback: %d %s", code, errb.String())
	}
	if r.exeContent() != string(r.artifact) {
		t.Error("the rollback was not installed")
	}
	if !strings.Contains(strings.ToLower(out.String()+errb.String()), "rollback") {
		t.Errorf("no warning about the rollback:\n%s%s", out.String(), errb.String())
	}
}
