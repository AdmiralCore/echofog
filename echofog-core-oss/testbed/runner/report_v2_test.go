package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/testbed/browserprofiles"
	"github.com/echofog/echofog-core/testbed/censor"
)

func f64(v float64) *float64 { return &v }

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
			"D1": {Recall: f64(1), FPR: 0, N: 3},
		},
		Aggregate: aggregate{VisibilityPct: 12.5},
	})
	if r.Schema != 2 {
		t.Fatalf("schema = %d, want 2", r.Schema)
	}
	// The WSL race run cannot read this Windows worktree's .git file, so specVersion falls back
	// to "v2"; specLabel is tested separately for the v2@<hash> form.
	if r.Spec != "v2" && !strings.HasPrefix(r.Spec, "v2@") {
		t.Errorf("spec = %q, want v2@<hash>", r.Spec)
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

func TestLabelFlowUsesTheHarnessPortNotTheClock(t *testing.T) {
	labels := map[uint16]harnessLabel{
		4341: {Suite: "udp-noise", Label: censor.LabelCarrier},
		4344: {Suite: "utls-chrome", Label: censor.LabelBrowser},
		4345: {Suite: "quic-go", Label: censor.LabelTool},
	}
	cases := []struct {
		server string
		label  censor.Label
		suite  string
	}{
		{"203.0.113.15:4341", censor.LabelCarrier, "udp-noise"},
		{"203.0.113.15:4344", censor.LabelBrowser, "utls-chrome"},
		{"203.0.113.15:4345", censor.LabelTool, "quic-go"},
		{"203.0.113.30:443", censor.LabelTool, "curl-control"},
		{"203.0.113.15:4242", censor.LabelProduct, "v1-product"},
		{"203.0.113.11:4242", censor.LabelLab, ""},
	}
	for _, tc := range cases {
		f := censor.Flow{Proto: "tcp", Server: netip.MustParseAddrPort(tc.server)}
		got := labelFlow(f, labels)
		if got.Label != tc.label || got.Suite != tc.suite {
			t.Errorf("%s: label=%q suite=%q, want %q/%q", tc.server, got.Label, got.Suite, tc.label, tc.suite)
		}
	}
}

// A hard pcap budget: the sink must stop at the limit, not sometime after it.
func TestCaptureSinkNeverExceedsBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wan.pcap")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const limit = 1000
	s := newCaptureSink(limit, f)
	chunk := bytes.Repeat([]byte{0xaa}, 700)
	if _, err := s.Write(chunk); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err = s.Write(chunk)
	if !errors.Is(err, errCaptureLimit) {
		t.Fatalf("second write error = %v, want errCaptureLimit", err)
	}
	if s.Size() > limit {
		t.Errorf("sink size %d > budget %d", s.Size(), limit)
	}
	if !s.Over() {
		t.Error("the sink did not record a budget violation")
	}
	if fi, err := f.Stat(); err != nil || fi.Size() > limit {
		t.Errorf("file size = %v, %v; want <= %d", fi, err, limit)
	}
}

// Without -pcap no file may be created at all, not even a temporary one.
func TestNoPcapRequestLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	c := &v2Capture{gw: "gw_b", out: dir, budget: 1 << 20}
	c.sink = newCaptureSink(c.budget, nil)
	if _, err := c.sink.Write([]byte("pcap bytes")); err != nil {
		t.Fatal(err)
	}
	info := c.finalizeInfo(false, true)
	if info.Kept || info.Deleted {
		t.Errorf("no -pcap: kept=%v deleted=%v, want neither", info.Kept, info.Deleted)
	}
	if !strings.Contains(info.Reason, "memory") {
		t.Errorf("reason = %q, want an explanation that nothing was written", info.Reason)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files were created without -pcap: %v", entries)
	}
}

// A detector with no positive control must not be reported as working.
func TestCoverageDoesNotClaimControlWorksWithoutPositives(t *testing.T) {
	rows := coverageFor(map[string]censor.DetectorMetric{
		"D1": {Recall: nil, FPR: 0, Positives: 0, N: 0},
	}, carrierInfo{})
	for _, r := range rows {
		if r.Detector == "D1" {
			if strings.Contains(r.Verdict, "control works") {
				t.Errorf("D1 with no positives: %q", r.Verdict)
			}
			if !strings.Contains(r.Verdict, "no positive") && !strings.Contains(r.Verdict, "unavailable") {
				t.Errorf("D1 verdict = %q, want a no-positive/unavailable explanation", r.Verdict)
			}
		}
	}
}

func TestSpecLabelPinsTheSpec(t *testing.T) {
	if got := specLabel("abc123"); got != "v2@abc123" {
		t.Errorf("specLabel = %q, want v2@abc123", got)
	}
	if got := specLabel(""); got != "v2" {
		t.Errorf("specLabel empty = %q, want v2", got)
	}
}

func TestBrowserProfileNoteUsesTheCaptureNetwork(t *testing.T) {
	db := &browserprofiles.Database{Version: "1", Entries: []browserprofiles.Profile{
		{ID: "chrome-lab-lab", Country: "lab", Network: "lab"},
	}}
	note := browserProfileNote(db, "ru-mobile")
	if strings.Contains(note, "lab/ru-mobile") {
		t.Errorf("note still claims the capture is lab/ru-mobile: %s", note)
	}
	if !strings.Contains(note, "lab/lab") || !strings.Contains(note, "ru-mobile") {
		t.Errorf("note = %q, want the capture network and the run label", note)
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
