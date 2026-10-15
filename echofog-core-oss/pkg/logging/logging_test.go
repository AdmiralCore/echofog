package logging

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// handle feeds a record through the recorder the way the fanout does: only when Enabled allows it.
func handle(r *Recorder, level slog.Level, msg string, attrs ...slog.Attr) {
	if !r.Enabled(context.Background(), level) {
		return
	}
	rec := slog.NewRecord(time.Now(), level, msg, 0)
	rec.AddAttrs(attrs...)
	_ = r.Handle(context.Background(), rec)
}

func TestRecorder_RespectsConfiguredLevel(t *testing.T) {
	r := newRecorder(100, "", 0, slog.LevelInfo)

	if r.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug should be disabled when the level is info")
	}
	if !r.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info should be enabled when the level is info")
	}

	handle(r, slog.LevelDebug, "a noisy debug line")
	handle(r, slog.LevelInfo, "an important info line")

	dump := r.Dump()
	if strings.Contains(dump, "noisy debug") {
		t.Errorf("debug line was captured despite the info level:\n%s", dump)
	}
	if !strings.Contains(dump, "important info") {
		t.Errorf("info line was not captured:\n%s", dump)
	}
}

func TestRecorder_MasksIPAddresses(t *testing.T) {
	r := newRecorder(100, "", 0, slog.LevelDebug)

	handle(r, slog.LevelInfo, "connected to 203.0.113.7:4001",
		slog.String("addr", "203.0.113.7:4001"),
		slog.String("peer", "ab12cd34ef"),
		slog.String("v6", "fe80::1"),
	)
	dump := r.Dump()

	if strings.Contains(dump, "203.0.113.7") {
		t.Errorf("IPv4 address was not masked:\n%s", dump)
	}
	if !strings.Contains(dump, "[ip]:4001") {
		t.Errorf("expected the masked IP to keep its port:\n%s", dump)
	}
	if strings.Contains(dump, "fe80::1") {
		t.Errorf("IPv6 address was not masked:\n%s", dump)
	}
	if !strings.Contains(dump, "ab12cd34ef") {
		t.Errorf("a non-IP identifier was wrongly masked:\n%s", dump)
	}
}
