package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vectorTime is the instant of the spec vectors: 2026-11-03T00:00:00Z.
var vectorTime = time.Unix(1793664000, 0)

func vectorLink(t *testing.T, which string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "protocol", "v2", "vectors", "link.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	s, _ := doc[which]["link"].(string)
	if s == "" {
		t.Fatalf("no %s link in the vector", which)
	}
	return s
}

func TestLinkOpenPrintsPreviewOffline(t *testing.T) {
	for _, tc := range []struct{ which, kind string }{{"contact", "contact"}, {"invitation", "invitation"}} {
		link := vectorLink(t, tc.which)
		var out bytes.Buffer
		if err := linkOpen(&out, link, vectorTime); err != nil {
			t.Fatalf("%s: %v", tc.which, err)
		}
		var doc struct {
			Kind     string `json:"kind"`
			NodeID   string `json:"node_id"`
			DeviceID string `json:"device_id"`
			NotAfter string `json:"not_after"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("%s: output is not JSON: %v\n%s", tc.which, err, out.String())
		}
		if doc.Kind != tc.kind || len(doc.NodeID) != 64 || len(doc.DeviceID) != 64 || doc.NotAfter == "" {
			t.Fatalf("%s: %+v", tc.which, doc)
		}
		if i := strings.Index(link, "&s="); i >= 0 && strings.Contains(out.String(), link[i+3:]) {
			t.Fatalf("%s: the preview prints the access secret", tc.which)
		}
	}
}

func TestLinkOpenAcceptsSlashForm(t *testing.T) {
	link := strings.Replace(vectorLink(t, "contact"), "echofog://join?", "echofog://join/?", 1)
	if err := linkOpen(&bytes.Buffer{}, link, vectorTime); err != nil {
		t.Fatalf("slash form: %v", err)
	}
}

func TestLinkOpenErrorsAreReadable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "не ссылка EchoFog"},
		{"echofog://join?v=3&card=AAAA", "другой версией"},
	}
	for _, tc := range cases {
		err := linkOpen(&bytes.Buffer{}, tc.in, vectorTime)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("linkOpen(%q) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

func TestLinkSubcommandUsage(t *testing.T) {
	for _, args := range [][]string{
		{"link"},
		{"link", "open"},
		{"link", "open", "a", "b"},
		{"link", "nothing"},
		{"link", "join", "--data", t.TempDir()},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%v) succeeded", args)
		}
	}
}
