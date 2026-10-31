package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const panicTrace = `panic: runtime error: invalid memory address or nil pointer dereference
[signal 0xc0000005 code=0x0 addr=0x18 pc=0x7ff6b1c2]

goroutine 112 [running]:
github.com/echofog/echofog-core/internal/services/call/media.(*Dispatcher).recoverFragments(0x0, {0xc0001, 0x4})
	D:/a/echofog/echofog/echofog-core-oss/internal/services/call/media/dispatcher.go:412 +0x52
github.com/echofog/echofog-core/internal/services/call/media.(*Dispatcher).ProcessIncomingVideo(...)
	D:/a/echofog/echofog/echofog-core-oss/internal/services/call/media/dispatcher.go:390
created by github.com/echofog/echofog-core/pkg/node.(*Node).Start in goroutine 1
	D:/a/echofog/echofog/echofog-core-oss/pkg/node/node.go:700 +0x1a5
`

const fatalTrace = `runtime: goroutine stack exceeds 1048576-byte limit
runtime: sp=0xc020160398 stack=[0xc020160000, 0xc020360000]
fatal error: stack overflow

runtime stack:
runtime.throw({0x7ff6, 0xe})
	C:/Program Files/Go/src/runtime/panic.go:1101 +0x4d

goroutine 7 gp=0xc000102380 m=4 mp=0xc000100008 [running]:
runtime.morestack()
	C:/Program Files/Go/src/runtime/asm_amd64.s:621 +0x25
main.crashTest.func1.1(0x5a3e)
	C:/Users/someone/echofog-desktop-oss/crash.go:255 +0x45 fp=0xc0201603a8 sp=0xc0201603a0 pc=0x7ff6
`

func TestParseCrashFindsReasonAndPlace(t *testing.T) {
	text := trimTracePaths(panicTrace)
	reason, where := parseCrash(text)
	if reason != "panic: runtime error: invalid memory address or nil pointer dereference" {
		t.Fatalf("reason %q", reason)
	}
	if where != "github.com/echofog/echofog-core/internal/services/call/media.(*Dispatcher).recoverFragments (media/dispatcher.go:412)" {
		t.Fatalf("where %q", where)
	}
	if strings.Contains(text, "D:/a/") {
		t.Fatalf("build paths kept:\n%s", text)
	}

	reason, where = parseCrash(trimTracePaths(fatalTrace))
	if reason != "fatal error: stack overflow" {
		t.Fatalf("reason %q", reason)
	}
	if !strings.HasPrefix(where, "main.crashTest.func1.1 (echofog-desktop-oss/crash.go:255") {
		t.Fatalf("where %q", where)
	}
}

func TestParseCrashWithoutAStack(t *testing.T) {
	reason, where := parseCrash("\nfatal error: out of memory\n")
	if reason != "fatal error: out of memory" || where != "" {
		t.Fatalf("%q %q", reason, where)
	}
	if reason, _ := parseCrash("something odd\nmore"); reason != "something odd" {
		t.Fatalf("reason %q", reason)
	}
}

func TestNewCrashTextReportsOnlyNewCrashes(t *testing.T) {
	dir := t.TempDir()
	log, seen := filepath.Join(dir, "crash.log"), filepath.Join(dir, "crash.seen")
	appendTo := func(s string) {
		f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}

	appendTo("panic: from long ago\n")
	if got := newCrashText(log, seen); got != "" {
		t.Fatalf("the first start reported an old crash: %q", got)
	}
	if got := newCrashText(log, seen); got != "" {
		t.Fatalf("nothing new, got %q", got)
	}
	appendTo(panicTrace)
	if got := newCrashText(log, seen); !strings.HasPrefix(got, "panic: runtime error") {
		t.Fatalf("new crash not found: %q", got)
	}
	if got := newCrashText(log, seen); got != "" {
		t.Fatalf("a crash was reported twice: %q", got)
	}
}

func TestCrashLogIsCutBack(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "crash.log")
	line := strings.Repeat("x", 99) + "\n"
	if err := os.WriteFile(log, []byte(strings.Repeat(line, (crashLogLimit/len(line))+50)), 0o600); err != nil {
		t.Fatal(err)
	}
	size := trimCrashLog(log, fileSize(log))
	if size > crashLogKeep || size != fileSize(log) {
		t.Fatalf("crash.log is %d bytes (file %d)", size, fileSize(log))
	}
	data, _ := os.ReadFile(log)
	if !strings.HasPrefix(string(data), "xxx") || !strings.HasSuffix(string(data), "x\n") {
		t.Fatalf("cut in the middle of a line")
	}
}

func TestParseCrashOfAFatalErrorWithoutItsLine(t *testing.T) {
	// what crash.log really gets from a fatal error: the "fatal error: …" line went to stderr only
	text := fatalTrace[strings.Index(fatalTrace, "runtime stack:"):]
	reason, where := parseCrash(trimTracePaths(text))
	if reason != "fatal error" || !strings.HasPrefix(where, "main.crashTest.func1.1") {
		t.Fatalf("%q %q", reason, where)
	}
}
