package main

import (
	"fmt"

	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/probe"
)

// structuralControls are the detectors, besides D1 and D2 which censor.GateFailures reads, whose
// control flows are structural (a hosted-name mismatch, a library fingerprint, a noise datagram):
// every one of them has to be found.
var structuralControls = []string{"D3", "D4", "D10"}

// gateFindings turns the metrics of a run and the v2 side of the gate into the findings that make
// the run red: the synthetic side (FPR, control recall, nested-TLS recall) and the v2 side
// (share flagged, D6 AUC). D6's own AUC on a naive proxy is its test and has to be high, so it is
// not read here; D7, D8 recall is reported but not gated; D9 is outside the gate.
func gateFindings(metrics map[string]censor.DetectorMetric, v2 censor.V2GateResult) []string {
	out := censor.GateFailures(metrics)
	for _, id := range structuralControls {
		if m, ok := metrics[id]; ok && m.Recall != nil && *m.Recall < 1 {
			out = append(out, fmt.Sprintf("%s recall %.2f on its control flows (want 1)", id, *m.Recall))
		}
	}
	return append(out, v2.Failures...)
}

// h1aFlows picks the flows H1a compares: the real v2 exit flows (carrier, not synthetic, TCP with
// records) and the browser control.
func h1aFlows(flows []censor.LabeledFlow) (exit, browser []censor.Flow) {
	for _, f := range flows {
		if f.Proto != "tcp" || len(f.Records) == 0 {
			continue
		}
		switch {
		case f.Label == censor.LabelCarrier && !f.Synthetic:
			exit = append(exit, f.Flow)
		case f.Label == censor.LabelBrowser:
			browser = append(browser, f.Flow)
		}
	}
	return exit, browser
}

// probeFindings lists the answers of a node's own that nobody expected.
func probeFindings(reps []probe.Report) []string {
	var out []string
	for _, r := range reps {
		for _, o := range r.Outcomes {
			if !o.Own || o.Expected || o.Skipped != "" {
				continue
			}
			out = append(out, fmt.Sprintf("%s: %s/%s got an answer of the node's own (%s %s) where the cover gives %s %s",
				r.Target, o.Detector, o.Case, o.Node.Class, o.Node.Detail, o.Cover.Class, o.Cover.Detail))
		}
	}
	return out
}
