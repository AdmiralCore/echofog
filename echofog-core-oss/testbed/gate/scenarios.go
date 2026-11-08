package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// RequiredScenarios are the scenarios of T15.1. Each has to be measured twice: in the model (the
// product's own code against a model of the network, no containers) and on the live stand.
var RequiredScenarios = []string{"v2-nat", "v2-paths", "v2-hidden", "v2-messaging", "v2-call", "v2-exit", "v2-censor"}

// ScenariosNeeded is how many runs in a row have to be green.
const ScenariosNeeded = 3

const (
	srcScen   = "T15.1 scenarios (architect: green three runs in a row)"
	liveAudit = "the live stand is not run before the audit phase (architect's decision); this is not a defect of the scenario"
)

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

// LoadScenarios reads a scenario report and refuses what the gate cannot read honestly.
func LoadScenarios(r io.Reader) (*ScenarioReport, error) {
	var rep ScenarioReport
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return nil, fmt.Errorf("scenarios: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("scenarios: data after the first JSON value")
	}
	if len(rep.Scenarios) == 0 {
		return nil, errors.New("scenarios: the report holds no scenario")
	}
	for _, s := range rep.Scenarios {
		if !slices.Contains(RequiredScenarios, s.Name) {
			return nil, fmt.Errorf("scenarios: unknown scenario %q", s.Name)
		}
		if s.Mode != "model" && s.Mode != "live" {
			return nil, fmt.Errorf("scenarios: %s: mode %q is neither model nor live", s.Name, s.Mode)
		}
		if s.Runs < 0 {
			return nil, fmt.Errorf("scenarios: %s: %d runs", s.Name, s.Runs)
		}
	}
	return &rep, nil
}

// scenarioRows is the gate's reading of the scenarios: every row a scenario measured, and per
// scenario and mode whether three runs in a row were green. A scenario or a mode that was not run
// is not measured, never green.
func scenarioRows(list []ScenarioResult) []Row {
	if len(list) == 0 {
		return []Row{{ID: "scenarios", Metric: "T15.1", Value: "no scenario report", Limit: "all scenarios green three runs in a row",
			Status: NotMeasured, Source: srcScen}}
	}
	var rows []Row
	for _, name := range RequiredScenarios {
		for _, mode := range []string{"model", "live"} {
			metric := mode + " runs"
			if mode == "live" {
				metric = "live"
			}
			var res *ScenarioResult
			for i := range list {
				if list[i].Name == name && list[i].Mode == mode {
					res = &list[i]
				}
			}
			row := Row{ID: "scenario:" + name, Metric: metric, Limit: fmt.Sprintf("%d runs in a row, every row green", ScenariosNeeded), Source: srcScen}
			if res == nil {
				row.Value, row.Status = "not run", NotMeasured
				if mode == "live" {
					row.Note = liveAudit
				}
				rows = append(rows, row)
				continue
			}
			st := Green
			for _, r := range res.Rows {
				rr := r
				rr.ID = "scenario:" + name + "/" + r.ID
				if rr.Source == "" {
					rr.Source = srcScen
				}
				rows = append(rows, rr)
				switch {
				case r.Status == Red:
					st = Red
				case r.Status == NotMeasured && st != Red:
					st = NotMeasured
				case r.Status == Yellow && st == Green:
					st = Yellow
				}
			}
			row.Value = fmt.Sprintf("%d runs", res.Runs)
			switch {
			case st == Red:
			case res.Runs < ScenariosNeeded:
				st = NotMeasured
				row.Note = fmt.Sprintf("%d of %d runs", res.Runs, ScenariosNeeded)
			case len(res.Rows) == 0:
				st = NotMeasured
				row.Note = "the scenario measured nothing"
			}
			row.Status = st
			if mode == "live" && st == NotMeasured {
				row.Note += " " + liveAudit
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// UnmarshalText reads the word MarshalText wrote.
func (s *Status) UnmarshalText(b []byte) error {
	for _, c := range []Status{Green, Yellow, Red, NotMeasured, Info} {
		if c.String() == string(b) {
			*s = c
			return nil
		}
	}
	return fmt.Errorf("unknown status %q", b)
}
