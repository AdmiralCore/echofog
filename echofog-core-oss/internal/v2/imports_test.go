package v2_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// modulePath is the Go module of the core. TestModulePathMatchesGoMod keeps it in sync.
const modulePath = "github.com/echofog/echofog-core"

// forbiddenFamilies are the v1 trees v2 code must not reach, directly or transitively
// (02-architecture AD1).
var forbiddenFamilies = []string{
	"internal/p2p",
	"internal/proto",
	"pkg/node",
	"pkg/api/node",
}

// allowedV1 is what v2 may reuse from v1 in place (02-architecture §4.1).
//
// AD2: v1 domain logic is used on the spot through ports, not copied. A v1 package that is not
// on this list needs a port task in the dev branch before v2 may import it.
var allowedV1 = []string{
	"pkg/api/types",
	"internal/network/net_types",
	"pkg/events",
	"pkg/dto",
	"pkg/videolayer",
	"internal/ratelimit",
	"internal/config",
	"internal/cache",
	"internal/crypto",
	"internal/storage",
	"internal/wallet/tonwallet",
	"internal/services/convlog",
	"internal/services/groups",
	"internal/services/call/tree",
	"internal/services/filexfer",
	"internal/protection/osprotect",
	"internal/network/tunmode",
}

func TestModulePathMatchesGoMod(t *testing.T) {
	t.Parallel()

	if got := modulePathFromGoMod(t); got != modulePath {
		t.Fatalf("go.mod declares module %q, tests assume %q", got, modulePath)
	}
}

func TestForbiddenImport(t *testing.T) {
	t.Parallel()

	mod := modulePath + "/"
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"p2p swarm", mod + "internal/p2p/swarm", true},
		{"p2p itself", mod + "internal/p2p", true},
		{"proto", mod + "internal/proto", true},
		{"proto subpackage", mod + "internal/proto/message", true},
		{"node", mod + "pkg/node", true},
		{"node subpackage", mod + "pkg/node/internal", true},
		{"api node", mod + "pkg/api/node", true},
		{"api node subpackage", mod + "pkg/api/node/geoip", true},
		{"network transport", mod + "internal/network", true},
		{"network dpi", mod + "internal/network/dpi", true},
		{"network handshake", mod + "internal/network/handshake", true},
		{"network net_types", mod + "internal/network/net_types", false},
		{"network tunmode", mod + "internal/network/tunmode", false},
		{"network tunmode subpackage", mod + "internal/network/tunmode/helper", false},
		{"v1 port package", mod + "internal/services/contacts", false},
		// A path that merely contains "internal/proto" is not the v1 proto tree.
		{"protobuf internal", "google.golang.org/protobuf/internal/protolazy", false},
		{"another module", "example.com/other/internal/p2p/swarm", false},
		{"standard library", "fmt", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := forbiddenImport(tc.path, modulePath); got != tc.want {
				t.Fatalf("forbiddenImport(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestFindChain(t *testing.T) {
	t.Parallel()

	bad := func(path string) bool { return path == "bad" }

	cases := []struct {
		name    string
		imports map[string][]string
		want    []string
	}{
		{
			name:    "shortest chain wins",
			imports: map[string][]string{"root": {"a", "b"}, "a": {"c"}, "b": {"bad"}, "c": {"bad"}},
			want:    []string{"root", "b", "bad"},
		},
		{
			name:    "no forbidden package",
			imports: map[string][]string{"root": {"a"}, "a": {"fmt"}},
			want:    nil,
		},
		{
			name:    "root itself is forbidden",
			imports: map[string][]string{"root": {}},
			want:    nil,
		},
		{
			name:    "cycle terminates",
			imports: map[string][]string{"root": {"a"}, "a": {"b"}, "b": {"a"}},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := "root"
			if tc.name == "root itself is forbidden" {
				got := findChain(root, tc.imports, func(path string) bool { return path == root })
				if len(got) != 1 || got[0] != root {
					t.Fatalf("findChain(%q) = %v, want [%s]", root, got, root)
				}
				return
			}
			if got := findChain(root, tc.imports, bad); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("findChain(%q) = %v, want %v", root, got, tc.want)
			}
		})
	}
}

