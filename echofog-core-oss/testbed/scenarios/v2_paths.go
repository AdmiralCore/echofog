package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func pathsScenario() Scenario {
	return Scenario{Name: "v2-paths", Title: "Paths: a second path, a cut, an address change, the H15 backoff", Live: livePaths, Run: runPaths}
}

// runPaths is not written yet: nothing measured, so nothing green.
func runPaths(_ context.Context, env Env) Result {
	return Result{Name: "v2-paths", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-paths/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}

// pathsCutWithoutSecondPath is the control of the cut row: one path, cut for good.
func pathsCutWithoutSecondPath(ctx context.Context) gate.Row {
	return gate.Row{ID: "v2-paths/cut-control", Status: gate.NotMeasured}
}
