package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func callScenario() Scenario {
	return Scenario{Name: "v2-call", Title: "Call: media across the network, with loss", Live: liveCall, Run: runCall}
}

// runCall is not written yet: nothing measured, so nothing green.
func runCall(_ context.Context, env Env) Result {
	return Result{Name: "v2-call", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-call/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}
