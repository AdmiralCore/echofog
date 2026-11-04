package v2_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// platformTags and archTags are the build constraints that make a file platform-specific.
// The list is here, in the test, so that adding a new platform is a deliberate change
// (platforms.md §1.1).
var (
	platformTags = []string{
		"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios",
		"js", "linux", "netbsd", "openbsd", "plan9", "solaris", "unix", "wasip1", "windows",
	}
	archTags = []string{
		"386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le",
		"mipsle", "ppc64", "ppc64le", "riscv64", "s390x", "wasm",
	}
)

// TestNoCgo enforces "No cgo in EchoFog's own code" (platforms.md §1.1): no package of the v2
// tree and no mobile binding may import "C".
func TestNoCgo(t *testing.T) {
	t.Parallel()

	for _, path := range collectGoFiles(t, protocolRoots(t)...) {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"C"` {
				t.Errorf("%s imports \"C\": EchoFog's own code must stay pure Go (platforms.md §1.1)", path)
			}
		}
	}
}

// TestPlatformFallbacks enforces "every platform-specific file has a fallback that compiles
// everywhere else" (platforms.md §1.1). Files are grouped by base name: a group that contains a
// file constrained by a platform or architecture tag must also contain a file without any such
// constraint (the `_other.go` convention).
func TestPlatformFallbacks(t *testing.T) {
	t.Parallel()

	type scannedFile struct {
		path        string
		constrained bool
	}

	groups := map[string][]scannedFile{}
	for _, path := range collectGoFiles(t, protocolRoots(t)...) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		constrained, base := classifyPlatformFile(filepath.Base(path), buildConstraint(src))
		key := filepath.Dir(path) + "/" + base
		groups[key] = append(groups[key], scannedFile{path: path, constrained: constrained})
	}

	for key, group := range groups {
		var constrained, portable []string
		for _, f := range group {
			if f.constrained {
				constrained = append(constrained, f.path)
			} else {
				portable = append(portable, f.path)
			}
		}
		if len(constrained) == 0 || len(portable) > 0 {
			continue
		}
		sort.Strings(constrained)
		t.Errorf("platform-specific files %v have no portable fallback (platforms.md §1.1): add %s_other.go",
			constrained, key)
	}
}

func TestClassifyPlatformFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		file        string
		build       string
		constrained bool
		base        string
	}{
		{name: "plain file", file: "doc.go", base: "doc"},
		{name: "test file", file: "doc_test.go", base: "doc"},
		{name: "goos suffix", file: "helper_windows.go", constrained: true, base: "helper"},
		{name: "goarch suffix", file: "asm_arm64.go", constrained: true, base: "asm"},
		{name: "goos and goarch suffix", file: "sock_linux_amd64.go", constrained: true, base: "sock"},
		{name: "other suffix is portable", file: "helper_other.go", base: "helper"},
		{name: "build tag", file: "helper.go", build: "windows", constrained: true, base: "helper"},
		{name: "negated build tag", file: "helper.go", build: "!windows", constrained: true, base: "helper"},
		{name: "combined build tag", file: "helper.go", build: "linux && !android", constrained: true, base: "helper"},
		{name: "non-platform build tag", file: "helper.go", build: "ignore", base: "helper"},
		{name: "tag and suffix", file: "helper_linux.go", build: "linux && !android", constrained: true, base: "helper"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			constrained, base := classifyPlatformFile(tc.file, tc.build)
			if constrained != tc.constrained || base != tc.base {
				t.Fatalf("classifyPlatformFile(%q, %q) = (%v, %q), want (%v, %q)",
					tc.file, tc.build, constrained, base, tc.constrained, tc.base)
			}
		})
	}
}

func TestBuildConstraint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "go:build before package",
			src:  "// Copyright\n\n//go:build windows || linux\n\npackage foo\n",
			want: "windows || linux",
		},
		{name: "no constraint", src: "package foo\n", want: ""},
		{name: "build line after package is ignored", src: "package foo\n\n//go:build windows\n", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := buildConstraint([]byte(tc.src)); got != tc.want {
				t.Fatalf("buildConstraint(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// protocolRoots returns the directories whose platform rules are checked: the v2 tree and,
// once it exists (T12.1), the gomobile binding package.
func protocolRoots(t *testing.T) []string {
	t.Helper()

	roots := []string{"."}
	mobile := filepath.Join("..", "..", "mobile")
	if info, err := os.Stat(mobile); err == nil && info.IsDir() {
		roots = append(roots, mobile)
	}
	return roots
}

func collectGoFiles(t *testing.T, roots ...string) []string {
	t.Helper()

	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(files)
	return files
}
