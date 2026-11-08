package scenarios

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/gate"
)

func TestTheRegistryHasTheSevenScenariosOfTheTask(t *testing.T) {
	var names []string
	for _, s := range Registry() {
		names = append(names, s.Name)
		assert.NotNil(t, s.Run, s.Name)
		assert.NotEmpty(t, s.Live.Command, "%s has a live command", s.Name)
		assert.NotEmpty(t, s.Live.Services, "%s has live services", s.Name)
	}
	assert.Equal(t, gate.RequiredScenarios, names, "the registry and the gate agree on the scenarios")
}

func stub(rows ...gate.Row) Scenario {
	i := 0
	return Scenario{Name: "v2-nat", Run: func(context.Context, Env) Result {
		r := Result{Name: "v2-nat", Mode: "model", Rows: []gate.Row{rows[min(i, len(rows)-1)]}}
		i++
		return r
	}}
}

func TestAResultWithoutRowsIsNotMeasured(t *testing.T) {
	assert.Equal(t, gate.NotMeasured, Result{}.Verdict())
	assert.Equal(t, gate.Green, Result{Rows: []gate.Row{{Status: gate.Green}, {Status: gate.Info}}}.Verdict())
	assert.Equal(t, gate.Red, Result{Rows: []gate.Row{{Status: gate.Green}, {Status: gate.Red}, {Status: gate.NotMeasured}}}.Verdict())
	assert.Equal(t, gate.NotMeasured, Result{Rows: []gate.Row{{Status: gate.Green}, {Status: gate.NotMeasured}}}.Verdict())
	assert.Equal(t, gate.Yellow, Result{Rows: []gate.Row{{Status: gate.Green}, {Status: gate.Yellow}}}.Verdict())
}

func TestRunManyKeepsTheWorstStatusOfEveryRowOverThreeRuns(t *testing.T) {
	green := gate.Row{ID: "x", Status: gate.Green}
	yellow := gate.Row{ID: "x", Status: gate.Yellow}
	out, runs := RunMany(context.Background(), stub(green, yellow, green), Env{}, 3)
	require.Len(t, runs, 3)
	assert.Equal(t, 3, out.Runs)
	assert.Equal(t, "model", out.Mode)
	assert.Equal(t, "v2-nat", out.Name)
	require.Len(t, out.Rows, 1)
	assert.Equal(t, gate.Yellow, out.Rows[0].Status)
}

func TestRunManyStopsAtTheFirstRedRun(t *testing.T) {
	green := gate.Row{ID: "x", Status: gate.Green}
	red := gate.Row{ID: "x", Status: gate.Red}
	out, runs := RunMany(context.Background(), stub(green, red, green), Env{}, 3)
	assert.Len(t, runs, 2, "a red run ends the series: three in a row cannot happen")
	assert.Equal(t, 2, out.Runs)
	require.Len(t, out.Rows, 1)
	assert.Equal(t, gate.Red, out.Rows[0].Status)
}

func TestRunManyHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, runs := RunMany(ctx, stub(gate.Row{ID: "x", Status: gate.Green}), Env{}, 3)
	assert.Empty(t, runs)
	assert.Equal(t, 0, out.Runs)
}
