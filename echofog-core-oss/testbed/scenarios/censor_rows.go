package scenarios

import (
	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/gate"
	"github.com/echofog/echofog-core/testbed/probe"
)

// classifierRow is the row v2-censor/<name>/classifier: the detectors that have the carrier's flows
// in scope, against the v2 side of the gate. The second result is a finding when the row is red.
func classifierRow(name string, carrierFlows, browserFlows []censor.Flow, cfg censor.DetectorConfig) (gate.Row, string) {
	return newRow("v2-censor/"+name+"/classifier", "classifier", "", "", gate.NotMeasured, "not written"), ""
}

// probeRow is the row v2-censor/<name>/probe from the prober's report.
func probeRow(name string, rep probe.Report) (gate.Row, string) {
	return newRow("v2-censor/"+name+"/probe", "active probe", "", "", gate.NotMeasured, "not written"), ""
}

// controlRow is the row v2-censor/control: the browser control against the false-positive limit.
func controlRow(browserFlows []censor.Flow, cfg censor.DetectorConfig) gate.Row {
	return newRow("v2-censor/control", "browser control", "", "", gate.NotMeasured, "not written")
}
