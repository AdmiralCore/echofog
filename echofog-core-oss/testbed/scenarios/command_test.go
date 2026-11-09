package scenarios

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/gate"
)

func fakeScenario(name string, st gate.Status) Scenario {
	return Scenario{Name: name, Title: name + " title", Live: Live{Command: "go run ./testbed/runner -scenario " + name, Services: []string{"x"}},
		Run: func(context.Context, Env) Result {
			return Result{Name: name, Mode: "model", Rows: []gate.Row{{ID: name + "/r", Metric: "m", Value: "v", Limit: "l", Status: st}},
				NotChecked: []string{"the " + name + " thing nobody checked"}}
		}}
}

func runCommand(t *testing.T, reg []Scenario, args ...string) (code int, out, errOut, dir string) {
	t.Helper()
	dir = t.TempDir()
	var so, se bytes.Buffer
	code = Command(append([]string{"-out", dir}, args...), &so, &se, reg, func() string { return "abc1234" })
	return code, so.String(), se.String(), dir
}

func TestGreenScenariosExitZeroAndWriteTheReportTheGateReads(t *testing.T) {
	reg := []Scenario{fakeScenario("v2-nat", gate.Green), fakeScenario("v2-paths", gate.Green)}
	code, out, _, dir := runCommand(t, reg, "-runs", "3")
	assert.Equal(t, 0, code)
	f, err := os.Open(filepath.Join(dir, "scenarios.json"))
	require.NoError(t, err)
	defer f.Close()
	rep, err := gate.LoadScenarios(f)
	require.NoError(t, err)
	require.Len(t, rep.Scenarios, 2)
	assert.Equal(t, 3, rep.Scenarios[0].Runs)
	assert.Equal(t, "model", rep.Scenarios[0].Mode)
	assert.Equal(t, "abc1234", rep.Commit)

	md, err := os.ReadFile(filepath.Join(dir, "scenarios.md"))
	require.NoError(t, err)
	for _, want := range []string{"v2-nat", "v2-paths", "model", "green", "live", "not verified", "audit"} {
		assert.Contains(t, string(md), want)
	}
	assert.Contains(t, out, "v2-nat")

	// the gate reads it: the model is green, the live stand is not measured
	res := gate.Evaluate(gate.Input{Scenarios: rep.Scenarios, Now: time.Now()})
	for _, r := range res.Rows {
		if r.ID == "scenario:v2-nat" && r.Metric == "live" {
			assert.Equal(t, gate.NotMeasured, r.Status)
		}
		if r.ID == "scenario:v2-nat" && r.Metric == "model runs" {
			assert.Equal(t, gate.Green, r.Status)
		}
	}
}

func TestARedScenarioExitsOneAndStopsItsSeries(t *testing.T) {
	code, _, _, dir := runCommand(t, []Scenario{fakeScenario("v2-nat", gate.Red)}, "-runs", "3")
	assert.Equal(t, 1, code)
	f, _ := os.Open(filepath.Join(dir, "scenarios.json"))
	defer f.Close()
	rep, err := gate.LoadScenarios(f)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Scenarios[0].Runs, "a red run ends the series")
}

func TestANotMeasuredScenarioExitsTwoUnlessAPartialRunIsAllowed(t *testing.T) {
	reg := []Scenario{fakeScenario("v2-nat", gate.NotMeasured)}
	code, _, _, _ := runCommand(t, reg)
	assert.Equal(t, 2, code)
	code, _, _, _ = runCommand(t, reg, "-allow-unmeasured")
	assert.Equal(t, 0, code)
}

func TestScenarioSelectionAndBadArguments(t *testing.T) {
	reg := []Scenario{fakeScenario("v2-nat", gate.Green), fakeScenario("v2-paths", gate.Green)}
	code, _, _, dir := runCommand(t, reg, "-scenario", "v2-paths")
	assert.Equal(t, 0, code)
	f, _ := os.Open(filepath.Join(dir, "scenarios.json"))
	defer f.Close()
	rep, err := gate.LoadScenarios(f)
	require.NoError(t, err)
	require.Len(t, rep.Scenarios, 1)
	assert.Equal(t, "v2-paths", rep.Scenarios[0].Name)

	for _, args := range [][]string{{"-scenario", "v2-moon"}, {"-runs", "0"}, {"-runs", "-2"}, {"-bogus"}} {
		code, _, errOut, _ := runCommand(t, reg, args...)
		assert.Equal(t, 3, code, "%v", args)
		assert.NotEmpty(t, errOut, "%v", args)
	}
}

func TestListPrintsTheScenariosAndTheirLiveCommandsWithoutRunningAnything(t *testing.T) {
	ran := false
	s := fakeScenario("v2-nat", gate.Green)
	s.Run = func(context.Context, Env) Result { ran = true; return Result{} }
	code, out, _, _ := runCommand(t, []Scenario{s}, "-list")
	assert.Equal(t, 0, code)
	assert.False(t, ran)
	assert.Contains(t, out, "v2-nat")
	assert.Contains(t, out, "go run ./testbed/runner -scenario v2-nat")
	assert.True(t, strings.Contains(out, "x"), "the live services")
}
