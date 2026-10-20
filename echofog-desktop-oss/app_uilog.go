package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Problems in the interface (a camera that would not open, a WebRTC connection that failed, a
// screen shared without sound) used to stay in the webview's console, which nobody sees in a
// release build. LogUI puts them into the same log as the core's, so one file tells the whole
// story of a call.

const (
	uiLogMaxMsg   = 300
	uiLogMaxAttrs = 24
	uiLogMaxValue = 400
	uiLogEvery    = 10 * time.Second // per message, after uiLogBurst lines
	uiLogBurst    = 5
)

type uiLogKey struct{ component, msg string }

type uiLogCount struct {
	since   time.Time
	n       int // lines let through since `since`
	skipped int // lines held back since the last one let through
}

var uiLog struct {
	mu   sync.Mutex
	seen map[uiLogKey]*uiLogCount
}

// uiLogAllow lets a burst of the same message through, then one line every uiLogEvery with how
// many were held back, so a loop in the interface cannot flood the log.
func uiLogAllow(k uiLogKey, now time.Time) (bool, int) {
	uiLog.mu.Lock()
	defer uiLog.mu.Unlock()
	if uiLog.seen == nil || len(uiLog.seen) > 512 {
		uiLog.seen = map[uiLogKey]*uiLogCount{}
	}
	c := uiLog.seen[k]
	if c == nil || now.Sub(c.since) >= uiLogEvery {
		skipped := 0
		if c != nil {
			skipped = c.skipped
		}
		uiLog.seen[k] = &uiLogCount{since: now, n: 1}
		return true, skipped
	}
	if c.n < uiLogBurst {
		c.n++
		return true, 0
	}
	c.skipped++
	return false, 0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// LogUI writes one line from the interface into the app's log. level is "debug", "info",
// "warn" or "error"; component names the part of the interface ("call", "screen", "groups").
func (a *App) LogUI(level, component, msg string, attrs map[string]any) {
	component = clip(strings.TrimSpace(component), 32)
	msg = clip(strings.TrimSpace(msg), uiLogMaxMsg)
	if msg == "" {
		return
	}
	ok, skipped := uiLogAllow(uiLogKey{component, msg}, time.Now())
	if !ok {
		return
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > uiLogMaxAttrs {
		keys = keys[:uiLogMaxAttrs]
	}
	args := make([]any, 0, 2*len(keys)+6)
	args = append(args, "component", "ui-"+component)
	for _, k := range keys {
		args = append(args, clip(k, 40), clip(fmt.Sprint(attrs[k]), uiLogMaxValue))
	}
	if skipped > 0 {
		args = append(args, "more_since_last", skipped)
	}
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "info":
		lv = slog.LevelInfo
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelWarn
	}
	slog.Log(context.Background(), lv, msg, args...)
}
