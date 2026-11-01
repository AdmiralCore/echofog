//go:build windows && !dev

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The watcher's copy is this test binary: started with appChildFlag it plays the window process
// (ECHOFOG_WATCH_TEST says how it ends) instead of running the tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == appChildFlag {
		os.Exit(watchTestWindow(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func watchTestWindow(args []string) int {
	keepCrashes()
	dir := os.Getenv("ECHOFOG_WATCH_DIR")
	runs, _ := os.ReadFile(filepath.Join(dir, "runs"))
	n, _ := strconv.Atoi(string(runs))
	n++
	_ = os.WriteFile(filepath.Join(dir, "runs"), []byte(strconv.Itoa(n)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "args"+strconv.Itoa(n)), []byte(strings.Join(args, " ")), 0o600)
	switch mode := os.Getenv("ECHOFOG_WATCH_TEST"); {
	case mode == "panic-once" && n == 1, mode == "panic":
		panic("watch test")
	case mode == "fatal":
		crashTest("fatal")
		select {}
	case mode == "exit":
		return 1 // closed some other way: no crash
	}
	return 0
}

func runWatched(t *testing.T, mode string, answer bool) (asked []CrashReport, dir string) {
	dir = t.TempDir()
	t.Setenv("LocalAppData", dir) // crash.log goes to os.UserCacheDir
	t.Setenv("ECHOFOG_WATCH_DIR", dir)
	t.Setenv("ECHOFOG_WATCH_TEST", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	done := watchLoop(exe, []string{"echofog://join?x"}, func(r CrashReport) bool {
		asked = append(asked, r)
		return answer && len(asked) < 3
	})
	if !done {
		t.Fatal("the watcher did not run the window")
	}
	return asked, dir
}

func TestWatcherShowsACrash(t *testing.T) {
	asked, _ := runWatched(t, "panic", false)
	if len(asked) != 1 || asked[0].Reason != "panic: watch test" {
		t.Fatalf("asked %+v", asked)
	}
	if !strings.Contains(asked[0].Where, "watchTestWindow") {
		t.Fatalf("where %q", asked[0].Where)
	}
}

func TestWatcherRestartsWithoutTheLink(t *testing.T) {
	asked, dir := runWatched(t, "panic-once", true)
	if len(asked) != 1 {
		t.Fatalf("asked %d times", len(asked))
	}
	first, _ := os.ReadFile(filepath.Join(dir, "args1"))
	second, err := os.ReadFile(filepath.Join(dir, "args2"))
	if err != nil {
		t.Fatal("not restarted")
	}
	if string(first) != "echofog://join?x" || string(second) != "" {
		t.Fatalf("args %q then %q", first, second)
	}
}

func TestWatcherIsQuietWithoutACrash(t *testing.T) {
	for _, mode := range []string{"exit", "clean"} {
		if asked, _ := runWatched(t, mode, true); len(asked) != 0 {
			t.Fatalf("%s: asked %+v", mode, asked)
		}
	}
}

func TestWatcherNamesAFatalError(t *testing.T) {
	asked, dir := runWatched(t, "fatal", false)
	if len(asked) != 1 || asked[0].Reason != "fatal error: stack overflow" {
		t.Fatalf("asked %+v", asked)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "EchoFog", "crash.log"))
	if !strings.HasSuffix(string(log), "fatal error: stack overflow\n") {
		t.Fatal("the reason was not added to crash.log")
	}
}

func TestFatalLineAndTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 8}
	for i := 0; i < 10; i++ {
		_, _ = b.Write([]byte("0123456789"))
	}
	if s := b.String(); len(s) > 16 || !strings.HasSuffix(s, "456789") {
		t.Fatalf("tail %q", s)
	}
	out := "runtime: goroutine stack exceeds 1048576-byte limit\nfatal error: stack overflow\n\nruntime stack:\n"
	if l := fatalLine(out); l != "fatal error: stack overflow" {
		t.Fatalf("line %q", l)
	}
	if l := fatalLine("log: no fatal error: here\n"); l != "" {
		t.Fatalf("line %q", l)
	}
}
