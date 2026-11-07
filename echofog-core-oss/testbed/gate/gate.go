// Package gate is the launchpad's threshold gate and regression check (T15.0e). It reads the
// report.json of a run (and, when there is one, the report of the behaviour module and a baseline
// signed with a date and a commit), puts every metric in green, yellow or red against the
// thresholds of the design, compares the run with the baseline and builds the M4 checklist from the
// numbers, not by hand. A red anywhere blocks the release.
//
// The thresholds are the design's (03-launchpad-v2.md §5a, the decisions of H1a, H7, H11–H16); the
// yellow bands are a proposal that the M4 audit signs. Nothing here runs a stand.
package gate

import (
	"time"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

// Status is where a metric stands.
type Status int

const (
	Green Status = iota
	Yellow
	Red
	NotMeasured
	Info
)

func (s Status) String() string {
	switch s {
	case Green:
		return "green"
	case Yellow:
		return "yellow"
	case Red:
		return "red"
	case NotMeasured:
		return "not measured"
	case Info:
		return "info"
	}
	return "?"
}

// Row is one metric against its threshold.
type Row struct {
	ID     string // D5, D24, regression, ...
	Metric string
	Value  string
	Limit  string
	Status Status
	Source string // where the limit comes from
	Note   string
}

// DetectorEntry is one detector's row of report.json.
type DetectorEntry struct {
	Recall    *float64 `json:"recall"`
	FPR       float64  `json:"fpr"`
	N         int      `json:"n"`
	Positives int      `json:"positives"`
	// BrowserNegatives is nil in a report that does not say; 0 means there was no browser control.
	BrowserNegatives *int     `json:"browser_negatives"`
	AUC              *float64 `json:"auc"`
	TPRAt1           *float64 `json:"tpr_at_1pct_fpr"`
}

// RunReport is the part of the runner's report.json the gate reads.
type RunReport struct {
	Schema    int                      `json:"schema"`
	When      string                   `json:"when"`
	Commit    string                   `json:"commit"`
	Spec      string                   `json:"spec"`
	Scenario  string                   `json:"scenario"`
	Profile   string                   `json:"profile"`
	Network   string                   `json:"network"`
	Detectors map[string]DetectorEntry `json:"detectors"`
	Aggregate struct {
		VisibilityPct       float64 `json:"visibility_pct"`
		StrictVisibilityPct float64 `json:"strict_visibility_pct"`
	} `json:"aggregate"`
	V2Gate *struct {
		Rows []struct {
			Detector string   `json:"detector"`
			N        int      `json:"n"`
			Flagged  int      `json:"flagged"`
			Share    float64  `json:"share"`
			AUC      *float64 `json:"auc"`
			Measured bool     `json:"measured"`
			Pass     bool     `json:"pass"`
		} `json:"rows"`
	} `json:"v2_gate"`
	H1a *struct {
		KS        float64 `json:"ks"`
		Threshold float64 `json:"threshold"`
		Flows     int     `json:"flows"`
		Pass      bool    `json:"pass"`
	} `json:"h1a"`
	Probes []struct {
		Target   string `json:"target"`
		Outcomes []struct {
			Detector string `json:"detector"`
			Case     string `json:"case"`
			Own      bool   `json:"own"`
			Expected bool   `json:"expected"`
			Skipped  string `json:"skipped"`
		} `json:"outcomes"`
	} `json:"probes"`
	Findings []string `json:"findings"`
}

// Baseline is a run kept as the reference of the regression check, signed with a date and a commit.
type Baseline struct {
	SignedAt time.Time          `json:"signed_at"`
	Commit   string             `json:"commit"`
	Run      RunReport          `json:"run"`
	Speed    map[string]float64 `json:"speed_mbps,omitempty"` // carrier -> Mbit/s of `efv2 echo` (T0.3, AD21)
}

// Change is one metric that moved between the baseline and the run.
type Change struct {
	Key    string
	Before string
	After  string
	Status Status
	Why    string
}

// Item is a line of the M4 checklist.
type Item struct {
	Text     string
	Status   Status
	Evidence string
}

// Input is what the gate is given.
type Input struct {
	Run       *RunReport
	Behaviour *behaviour.Report
	Baseline  *Baseline
	Speed     map[string]float64 // this run's `efv2 echo` Mbit/s per carrier; nil when efv2 did not run
	Now       time.Time
	Commit    string
}

// Result is the verdict of a gate run.
type Result struct {
	SignedAt  time.Time
	Commit    string
	Rows      []Row
	Changes   []Change
	Waiting   []Row // red gates that wait for a fix of the product, not of the lab
	Checklist []Item
	Verdict   Status
	Blocks    bool
}
