package logging

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupCapturesDefaultSlogAndRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echofog.log")
	Setup(Options{Level: slog.LevelDebug, MaxEntries: 100, FilePath: path})
	t.Cleanup(Close)

	// a plain package-level slog call anywhere must be captured
	slog.Info("network up", "mode", "quic", "peers", 3)
	slog.Warn("login", "password", "hunter2") // must be redacted

	got := Recent(0)
	if len(got) < 2 {
		t.Fatalf("expected the two lines to be captured, got %d", len(got))
	}
	var sawNet, sawRedacted bool
	for _, e := range got {
		if e.Message == "network up" && e.Attrs["mode"] == "quic" && e.Attrs["peers"] == "3" {
			sawNet = true
		}
		if e.Message == "login" {
			if e.Attrs["password"] != "<redacted>" {
				t.Fatalf("password was not redacted: %q", e.Attrs["password"])
			}
			sawRedacted = true
		}
	}
	if !sawNet || !sawRedacted {
		t.Fatalf("missing lines: net=%v redacted=%v", sawNet, sawRedacted)
	}

	// Dump produces shareable text that never contains the secret
	dump := Dump()
	if strings.Contains(dump, "hunter2") {
		t.Fatal("the secret leaked into the dump")
	}
	if !strings.Contains(dump, "network up") {
		t.Fatal("dump missing a known line")
	}
}

func TestRingBufferDropsOldest(t *testing.T) {
	Setup(Options{Level: slog.LevelInfo, MaxEntries: 10})
	for i := 0; i < 25; i++ {
		slog.Info("line", "i", i)
	}
	got := Recent(0)
	if len(got) != 10 {
		t.Fatalf("ring should cap at 10, got %d", len(got))
	}
	// the newest line is kept, the oldest dropped
	if got[len(got)-1].Attrs["i"] != "24" {
		t.Fatalf("newest line missing, last i=%q", got[len(got)-1].Attrs["i"])
	}
	if got[0].Attrs["i"] != "15" {
		t.Fatalf("oldest retained line should be i=15, got %q", got[0].Attrs["i"])
	}
}

func TestComponentAttrSurvivesWith(t *testing.T) {
	Setup(Options{Level: slog.LevelInfo, MaxEntries: 50})
	slog.Default().With("component", "swarm").Info("peer added", "id", "abc")
	for _, e := range Recent(0) {
		if e.Message == "peer added" {
			if e.Attrs["component"] != "swarm" || e.Attrs["id"] != "abc" {
				t.Fatalf("attrs lost: %+v", e.Attrs)
			}
			if !strings.Contains(e.Line(), "component=swarm") {
				t.Fatalf("Line() missing component: %q", e.Line())
			}
			return
		}
	}
	t.Fatal("line with component not found")
}
