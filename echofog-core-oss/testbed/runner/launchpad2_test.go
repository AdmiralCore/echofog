package main

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/probe"
)

func TestGateFindingsAreTheTwoSidedRulesNotRecallOne(t *testing.T) {
	ok := map[string]censor.DetectorMetric{
		"D1": {Recall: f64(1)}, "D5": {Recall: f64(0.95), FPR: 0.004},
		"D6": {AUC: f64(0.97), Positives: 100}, // a naive proxy must be found: that is D6's own test
		"D9": {FPR: 1, AUC: f64(0.5)},          // outside the gate
	}
	if got := gateFindings(ok, censor.V2GateResult{}); len(got) != 0 {
		t.Errorf("a run inside the gate has findings: %v", got)
	}

	bad := map[string]censor.DetectorMetric{
		"D3": {Recall: f64(0.9)},                // a structural control must be found every time
		"D5": {Recall: f64(0.5)},                // below the 90% of the synthetic side
		"D4": {FPR: 0.03},                       // browsers must not be flagged
		"D8": {Recall: f64(0.5), Positives: 10}, // recall of D8 is reported, not gated
	}
	got := strings.Join(gateFindings(bad, censor.V2GateResult{Failures: []string{"D5 flags 100% of the v2 flows"}}), "\n")
	for _, want := range []string{"D3", "D5 recall", "D4 false-positive", "D5 flags 100% of the v2 flows"} {
		if !strings.Contains(got, want) {
			t.Errorf("findings lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "D8 recall") {
		t.Errorf("D8 recall is not a gate rule:\n%s", got)
	}
}

func TestCoverageHasRowsForTheNewDetectors(t *testing.T) {
	metrics := map[string]censor.DetectorMetric{
		"D5": {Recall: f64(1), Positives: 10, N: 50},
		"D6": {AUC: f64(0.98), Positives: 10, N: 50},
		"D7": {Recall: f64(0.9), Positives: 10, N: 50},
		"D8": {Recall: f64(1), Positives: 10, N: 50},
		"D9": {AUC: f64(0.9), TPRAt1: f64(0.6), Positives: 10, N: 50, Recall: f64(0.5)},
	}
	rows := map[string]coverageRow{}
	for _, r := range coverageFor(metrics, carrierInfo{}) {
		rows[r.Detector] = r
	}
	for _, id := range []string{"D5", "D6", "D7", "D8", "D9"} {
		r, ok := rows[id]
		if !ok {
			t.Errorf("no coverage row for %s", id)
			continue
		}
		if r.Verdict == "" || r.Verdict == "no data" {
			t.Errorf("%s: verdict %q", id, r.Verdict)
		}
	}
	if !strings.Contains(rows["D9"].Verdict, "outside the gate") {
		t.Errorf("D9 verdict must say it is outside the gate: %q", rows["D9"].Verdict)
	}
	if !strings.Contains(rows["D9"].Note, "TPR") {
		t.Errorf("D9 note must carry TPR@FPR: %q", rows["D9"].Note)
	}
}

func TestFlowsCSVHasAColumnForEveryDetector(t *testing.T) {
	var b bytes.Buffer
	flows := []censor.LabeledFlow{{Flow: censor.Flow{Proto: "tcp"}, Label: censor.LabelBrowser,
		Flags: map[string]bool{"D5": true, "D9": false}}}
	if err := writeFlowsCSV(&b, flows, 10); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	header := strings.Split(lines[0], ",")
	idx := map[string]int{}
	for i, h := range header {
		idx[h] = i
	}
	for _, id := range []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9", "d10"} {
		if _, ok := idx[id]; !ok {
			t.Errorf("flows.csv has no %s column: %s", id, lines[0])
		}
	}
	row := strings.Split(lines[1], ",")
	if len(row) != len(header) {
		t.Fatalf("row has %d fields, header %d", len(row), len(header))
	}
	if row[idx["d5"]] != "1" || row[idx["d9"]] != "0" || row[idx["d6"]] != "-" {
		t.Errorf("d5/d9/d6 = %q/%q/%q", row[idx["d5"]], row[idx["d9"]], row[idx["d6"]])
	}
}

func TestH1aFlowsTakesTheRealExitAndTheBrowserControl(t *testing.T) {
	rec := []censor.Record{{Len: 100}}
	mk := func(label censor.Label, synth bool, proto string, recs []censor.Record) censor.LabeledFlow {
		return censor.LabeledFlow{Flow: censor.Flow{Proto: proto, Records: recs,
			Client: netip.MustParseAddrPort("203.0.113.21:1")}, Label: label, Synthetic: synth}
	}
	flows := []censor.LabeledFlow{
		mk(censor.LabelCarrier, false, "tcp", rec), // a real exit flow
		mk(censor.LabelCarrier, true, "tcp", rec),  // a control
		mk(censor.LabelCarrier, false, "udp", nil), // noise3: nothing to read
		mk(censor.LabelBrowser, false, "tcp", rec),
		mk(censor.LabelTool, false, "tcp", rec),
	}
	exit, browser := h1aFlows(flows)
	if len(exit) != 1 || len(browser) != 1 {
		t.Errorf("exit %d, browser %d; want 1 and 1", len(exit), len(browser))
	}
}

func TestReportCarriesTheGateH1aAndProbes(t *testing.T) {
	gate := censor.V2GateResult{Rows: []censor.V2GateRow{{Detector: "D5", N: 3, Measured: true, Pass: true}}}
	h1a := censor.H1aResult{KS: 0.1, Threshold: 0.2, Flows: 3, Pass: true}
	probes := []probe.Report{{Target: "ordinary", Outcomes: []probe.Outcome{{Detector: "D11", Case: "at once"}}}}
	r := buildReport(reportInput{Scenario: "v2-censor", V2Gate: &gate, H1a: &h1a, Probes: probes})
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"v2_gate"`, `"h1a"`, `"probes"`, `"ordinary"`, `"D11"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report lacks %s", want)
		}
	}
	plain, _ := json.Marshal(buildReport(reportInput{Scenario: "v2-basic"}))
	if strings.Contains(string(plain), `"v2_gate"`) || strings.Contains(string(plain), `"probes"`) {
		t.Errorf("a run without them must not carry empty sections: %s", plain)
	}
}

