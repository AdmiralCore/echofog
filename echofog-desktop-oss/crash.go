package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A crash (a panic nobody recovered, a fatal runtime error) goes to crash.log next to the app's log.
// On Windows the EchoFog.exe the user starts only watches a second copy of itself that runs the
// window (watch_windows.go): when that copy dies with a crash, the watcher shows a system dialog
// with the reason and offers a restart. Whichever way the app was started, the next start finds
// the new part of crash.log and the window offers a report about it.

// appChildFlag as os.Args[1] means the watcher started this process to run the window.
const appChildFlag = "--echofog-window"

const (
	crashTextMax  = 64 << 10  // how much of one crash is kept for the report
	crashLogLimit = 1 << 20   // crash.log is cut back once it grows past this
	crashLogKeep  = 256 << 10 // to its last this many bytes
)

// CrashReport is a crash from an earlier run, for the window to offer as a report.
type CrashReport struct {
	Reason  string `json:"reason"`  // "panic: …" or "fatal error: …"
	Where   string `json:"where"`   // the first function outside the Go runtime, "pkg.Func (dir/file.go:12)"
	Text    string `json:"text"`    // the trace, with build paths cut down
	Version string `json:"version"` // this build
	OS      string `json:"os"`
}

var (
	lastCrashMu sync.Mutex
	lastCrash   *CrashReport
)

func crashDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "EchoFog")
}

func crashLogPath() string { return filepath.Join(crashDir(), "crash.log") }

// keepCrashes notes a crash of the previous run for the window, then sends this run's crash output
// to crash.log: a windowed app has no console, so the trace was lost and a crash at login left
// nothing to go on.
func keepCrashes() {
	dir := crashDir()
	_ = os.MkdirAll(dir, 0o700)
	path := crashLogPath()
	if text := newCrashText(path, filepath.Join(dir, "crash.seen")); text != "" {
		r := crashReport(text)
		lastCrashMu.Lock()
		lastCrash = &r
		lastCrashMu.Unlock()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	_ = f.Close() // SetCrashOutput keeps its own copy
}

// newCrashText returns what crash.log gained since the last start, and remembers its new length in
// seenPath. The first start that keeps a seenPath reports nothing: older crashes are not news.
func newCrashText(logPath, seenPath string) string {
	size := fileSize(logPath)
	var text string
	if data, err := os.ReadFile(seenPath); err == nil {
		if seen, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && seen >= 0 && seen < size {
			text = readCrashFrom(logPath, seen)
		}
	}
	size = trimCrashLog(logPath, size)
	_ = os.WriteFile(seenPath, []byte(strconv.FormatInt(size, 10)), 0o600)
	return text
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// readCrashFrom reads crash.log from offset on, at most crashTextMax bytes.
func readCrashFrom(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(f, crashTextMax))
	return strings.TrimSpace(string(data))
}

// trimCrashLog cuts crash.log back to its last crashLogKeep bytes, from a line start, once it is
// past crashLogLimit, and returns its length.
func trimCrashLog(path string, size int64) int64 {
	if size <= crashLogLimit {
		return size
	}
	tail := readCrashFrom(path, size-crashLogKeep)
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	tail += "\n"
	if err := os.WriteFile(path, []byte(tail), 0o600); err != nil {
		return size
	}
	return int64(len(tail))
}

// crashReport makes a report of a crash trace.
func crashReport(text string) CrashReport {
	text = trimTracePaths(text)
	reason, where := parseCrash(text)
	return CrashReport{
		Reason:  reason,
		Where:   where,
		Text:    text,
		Version: version,
		OS:      runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// parseCrash finds the reason of a Go crash trace (its "panic: …" or "fatal error: …" line) and
// the first function outside the Go runtime in the goroutine that crashed.
func parseCrash(text string) (reason, where string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	stack := -1
	for i, l := range lines {
		if reason == "" && (strings.HasPrefix(l, "panic: ") || strings.HasPrefix(l, "fatal error: ") || strings.HasPrefix(l, "Exception 0x")) {
			reason = strings.TrimSpace(l)
		}
		if stack < 0 && strings.HasPrefix(l, "goroutine ") && strings.HasSuffix(strings.TrimSpace(l), ":") {
			stack = i + 1
		}
	}
	if reason == "" {
		for _, l := range lines {
			if l = strings.TrimSpace(l); l != "" {
				reason = l
				break
			}
		}
	}
	if stack < 0 {
		return reason, ""
	}
	// the stack is pairs of lines, the function and then "\tfile.go:12 +0x1d", up to an empty line
	for i := stack; i < len(lines) && lines[i] != ""; i++ {
		fn := lines[i]
		if strings.HasPrefix(fn, "\t") || strings.HasPrefix(fn, "runtime.") || strings.HasPrefix(fn, "panic(") ||
			strings.HasPrefix(fn, "internal/") || strings.HasPrefix(fn, "created by ") {
			continue
		}
		if p := strings.LastIndexByte(fn, '('); p > 0 && strings.HasSuffix(fn, ")") {
			fn = fn[:p] // the arguments
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "\t") {
			return reason, fn
		}
		file := strings.TrimSpace(lines[i+1])
		if p := strings.LastIndex(file, " +0x"); p > 0 {
			file = file[:p]
		}
		return reason, fn + " (" + file + ")"
	}
	return reason, ""
}

// trimTracePaths cuts the source paths of a trace ("\tC:/Users/…/node/node.go:12 +0x1d") down to
// the file and its folder: the build machine's folders say nothing about the crash, and a local
// build's would name the user.
func trimTracePaths(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "\t") || !strings.Contains(l, ".go:") {
			continue
		}
		path := strings.ReplaceAll(strings.TrimPrefix(l, "\t"), "\\", "/")
		parts := strings.Split(path, "/")
		if len(parts) > 2 {
			path = strings.Join(parts[len(parts)-2:], "/")
		}
		lines[i] = "\t" + path
	}
	return strings.Join(lines, "\n")
}

// LastCrash is the crash of an earlier run that the user has not dealt with yet, or nil.
func (a *App) LastCrash() *CrashReport {
	lastCrashMu.Lock()
	defer lastCrashMu.Unlock()
	return lastCrash
}

// ForgetLastCrash drops the crash report: the user read it, saved it or closed it.
func (a *App) ForgetLastCrash() {
	lastCrashMu.Lock()
	lastCrash = nil
	lastCrashMu.Unlock()
}

// SaveCrashReport writes a crash report to a timestamped file in the home directory and returns
// its path, for the user to attach to a bug report or send into a chat.
func (a *App) SaveCrashReport(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("empty crash report")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	path := filepath.Join(home, fmt.Sprintf("echofog-crash-%s.txt", time.Now().Format("2006-01-02-150405")))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// crashTest makes this run crash a few seconds after start when ECHOFOG_CRASH_TEST is "panic" or
// "fatal", to see the crash dialog and the report without waiting for a real bug.
func crashTest(kind string) {
	if kind != "panic" && kind != "fatal" {
		return
	}
	go func() {
		time.Sleep(4 * time.Second)
		if kind == "fatal" {
			debug.SetMaxStack(1 << 20)
			var deeper func(int) int
			deeper = func(n int) int { return deeper(n+1) + 1 }
			deeper(0)
		}
		panic("crash test: ECHOFOG_CRASH_TEST=panic")
	}()
}
