package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func hiddenScenario() Scenario {
	return Scenario{Name: "v2-hidden", Title: "Hidden node: the invitation opens it, a stranger gets silence", Live: liveHidden, Run: runHidden}
}

// runHidden is not written yet: nothing measured, so nothing green.
func runHidden(_ context.Context, env Env) Result {
	return Result{Name: "v2-hidden", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-hidden/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}
