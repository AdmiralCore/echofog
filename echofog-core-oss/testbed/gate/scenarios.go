package gate

import (
	"errors"
	"io"
)

// RequiredScenarios are the scenarios of T15.1. Each has to be measured twice: in the model (the
// product's own code against a model of the network, no containers) and on the live stand.
var RequiredScenarios = []string{"v2-nat", "v2-paths", "v2-hidden", "v2-messaging", "v2-call", "v2-exit", "v2-censor"}

// ScenariosNeeded is how many runs in a row have to be green.
const ScenariosNeeded = 3

// ScenarioResult is what a scenario measured, in one mode, over Runs runs in a row. Rows holds the
// worst status each row had over the runs.
type ScenarioResult struct {
	Name       string   `json:"name"`
	Mode       string   `json:"mode"` // "model" or "live"
	Runs       int      `json:"runs"`
	Rows       []Row    `json:"rows"`
	Findings   []string `json:"findings,omitempty"`
	NotChecked []string `json:"not_checked,omitempty"`
}

// ScenarioReport is the file a scenario run writes and the gate reads.
type ScenarioReport struct {
	Schema    int              `json:"schema"`
	Commit    string           `json:"commit,omitempty"`
	Scenarios []ScenarioResult `json:"scenarios"`
}

// LoadScenarios reads a scenario report.
func LoadScenarios(r io.Reader) (*ScenarioReport, error) { return nil, errors.New("not implemented") }

// scenarioRows is the gate's reading of the scenarios.
func scenarioRows(list []ScenarioResult) []Row { return nil }

// UnmarshalText reads the word MarshalText wrote.
func (s *Status) UnmarshalText(b []byte) error { return nil }
