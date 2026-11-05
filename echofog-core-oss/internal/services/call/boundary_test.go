package call

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The call service is a v1 domain package that a v2 node plugs in (AD1, AD2). Its packages must
// not reach into the v1 network: the only piece of internal/network they may use is the leaf that
// names frame types, net_types.
func TestCallPackagesDoNotImportTheV1Network(t *testing.T) {
	const module = "github.com/echofog/echofog-core/internal/services/call"
	for _, pkg := range []string{module, module + "/media", module + "/signaling"} {
		cmd := exec.Command("go", "list", "-deps", pkg)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if forbiddenCallDep(dep) {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}

func forbiddenCallDep(dep string) bool {
	const core = "github.com/echofog/echofog-core/"
	switch {
	case strings.HasPrefix(dep, core+"internal/p2p/"):
		return true
	case dep == core+"internal/proto":
		return true
	case dep == core+"internal/network":
		return true
	case strings.HasPrefix(dep, core+"internal/network/") && dep != core+"internal/network/net_types":
		return true
	}
	return false
}
