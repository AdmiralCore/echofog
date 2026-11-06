package servernode_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A server exposed to the internet has no profiling or expvar endpoint: those packages register
// handlers on the default mux as a side effect of the import.
func TestNoDebugPackagesImported(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range src.Imports {
			switch imp.Path.Value {
			case `"net/http/pprof"`, `"expvar"`, `"runtime/pprof"`:
				t.Errorf("%s imports %s", f, imp.Path.Value)
			}
		}
	}
	_ = os.Getenv
}
