package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/testbed/gate"
)

func messagingScenario() Scenario {
	return Scenario{Name: "v2-messaging", Title: "Messaging: every accepted message arrives exactly once", Live: liveMessaging, Run: runMessaging}
}

// runMessaging is not written yet: nothing measured, so nothing green.
func runMessaging(_ context.Context, env Env) Result {
	return Result{Name: "v2-messaging", Mode: "model", Seed: env.Seed, Rows: []gate.Row{{
		ID: "v2-messaging/implemented", Metric: "scenario", Value: "not written", Limit: "measured", Status: gate.NotMeasured, Source: "T15.1"}}}
}