func TestProbeFindingsNameTheLeakAndSkipTheExpected(t *testing.T) {
	reps := []probe.Report{{Target: "hidden", Outcomes: []probe.Outcome{
		{Detector: "D12", Case: "garbage", Own: true, Node: probe.Answer{Class: "alert"}, Cover: probe.Answer{Class: "closed"}},
		{Detector: "D15", Case: "valid token", Own: true, Expected: true},
		{Detector: "D11", Case: "at once"},
	}}}
	got := probeFindings(reps)
	if len(got) != 1 {
		t.Fatalf("want one finding, got %v", got)
	}
	for _, want := range []string{"hidden", "D12", "garbage", "alert", "closed"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("finding lacks %q: %s", want, got[0])
		}
	}
	if len(probeFindings(nil)) != 0 {
		t.Error("no probes, no findings")
	}
}

func TestProbePhaseNeedsASpecBecauseTheLabKnowsNoV2Node(t *testing.T) {
	oldPhases, oldSpec := *v2Phases, *v2ProbeSpec
	defer func() { *v2Phases, *v2ProbeSpec = oldPhases, oldSpec }()
	*v2Phases, *v2ProbeSpec = "censor,probe", ""
	if _, err := v2PhaseList("v2-censor"); err == nil {
		t.Error("the probe phase without a spec must be refused before Docker is touched")
	}
	*v2ProbeSpec = "nodes.json"
	got, err := v2PhaseList("v2-censor")
	if err != nil || len(got) != 2 || got[1] != "probe" {
		t.Errorf("phases %v, err %v", got, err)
	}
}