// TestAllowedV1NetworkExceptions keeps the AD1 network whitelist and the §4.1 list from drifting
// apart: only net_types and tunmode may be imported from internal/network/....
func TestAllowedV1NetworkExceptions(t *testing.T) {
	t.Parallel()

	want := map[string]bool{"net_types": true, "tunmode": true}
	got := map[string]bool{}
	for _, path := range allowedV1 {
		if rest, ok := strings.CutPrefix(path, "internal/network/"); ok {
			got[rest] = true
		}
	}
	if len(got) != len(want) {
		t.Fatalf("allowedV1 lists network imports %v, AD1 permits exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("allowedV1 is missing internal/network/%s (AD1)", name)
		}
	}
	// And the whitelist itself must actually pass the rule.
	for _, path := range allowedV1 {
		if forbiddenImport(modulePath+"/"+path, modulePath) {
			t.Fatalf("allowedV1 entry %q is rejected by forbiddenImport", path)
		}
	}
}

func TestImportBoundary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("the go command is not in PATH, but this is CI: the import boundary must not be skipped")
		}
		t.Skip("the go command is not in PATH; the import boundary needs `go list` (skipped outside CI)")
	}

	patterns := []string{"./internal/v2/..."}
	if v := strings.TrimSpace(os.Getenv("ECHOFOG_IMPORT_PATTERNS")); v != "" {
		patterns = strings.Fields(v)
	}

	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module directory: %v", err)
	}
	mod := modulePathFromGoMod(t)
	pkgs := listDeps(t, dir, patterns)

	imports := make(map[string][]string, len(pkgs))
	var roots []string
	for path, p := range pkgs {
		imports[path] = p.Imports
		if !p.DepOnly {
			roots = append(roots, path)
		}
	}
	sort.Strings(roots)
	if len(roots) == 0 {
		t.Fatalf("`go list -deps %s` returned no root packages", strings.Join(patterns, " "))
	}
	if len(patterns) == 1 && patterns[0] == "./internal/v2/..." {
		v2roots := 0
		for _, root := range roots {
			if strings.HasPrefix(root, mod+"/internal/v2") {
				v2roots++
			}
		}
		if v2roots == 0 {
			t.Fatalf("no v2 root packages found under %s/internal/v2 (patterns silently matched nothing)", mod)
		}
	}

	for _, root := range roots {
		chain := findChain(root, imports, func(path string) bool { return forbiddenImport(path, mod) })
		if chain != nil {
			t.Errorf("%s reaches a package forbidden by AD1 (02-architecture §2):\n    %s\n  port the domain logic instead of importing the v1 network (AD2)",
				root, strings.Join(chain, "\n    -> "))
		}
	}
}

// modulePathFromGoMod reads the module directive of echofog-core-oss/go.mod. The test package
// lives in internal/v2, so the module root is two levels up.
func modulePathFromGoMod(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("go.mod has no module directive")
	return ""
}

// forbiddenImport reports whether path is a v1 package that v2 code must not import, directly or
// transitively (AD1). Paths outside the core module are never v2's business.
func forbiddenImport(path, module string) bool {
	rel, ok := strings.CutPrefix(path, module+"/")
	if !ok {
		return false
	}

	for _, family := range forbiddenFamilies {
		if rel == family || strings.HasPrefix(rel, family+"/") {
			return true
		}
	}

	if rel == "internal/network" || strings.HasPrefix(rel, "internal/network/") {
		for _, allowed := range allowedV1 {
			rest, ok := strings.CutPrefix(allowed, "internal/network/")
			if !ok {
				continue
			}
			if rel == "internal/network/"+rest || strings.HasPrefix(rel, "internal/network/"+rest+"/") {
				return false
			}
		}
		return true
	}

	return false
}

// findChain returns the shortest import chain from root to a package for which bad is true.
// A nil result means root's transitive imports are clean. Breadth-first search keeps the
// reported chain short, which is what a failing CI message needs.
func findChain(root string, imports map[string][]string, bad func(string) bool) []string {
	if bad(root) {
		return []string{root}
	}

	type step struct {
		path  string
		chain []string
	}
	seen := map[string]bool{root: true}
	queue := []step{{path: root, chain: []string{root}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range imports[cur.path] {
			if seen[next] {
				continue
			}
			seen[next] = true
			chain := append(append([]string(nil), cur.chain...), next)
			if bad(next) {
				return chain
			}
			queue = append(queue, step{path: next, chain: chain})
		}
	}
	return nil
}

type listedPackage struct {
	ImportPath string
	DepOnly    bool
	Imports    []string
}

// listDeps runs `go list -deps -json` for the given patterns from the module root.
func listDeps(t *testing.T, dir string, patterns []string) map[string]*listedPackage {
	t.Helper()

	args := append([]string{"list", "-deps", "-json"}, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", strings.Join(patterns, " "), err, stderr.String())
	}

	pkgs := make(map[string]*listedPackage)
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode `go list -deps -json` output: %v", err)
		}
		pkgs[p.ImportPath] = &p
	}
	return pkgs
}
