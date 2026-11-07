package gate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Command(args, &out, &errb, func() time.Time { return now }, func() string { return "deadbee" })
	return code, out.String(), errb.String()
}

func TestTheNightlyCommandWritesTheChecklistAndTheVerdict(t *testing.T) {
	dir := t.TempDir()
	report := write(t, dir, "report.json", goodRun(t))
	beh := write(t, dir, "behaviour.json", behaviourReport(false))
	outDir := filepath.Join(dir, "gate-out")

	code, stdout, stderr := runCommand(t, "-report", report, "-behaviour", beh, "-out", outDir)
	assert.Equal(t, 2, code, "no baseline and no speed: the gate is not green")
	assert.Contains(t, stderr, "not measured")
	code, stdout, stderr = runCommand(t, "-report", report, "-behaviour", beh, "-out", outDir, "-allow-unmeasured")
	assert.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "not measured")
	md, err := os.ReadFile(filepath.Join(outDir, "checklist.md"))
	require.NoError(t, err)
	assert.Contains(t, string(md), "deadbee")
	assert.Contains(t, string(md), "- [x] visibility_pct is 0 %")
	raw, err := os.ReadFile(filepath.Join(outDir, "gate.json"))
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, "deadbee", back["Commit"])
	assert.Contains(t, string(raw), `"green"`, "statuses are words in gate.json")
}

func TestTheNightlyCommandFailsOnASwappedReportAndOnProductDefects(t *testing.T) {
	dir := t.TempDir()
	run := goodRun(t)
	run.Aggregate.VisibilityPct = 12
	swapped := write(t, dir, "swapped.json", run)
	code, stdout, _ := runCommand(t, "-report", swapped, "-out", filepath.Join(dir, "a"))
	assert.Equal(t, 1, code)
	assert.Contains(t, stdout, "red")

	good := write(t, dir, "good.json", goodRun(t))
	defects := write(t, dir, "defects.json", behaviourReport(true))
	code, stdout, _ = runCommand(t, "-report", good, "-behaviour", defects, "-out", filepath.Join(dir, "b"))
	assert.Equal(t, 1, code, "the open findings of the product keep the release blocked")
	assert.Contains(t, stdout, "product:freeze-no-limit")
	assert.Contains(t, stdout, "waits for a fix of the product")
}

func TestTheNightlyCommandIsStrictByDefault(t *testing.T) {
	dir := t.TempDir()
	report := write(t, dir, "report.json", goodRun(t))
	code, _, stderr := runCommand(t, "-report", report, "-out", dir)
	assert.Equal(t, 2, code, "no baseline, no behaviour report: not measured")
	assert.Contains(t, stderr, "not measured")
	code, _, _ = runCommand(t, "-report", report, "-out", dir, "-allow-unmeasured")
	assert.Equal(t, 0, code, "a person who wants a partial run says so")
	code, _, _ = runCommand(t, "-report", report, "-out", dir, "-strict")
	assert.Equal(t, 3, code, "the old flag is gone: strict is the default now")
}

func TestTheNightlyCommandSignsABaseline(t *testing.T) {
	dir := t.TempDir()
	report := write(t, dir, "report.json", goodRun(t))
	speed := write(t, dir, "speed.json", map[string]float64{"noise3": 90})
	basePath := filepath.Join(dir, "baseline.json")
	code, _, stderr := runCommand(t, "-report", report, "-speed", speed, "-write-baseline", basePath)
	require.Equal(t, 0, code, stderr)
	f, err := os.Open(basePath)
	require.NoError(t, err)
	defer f.Close()
	b, err := LoadBaseline(f)
	require.NoError(t, err)
	assert.Equal(t, "deadbee", b.Commit)
	assert.Equal(t, now, b.SignedAt)
	assert.Equal(t, 90.0, b.Speed["noise3"])

	// and the regression check works against it
	code, _, _ = runCommand(t, "-report", report, "-baseline", basePath, "-speed", write(t, dir, "slow.json", map[string]float64{"noise3": 70}), "-out", dir)
	assert.Equal(t, 1, code, "22 % slower than the baseline")
}

func TestTheNightlyCommandRefusesBadArguments(t *testing.T) {
	code, _, stderr := runCommand(t)
	assert.Equal(t, 3, code)
	assert.Contains(t, stderr, "-report")
	code, _, stderr = runCommand(t, "-report", filepath.Join(t.TempDir(), "none.json"))
	assert.Equal(t, 3, code)
	assert.NotEmpty(t, strings.TrimSpace(stderr))
	code, _, _ = runCommand(t, "-nonsense")
	assert.Equal(t, 3, code)
}

func FuzzLoadRunReport(f *testing.F) {
	f.Add(`{"schema":2,"detectors":{"D1":{"recall":1,"fpr":0}}}`)
	f.Add(`{"schema":2,"detectors":{"D5":{"recall":null,"fpr":0.5,"auc":0.9}},"v2_gate":{"rows":[]},"probes":[{"outcomes":[{}]}]}`)
	f.Add(`{`)
	f.Fuzz(func(t *testing.T, raw string) {
		r, err := LoadRunReport(strings.NewReader(raw))
		if err != nil {
			return
		}
		// whatever loads can be judged, compared with itself and written, without a panic
		res := Evaluate(Input{Run: r, Now: now})
		base := MakeBaseline(*r, nil, now, "c")
		same := Evaluate(Input{Run: r, Baseline: &base, Now: now})
		if len(same.Changes) != 0 {
			t.Fatalf("a run differs from itself: %+v", same.Changes)
		}
		_ = RenderChecklist(res)
	})
}
