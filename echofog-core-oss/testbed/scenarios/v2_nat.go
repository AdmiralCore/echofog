package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func natScenario() Scenario {
	return Scenario{Name: "v2-nat", Title: "NAT matrix: punch where it can, the relay where it cannot", Live: liveNAT, Run: runNat}
}

// runNat is not written yet: nothing measured, so nothing green.
func runNat(_ context.Context, env Env) Result {
	return Result{Name: "v2-nat", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-nat/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}
