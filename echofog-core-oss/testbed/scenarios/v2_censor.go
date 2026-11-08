package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func censorScenario() Scenario {
	return Scenario{Name: "v2-censor", Title: "Censor: four carriers, the classifier does not single them out, the probe sees the cover", Live: liveCensor, Run: runCensor}
}

// runCensor is not written yet: nothing measured, so nothing green.
func runCensor(_ context.Context, env Env) Result {
	return Result{Name: "v2-censor", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-censor/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}
