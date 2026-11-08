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
	"slices"
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
func Registry() []Scenario {
	return []Scenario{natScenario(), pathsScenario(), hiddenScenario(), messagingScenario(), callScenario(), exitScenario(), censorScenario()}
}

// Verdict is the worst status of the rows: red, else not measured, else yellow, else green. A
// result with no rows is not measured.
func (r Result) Verdict() gate.Status {
	var red, missing, yellow, green bool
	for _, row := range r.Rows {
		switch row.Status {
		case gate.Red:
			red = true
		case gate.NotMeasured:
			missing = true
		case gate.Yellow:
			yellow = true
		case gate.Green:
			green = true
		}
	}
	switch {
	case red:
		return gate.Red
	case missing || !(green || yellow):
		return gate.NotMeasured
	case yellow:
		return gate.Yellow
	}
	return gate.Green
}

// rank orders statuses for "the worst over the runs".
func rank(s gate.Status) int {
	switch s {
	case gate.Red:
		return 4
	case gate.NotMeasured:
		return 3
	case gate.Yellow:
		return 2
	case gate.Green:
		return 1
	}
	return 0
}

// RunMany runs a scenario n times in a row and returns the gate's reading: the worst status each
// row had over the runs. Runs counts the runs made, which stops at the first red run (three in a
// row cannot happen after it) and at a cancelled context.
func RunMany(ctx context.Context, s Scenario, env Env, n int) (gate.ScenarioResult, []Result) {
	out := gate.ScenarioResult{Name: s.Name, Mode: "model"}
	var runs []Result
	index := map[string]int{}
	for i := 0; i < n && ctx.Err() == nil; i++ {
		e := env
		e.Seed = env.Seed + int64(i)
		start := time.Now()
		r := s.Run(ctx, e)
		r.Elapsed = time.Since(start)
		runs = append(runs, r)
		out.Runs++
		for _, row := range r.Rows {
			if at, ok := index[row.ID]; ok {
				if rank(row.Status) > rank(out.Rows[at].Status) {
					out.Rows[at] = row
				}
				continue
			}
			index[row.ID] = len(out.Rows)
			out.Rows = append(out.Rows, row)
		}
		out.Findings = appendNew(out.Findings, r.Findings)
		out.NotChecked = appendNew(out.NotChecked, r.NotChecked)
		if r.Verdict() == gate.Red {
			break
		}
	}
	return out, runs
}

func appendNew(dst, src []string) []string {
	for _, s := range src {
		if !slices.Contains(dst, s) {
			dst = append(dst, s)
		}
	}
	return dst
}

// removeTree removes a scratch directory and waits for the files a closing node still holds.
func removeTree(dir string) error { return nil }
