// Package scenarios holds the v2 scenarios of the launchpad (T15.1): NAT, paths, hidden nodes,
// messaging, calls, the exit and the censor. Each scenario runs the product's own code (internal/v2)
// against a model of the network and of the censor, in one process and without containers: that is
// the "model" mode. The same scenarios have a "live" mode on the Docker stand, prepared in the
// compose file and the entrypoint and run by testbed/runner; it needs Docker and is not run here.
//
// A scenario returns rows with a status, in the vocabulary of testbed/gate. A row is green only
// when it was measured against its limit; a thing the scenario could not measure is NotMeasured,
// never green. The limits are the design's and are never loosened to turn a scenario green: what
// the product gets wrong is a finding.
package scenarios

import (
	"context"
	"time"

	"github.com/echofog/echofog-core/testbed/gate"
)

// Env is what a scenario gets.
type Env struct {
	Seed  int64
	Short bool   // a smaller run: fewer cases, shorter transfers
	Dir   string // scratch directory; the scenario removes what it writes there
}

// Result is one run of one scenario.
type Result struct {
	Name       string
	Mode       string // "model"
	Seed       int64
	Rows       []gate.Row
	Findings   []string // what the product got wrong, found by the scenario
	NotChecked []string // what the model cannot check
	Elapsed    time.Duration
}

// Scenario is a named scenario with its model run and its live description.
type Scenario struct {
	Name  string
	Title string
	Run   func(ctx context.Context, env Env) Result
	Live  Live
}

// Live describes the scenario on the Docker stand: what runner starts and what it needs.
type Live struct {
	Command  string   // the runner command
	Services []string // compose services it starts
	Needs    []string // what the stand or the efv2 CLI must have for it
}

// Registry is the seven scenarios in the order of the task.
func Registry() []Scenario { return nil }

// Verdict is the worst status of the rows: red, else not measured, else yellow, else green. A
// result with no rows is not measured.
func (r Result) Verdict() gate.Status { return gate.NotMeasured }

// RunMany runs a scenario n times in a row and returns the gate's reading: the worst status each
// row had over the runs. Runs counts the runs made, which stops at the first red run.
func RunMany(ctx context.Context, s Scenario, env Env, n int) (gate.ScenarioResult, []Result) {
	return gate.ScenarioResult{}, nil
}
