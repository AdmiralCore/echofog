//go:build !android && !ios

// Package mobilecheck holds build checks that only make sense on a host toolchain.
package mobilecheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// androidPackages are the packages T12.0 changes. The desktop WebRTC bridge and the
// system TUN helper must not reach a cgo-enabled Android build: the first pulls pion,
// the second needs os/exec and tun2socks. The rest of internal/services/call keeps the
// v1 network until T9.0 ports its transport.
var androidPackages = []string{
	"./internal/services/call/bridge",
	"./internal/network/tunmode",
}

var forbiddenAndroidDeps = []string{
	"github.com/wlynxg/anet",
	"github.com/pion/webrtc/v4",
	"github.com/xjasonlyu/tun2socks/v2",
	"os/exec",
}

func TestAndroidBuildPullsNoDesktopOnlyCode(t *testing.T) {
	root := moduleRoot(t)
	for _, pkg := range androidPackages {
		deps := goListDeps(t, root, pkg)
		for _, bad := range forbiddenAndroidDeps {
			if hasLine(deps, bad) {
				t.Errorf("%s: an Android build pulls %s", pkg, bad)
			}
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func goListDeps(t *testing.T, root, pkg string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", pkg)
	cmd.Dir = root
	cmd.Env = androidEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Split(string(out), "\n")
}

// androidEnv keeps the host environment but pins the cross-build settings, so the test
// does not inherit GOOS/GOARCH from an outer cross-compilation run.
func androidEnv() []string {
	const pinned = "GOOS GOARCH CGO_ENABLED GOWORK GOFLAGS GO111MODULE"
	drop := map[string]bool{}
	for _, k := range strings.Fields(pinned) {
		drop[k] = true
	}
	env := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GOOS=android",
		"GOARCH=arm64",
		"CGO_ENABLED=1",
		"GOWORK=off",
		"GOFLAGS=-buildvcs=false",
	)
}

func hasLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
