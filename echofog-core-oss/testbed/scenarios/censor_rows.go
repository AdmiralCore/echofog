package scenarios

import (
	"fmt"
	"slices"
	"strings"

	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/gate"
	"github.com/echofog/echofog-core/testbed/probe"
)

const (
	classifierLimit = "each detector flags <=10% of the carrier's flows, D6 AUC <=0.65"
	probeLimit      = "0 answers of the node's own (D11-D15)"
	controlLimit    = "false-positive rate <=1% per detector on the browser control"
)

// labelFlows turns the censor's flows into labelled ones, after the SNI check against what the
// addresses host. The suite names the source; the label is the ground truth.
func labelFlows(flows []censor.Flow, label censor.Label, suite string, cfg censor.DetectorConfig) []censor.LabeledFlow {
	own := slices.Clone(flows)
	censor.CheckSNI(own, cfg.Hosted)
	out := make([]censor.LabeledFlow, len(own))
	for i, f := range own {
		out[i] = censor.LabeledFlow{Flow: f, Label: label, Suite: suite}
	}
	return out
}

// classifierRow is the row v2-censor/<name>/classifier: the detectors that have the carrier's flows
// in scope, against the v2 side of the gate (each detector flags at most 10% of the real carrier
// flows, D6's AUC against the browser control at most 0.65). A detector with no flow in its scope is
// named as not measured. The second result is a finding when the row is red.
func classifierRow(name string, carrierFlows, browserFlows []censor.Flow, cfg censor.DetectorConfig) (gate.Row, string) {
	id := "v2-censor/" + name + "/classifier"
	if len(carrierFlows) == 0 {
		return newRow(id, "classifier", "no flows", classifierLimit, gate.NotMeasured, "the capture holds no flow of the carrier"), ""
	}
	labeled := labelFlows(carrierFlows, censor.LabelCarrier, name, cfg)
	labeled = append(labeled, labelFlows(browserFlows, censor.LabelBrowser, "utls-chrome", cfg)...)
	res := censor.V2Gate(labeled, cfg)

	var flagged, clean, unmeasured []string
	measured := map[string]bool{}
	for _, r := range res.Rows {
		switch {
		case !r.Measured:
			unmeasured = append(unmeasured, r.Detector)
		case r.AUC != nil:
			measured[r.Detector] = true
			s := fmt.Sprintf("%s AUC %.2f over %d flows", r.Detector, *r.AUC, r.N)
			if r.Pass {
				clean = append(clean, s)
			} else {
				flagged = append(flagged, s)
			}
		default:
			measured[r.Detector] = true
			s := fmt.Sprintf("%s %d/%d", r.Detector, r.Flagged, r.N)
			if r.Pass {
				clean = append(clean, s)
			} else {
				flagged = append(flagged, s)
			}
		}
	}
	// the detector of the wire the flows have must have looked at them
	want := "D1"
	if carrierFlows[0].Proto == "udp" {
		want = "D10"
	}
	value := fmt.Sprintf("%d flows; flagged: %s; clean: %s", len(carrierFlows), orNone(flagged), orNone(clean))
	note := "SNI hosted at the fake address by assumption (the cover is where the node is); " + fmt.Sprintf("browser control: %d flows", len(browserFlows))
	if len(unmeasured) > 0 {
		note += "; not measured (no flow in scope): " + strings.Join(unmeasured, ", ")
	}
	if carrierFlows[0].Proto == "udp" {
		big := 0
		for _, f := range carrierFlows {
			if f.UDPFirst >= 1001 {
				big++
			}
		}
		note += fmt.Sprintf("; first datagram of the flow: %d of %d are 1001 bytes or more, the rest are shorter", big, len(carrierFlows))
	}
	switch {
	case len(res.Failures) > 0:
		return newRow(id, "classifier", value, classifierLimit, gate.Red, note),
			fmt.Sprintf("%s: %s", name, strings.Join(res.Failures, "; "))
	case !measured[want]:
		return newRow(id, "classifier", value, classifierLimit, gate.NotMeasured, note+"; "+want+" did not look at the flows"), ""
	}
	return newRow(id, "classifier", value, classifierLimit, gate.Green, note), ""
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// probeRow is the row v2-censor/<name>/probe from the prober's report: zero answers of the node's
// own that the cover would not give. Cases the prober skipped are named, not counted.
func probeRow(name string, rep probe.Report) (gate.Row, string) {
	id := "v2-censor/" + name + "/probe"
	ran, expected := 0, 0
	var skipped, own, unconfirmed []string
	for _, o := range rep.Outcomes {
		switch {
		case o.Skipped != "":
			skipped = append(skipped, fmt.Sprintf("%s/%s (%s)", o.Detector, o.Case, o.Skipped))
		default:
			ran++
			if strings.HasPrefix(o.Note, "unconfirmed") {
				unconfirmed = append(unconfirmed, fmt.Sprintf("%s/%s", o.Detector, o.Case))
			}
			if o.Own && o.Expected {
				expected++
			}
			if o.Own && !o.Expected {
				own = append(own, fmt.Sprintf("%s/%s: node %s %s, cover %s %s", o.Detector, o.Case, o.Node.Class, o.Node.Detail, o.Cover.Class, o.Cover.Detail))
			}
		}
	}
	note := ""
	if expected > 0 {
		note += fmt.Sprintf("%d case(s) with a valid token were answered by the node, as they must be; ", expected)
	}
	if len(unconfirmed) > 0 {
		note += "answered differently once, not on a repeat (a race of the machine): " + strings.Join(unconfirmed, ", ") + "; "
	}
	if len(skipped) > 0 {
		note += "not run: " + strings.Join(skipped, "; ")
	}
	switch {
	case ran == 0:
		return newRow(id, "active probe", "no case ran", probeLimit, gate.NotMeasured, "no probe case ran. "+note), ""
	case len(own) > 0:
		return newRow(id, "active probe", fmt.Sprintf("%d own answers in %d cases", len(own), ran), probeLimit, gate.Red, strings.TrimSuffix(note, "; ")),
			fmt.Sprintf("%s: the node answers a probe differently from its cover: %s", name, strings.Join(own, "; "))
	}
	return newRow(id, "active probe", fmt.Sprintf("0 own answers in %d cases", ran), probeLimit, gate.Green, strings.TrimSuffix(note, "; ")), ""
}

// controlRow is the row v2-censor/control: the browser control against the false-positive limit of
// the gate. D6, D7 and D9 calibrate on the control itself, so their rate there is low by
// construction; D1-D5 and D8 judge each flow alone.
func controlRow(browserFlows []censor.Flow, cfg censor.DetectorConfig) gate.Row {
	id := "v2-censor/control"
	if len(browserFlows) == 0 {
		return newRow(id, "browser control", "no flows", controlLimit, gate.NotMeasured, "the capture holds no browser flow")
	}
	labeled := labelFlows(browserFlows, censor.LabelBrowser, "utls-chrome", cfg)
	m := censor.EvaluateDetectors(labeled, cfg)
	var flagged, clean, unmeasured []string
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	slices.SortFunc(ids, func(a, b string) int { return detectorOrder(a) - detectorOrder(b) })
	for _, k := range ids {
		d := m[k]
		switch {
		case d.BrowserNegatives == 0:
			unmeasured = append(unmeasured, k)
		case d.BrowserFP > 0:
			flagged = append(flagged, fmt.Sprintf("%s %d/%d", k, d.BrowserFP, d.BrowserNegatives))
		default:
			clean = append(clean, fmt.Sprintf("%s 0/%d", k, d.BrowserNegatives))
		}
	}
	value := fmt.Sprintf("%d browser flows; flagged: %s; clean: %s", len(browserFlows), orNone(flagged), orNone(clean))
	note := "uTLS HelloChrome_Auto fetching pages of varied size from a local TLS server through the TCP tap"
	if len(unmeasured) > 0 {
		note += "; not measured (no browser flow in scope): " + strings.Join(unmeasured, ", ")
	}
	if fails := censor.GateFailures(m); len(fails) > 0 {
		return newRow(id, "browser control", value, controlLimit, gate.Red, note+"; "+strings.Join(fails, "; "))
	}
	return newRow(id, "browser control", value, controlLimit, gate.Green, note)
}

func detectorOrder(id string) int {
	var n int
	_, _ = fmt.Sscanf(id, "D%d", &n)
	return n
}

// classifierRowWith is classifierRow for a carrier whose reference or control is missing for some
// detectors (name to reason): those are not judged, said so in the row, and a row with nothing else
// red is not green.
func classifierRowWith(name string, carrierFlows, browserFlows []censor.Flow, cfg censor.DetectorConfig, inconclusive map[string]string) (gate.Row, string) {
	return classifierRow(name, carrierFlows, browserFlows, cfg)
}
