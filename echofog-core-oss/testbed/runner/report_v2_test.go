package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/testbed/censor"
)

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"2GB":   2 << 30,
		"512MB": 512 << 20,
		"1024":  1024,
		"1.5GB": 3 << 29,
	}
	for in, want := range cases {
		got, err := parseByteSize(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "-1", "GB", "3TB"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestPcapBudgetIsRequiredAndCapped(t *testing.T) {
	if err := validatePcapBudget(0); err == nil {
		t.Error("a zero budget must not start a run")
	}
	if err := validatePcapBudget(2<<30 + 1); err == nil {
		t.Error("a budget above 2 GiB must be refused")
	}
	if err := validatePcapBudget(2 << 30); err != nil {
		t.Errorf("2 GiB: %v", err)
	}
	if err := validatePcapBudget(8 << 20); err != nil {
		t.Errorf("8 MiB: %v", err)
	}
}

func TestFlowsCSVHasTheTopFlows(t *testing.T) {
	var flows []censor.LabeledFlow
	for i := 0; i < 205; i++ {
		flows = append(flows, censor.LabeledFlow{
			Flow: censor.Flow{
				Proto: "tcp", Looks: "TLS", BytesUp: i, BytesDn: 1000 - i, FirstSize: 512,
			},
			Label: censor.LabelBrowser, Suite: "curl-control",
		})
	}
	var b bytes.Buffer
	if err := writeFlowsCSV(&b, flows, 200); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 201 {
		t.Fatalf("%d lines, want a header and 200 flows", len(lines))
	}
	for _, h := range []string{"proto", "client", "server", "suite", "service", "bytes", "duration_ms", "first_size", "label", "verdict", "d1", "d10"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header lacks %q: %s", h, lines[0])
		}
	}
}

func TestReportSchemaTwo(t *testing.T) {
	r := buildReport(reportInput{
		Scenario: "v2-censor", Seed: 42, Profile: "desktop", Network: "lab",
		Phases: []string{"censor"},
		Detectors: map[string]censor.DetectorMetric{
			"D1": {Recall: 1, FPR: 0, N: 3},
		},
		Aggregate: aggregate{VisibilityPct: 12.5},
	})
	if r.Schema != 2 {
		t.Fatalf("schema = %d, want 2", r.Schema)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"schema":2`, `"scenario":"v2-censor"`, `"seed":42`, `"D1"`, `"visibility_pct":12.5`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report lacks %s: %s", want, raw)
		}
	}
}

func TestCompareMarksChangedMetrics(t *testing.T) {
	a := []byte(`{"schema":2,"scenario":"v2-censor","aggregate":{"visibility_pct":10},"detectors":{"D1":{"recall":1,"fpr":0}},"findings":["a"]}`)
	b := []byte(`{"schema":2,"scenario":"v2-censor","aggregate":{"visibility_pct":0},"detectors":{"D1":{"recall":1,"fpr":0}},"findings":["b"]}`)
	out, err := compareReports(a, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"aggregate.visibility_pct", "*", "before", "after", "10", "0", "a", "b"} {
		if !strings.Contains(out, want) {
			t.Errorf("compare output lacks %q:\n%s", want, out)
		}
	}
	// an unchanged metric is shown without the change marker
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "detectors.D1.recall") && strings.Contains(line, "*") {
			t.Errorf("unchanged metric marked as changed: %q", line)
		}
	}
}

func TestPruneOutKeepsSevenDays(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	names := []string{"20261105-100000-v2-censor", "20261020-100000-v2-censor", "20261104-100000-v2-basic"}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(base, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := pruneOut(base, 7*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "20261020") {
		t.Errorf("removed = %v, want only the 16-day-old run", removed)
	}
	if _, err := os.Stat(filepath.Join(base, names[0])); err != nil {
		t.Errorf("fresh run was removed: %v", err)
	}
}

func TestRunLockIsExclusiveAndReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "testbed.lock")
	release, err := acquireRunLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireRunLock(path); err == nil {
		t.Error("a second run took the lock")
	}
	release()
	release2, err := acquireRunLock(path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	release2()
}
