package main

import (
	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/probe"
)

// gateFindings turns the metrics of a run and the v2 side of the gate into the findings that make
// the run red: the synthetic side (FPR, control recall, nested-TLS recall) and the v2 side
// (share flagged, D6 AUC). D9 is not read.
func gateFindings(metrics map[string]censor.DetectorMetric, v2 censor.V2GateResult) []string {
	return []string{"stub"}
}

// h1aFlows picks the flows H1a compares: the real v2 exit flows (carrier, not synthetic, TCP with
// records) and the browser control.
func h1aFlows(flows []censor.LabeledFlow) (exit, browser []censor.Flow) { return nil, nil }

// probeFindings lists the answers of a node's own that nobody expected.
func probeFindings(reps []probe.Report) []string { return []string{"stub"} }
