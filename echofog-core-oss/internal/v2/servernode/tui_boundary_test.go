package servernode_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTUIStaysInTheNodeCommand pins the dependency boundary of T12.4f: the TUI libraries are
// imported only by internal/v2/servernode and cmd/echofog-node, and never reach the mobile
// package, the app runtime or the desktop (the desktop is another module).
func TestTUIStaysInTheNodeCommand(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go command is not in PATH")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tui := func(path string) bool {
		return strings.HasPrefix(path, "github.com/charmbracelet/") || path == "rsc.io/qr"
	}
	allowed := map[string]bool{
		"github.com/echofog/echofog-core/internal/v2/servernode": true,
		"github.com/echofog/echofog-core/cmd/echofog-node":       true,
	}

	type listed struct {
		ImportPath string
		Imports    []string
	}
	out := goList(t, root, "-json", "./...")
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var p listed
		err := dec.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		for _, imp := range p.Imports {
			if tui(imp) && !allowed[p.ImportPath] {
				t.Errorf("%s imports the TUI library %s: the TUI must stay in servernode and the node command", p.ImportPath, imp)
			}
		}
	}

	// The mobile package and the app runtime must not even reach them indirectly.
	for _, pattern := range []string{"./mobile", "./internal/v2/app"} {
		deps := goList(t, root, "-deps", pattern)
		for _, line := range strings.Split(deps, "\n") {
			if tui(strings.TrimSpace(line)) {
				t.Errorf("%s depends on the TUI library %s", pattern, line)
			}
		}
	}
}

func goList(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return string(out)
}
