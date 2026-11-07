package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

func TestLoadRunReportReadsTheRunnersReportAndRefusesOtherThings(t *testing.T) {
	good := `{"schema":2,"commit":"abc","detectors":{"D1":{"recall":1,"fpr":0,"n":3}},"aggregate":{},"extra_section":{"x":1}}`
	r, err := LoadRunReport(strings.NewReader(good))
	require.NoError(t, err)
	assert.Equal(t, "abc", r.Commit)
	assert.Contains(t, r.Detectors, "D1")

	for name, bad := range map[string]string{
		"not json":      `{`,
		"schema 1":      `{"schema":1,"detectors":{}}`,
		"no schema":     `{"detectors":{"D1":{}}}`,
		"no detectors":  `{"schema":2}`,
		"trailing data": good + ` {}`,
		"fpr is text":   `{"schema":2,"detectors":{"D1":{"fpr":"high"}}}`,
		"fpr negative":  `{"schema":2,"detectors":{"D1":{"fpr":-0.1}}}`,
		"fpr over one":  `{"schema":2,"detectors":{"D1":{"fpr":1.5}}}`,
		"recall over 1": `{"schema":2,"detectors":{"D1":{"recall":2}}}`,
	} {
		_, err := LoadRunReport(strings.NewReader(bad))
		assert.Error(t, err, name)
	}
}

func TestLoadBaselineKeepsTheSignature(t *testing.T) {
	b := MakeBaseline(*goodRun(t), map[string]float64{"noise3": 80}, now, "deadbee")
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	back, err := LoadBaseline(strings.NewReader(string(raw)))
	require.NoError(t, err)
	assert.Equal(t, "deadbee", back.Commit)
	assert.Equal(t, now, back.SignedAt)

	for name, bad := range map[string]string{
		"unsigned":  `{"run":{"schema":2,"detectors":{"D1":{}}}}`,
		"no commit": `{"signed_at":"2026-11-07T00:00:00Z","run":{"schema":2,"detectors":{"D1":{}}}}`,
		"bad speed": `{"signed_at":"2026-11-07T00:00:00Z","commit":"c","run":{"schema":2,"detectors":{"D1":{}}},"speed_mbps":{"noise3":-1}}`,
		"empty run": `{"signed_at":"2026-11-07T00:00:00Z","commit":"c","run":{}}`,
	} {
		_, err := LoadBaseline(strings.NewReader(bad))
		assert.Error(t, err, name)
	}
}

func TestLoadSpeedAndBehaviour(t *testing.T) {
	s, err := LoadSpeed(strings.NewReader(`{"noise3":78.5,"ws2":20}`))
	require.NoError(t, err)
	assert.Equal(t, 78.5, s["noise3"])
	for _, bad := range []string{`{`, `{"noise3":0}`, `{"noise3":-3}`, `{"noise3":"fast"}`, `[]`, `{}`} {
		_, err := LoadSpeed(strings.NewReader(bad))
		assert.Error(t, err, bad)
	}

	raw, err := json.Marshal(behaviourReport(true))
	require.NoError(t, err)
	b, err := LoadBehaviour(strings.NewReader(string(raw)))
	require.NoError(t, err)
	assert.Len(t, b.Freeze, 2)
	_, err = LoadBehaviour(strings.NewReader(`{`))
	assert.Error(t, err)
	_, err = LoadBehaviour(strings.NewReader(`{}`))
	assert.Error(t, err, "a behaviour report with nothing in it is not a report")
	var _ behaviour.Report
}

func write(t *testing.T, dir, name string, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, raw, 0o644))
	return p
}

// One command, files in, verdict out: the nightly run.
func TestNightlyReadsTheFilesAndRedBlocks(t *testing.T) {
	dir := t.TempDir()
	run := goodRun(t)
	files := Files{Report: write(t, dir, "report.json", run), Behaviour: write(t, dir, "behaviour.json", behaviourReport(false))}
	base := MakeBaseline(*run, map[string]float64{"noise3": 100}, now.Add(-time.Hour*24), "old")
	files.Baseline = write(t, dir, "baseline.json", base)
	files.Speed = write(t, dir, "speed.json", map[string]float64{"noise3": 99})

	res, err := Nightly(files, Input{Now: now, Commit: "deadbee"})
	require.NoError(t, err)
	assert.False(t, res.Blocks)
	assert.Equal(t, "deadbee", res.Commit)
	assert.Equal(t, Green, row(res, "regression", "").Status)
	assert.Equal(t, Green, row(res, "speed", "noise3").Status)
	assert.Equal(t, Green, row(res, "D24", "").Status)

	// the same files with one number changed: red, and the gate says which
	run.Detectors["D3"] = DetectorEntry{Recall: f(1), FPR: 0.08, N: 16, Positives: 2}
	files.Report = write(t, dir, "swapped.json", run)
	res, err = Nightly(files, Input{Now: now})
	require.NoError(t, err)
	assert.True(t, res.Blocks)
	assert.Equal(t, 1, ExitCode(res, false))

	files.Report = filepath.Join(dir, "missing.json")
	_, err = Nightly(files, Input{Now: now})
	assert.Error(t, err, "no report, no gate")

	_, err = Nightly(Files{}, Input{Now: now})
	assert.Error(t, err)
}
