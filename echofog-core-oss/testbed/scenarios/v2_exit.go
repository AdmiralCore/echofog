package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func exitScenario() Scenario {
	return Scenario{Name: "v2-exit", Title: "Exit: a download through the exit, whole, and the exit gives its resources back", Live: liveExit, Run: runExit}
}

// runExit is not written yet: nothing measured, so nothing green.
func runExit(_ context.Context, env Env) Result {
	return Result{Name: "v2-exit", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-exit/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}
