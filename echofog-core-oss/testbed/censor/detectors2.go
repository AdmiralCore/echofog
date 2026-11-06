package censor

import "time"

// Scorer is a detector that ranks flows instead of only flagging them: D6 and D9 report AUC and
// TPR at 1% FPR, and D7 keeps its distance.
type Scorer interface {
	Score(LabeledFlow, DetectorConfig) float64
}

// A Contextual detector needs the whole run before it can judge one flow: D8 counts connections
// to an address, D9 trains on the run's own flows.
type Contextual interface {
	Begin(flows []LabeledFlow, cfg DetectorConfig)
}

// ---------------------------------------------------------------------------- D5

type tlsInTLSDetector struct{}

func (tlsInTLSDetector) ID() string   { return "D5" }
func (tlsInTLSDetector) Name() string { return "TLS-in-TLS (an inner handshake in the first records)" }
func (tlsInTLSDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "tcp" && len(f.Records) >= 6
}
func (tlsInTLSDetector) Truth(f LabeledFlow, _ DetectorConfig) bool { return f.Suite == SuiteTLSInTLS }
func (tlsInTLSDetector) Flag(LabeledFlow, DetectorConfig) bool      { return false }

// ---------------------------------------------------------------------------- D6

type crossLayerRTTDetector struct{}

func (crossLayerRTTDetector) ID() string { return "D6" }
func (crossLayerRTTDetector) Name() string {
	return "cross-layer RTT (answer latency against transport ACK latency)"
}
func (crossLayerRTTDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "tcp" && len(f.Pkts) >= 12
}
func (crossLayerRTTDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == SuiteTLSInTLS
}
func (crossLayerRTTDetector) Score(LabeledFlow, DetectorConfig) float64 { return 0 }
func (crossLayerRTTDetector) Flag(LabeledFlow, DetectorConfig) bool     { return false }

// ---------------------------------------------------------------------------- D7

type sizeTimingDetector struct{}

func (*sizeTimingDetector) ID() string { return "D7" }
func (*sizeTimingDetector) Name() string {
	return "record sizes and timings (KS distance to the browser control)"
}
func (*sizeTimingDetector) Applies(f LabeledFlow) bool { return len(f.Records) >= 6 }
func (*sizeTimingDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == SuiteTLSInTLS || f.Suite == SuiteConstSize
}
func (*sizeTimingDetector) Score(LabeledFlow, DetectorConfig) float64 { return 0 }
func (*sizeTimingDetector) Flag(LabeledFlow, DetectorConfig) bool     { return false }

// ---------------------------------------------------------------------------- D8

type longHeavyDetector struct{}

func (*longHeavyDetector) ID() string { return "D8" }
func (*longHeavyDetector) Name() string {
	return "long and heavy flows, connection count to one address, periodicity"
}
func (*longHeavyDetector) Applies(f LabeledFlow) bool { return f.Proto == "tcp" || f.Proto == "udp" }
func (*longHeavyDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == SuiteBulkTunnel || f.Suite == SuiteConnStorm
}
func (*longHeavyDetector) Begin([]LabeledFlow, DetectorConfig)   {}
func (*longHeavyDetector) Flag(LabeledFlow, DetectorConfig) bool { return false }

// ---------------------------------------------------------------------------- D9

type flowPhysicsDetector struct{}

func (*flowPhysicsDetector) ID() string { return "D9" }
func (*flowPhysicsDetector) Name() string {
	return "ML flow physics (random forest on six features; outside the gate)"
}
func (*flowPhysicsDetector) Applies(f LabeledFlow) bool { return len(f.Pkts) >= 12 }
func (*flowPhysicsDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return isTunnelControl(f)
}
func (*flowPhysicsDetector) Begin([]LabeledFlow, DetectorConfig)       {}
func (*flowPhysicsDetector) Score(LabeledFlow, DetectorConfig) float64 { return 0 }
func (*flowPhysicsDetector) Flag(LabeledFlow, DetectorConfig) bool     { return false }

// Suites the scenarios give to synthetic positive controls. A real v2 carrier has its own suite
// name and is never a positive: its exposure is what the gate measures.
const (
	SuiteTLSInTLS   = "tls-in-tls"  // a proxy that runs a second TLS inside the first
	SuiteConstSize  = "const-size"  // a tunnel with fixed-size records at a fixed pace
	SuiteBulkTunnel = "bulk-tunnel" // one long, heavy connection
	SuiteConnStorm  = "conn-storm"  // many connections to one address in a short time
)

func isTunnelControl(f LabeledFlow) bool {
	switch f.Suite {
	case SuiteTLSInTLS, SuiteConstSize, SuiteBulkTunnel, SuiteConnStorm:
		return true
	}
	return false
}

// Thresholds of D8 when DetectorConfig leaves them zero.
const (
	defaultLongMin       = 2 * time.Minute
	defaultLongBytes     = 20 << 20
	defaultMaxConnsPerIP = 20
)

// Exposure says how many of the flows with the label (and not the synthetic positive controls)
// the detector flags: the share that matters for the v2 gate, where a flagged v2 flow is a leak.
func Exposure(flows []LabeledFlow, label Label, id string) (flagged, n int) { return 0, 0 }

// FinalVerdict gives a flow exactly one verdict: the most severe of its detector flags, else the
// plain verdict of Flow.Verdict. The same flow always gets the same string.
func FinalVerdict(f LabeledFlow) string { return "" }

// GateFailures lists the two-sided rules of the gate that the metrics break. D6's AUC must come
// from the v2 flows against the browser control, not from the synthetic proxy. D9 is not in it.
func GateFailures(m map[string]DetectorMetric) []string { return []string{"stub"} }

func (*sizeTimingDetector) Begin([]LabeledFlow, DetectorConfig) {}

// V2GateRow is one detector on the real v2 flows: the share of them it flags (the gate wants at
// most 10%), or for D6 the AUC against the browser control (at most 0.65).
type V2GateRow struct {
	Detector string   `json:"detector"`
	N        int      `json:"n"`
	Flagged  int      `json:"flagged"`
	Share    float64  `json:"share"`
	AUC      *float64 `json:"auc,omitempty"`
	Measured bool     `json:"measured"` // false: no v2 flow was in the detector's scope, so nothing was shown
	Pass     bool     `json:"pass"`
}

// V2GateResult is the v2 side of the two-sided gate H1a.
type V2GateResult struct {
	Rows     []V2GateRow `json:"rows"`
	Failures []string    `json:"failures,omitempty"`
}

// V2Gate evaluates the flows (after EvaluateDetectors) against the v2 side of the gate: of the
// real v2 flows (labelled carrier, not synthetic) D5, D7 and D8 may flag at most 10%, D6's AUC
// against the browser control must be at most 0.65. A detector with no v2 flow in its scope is
// reported as not measured, never as a pass.
func V2Gate(flows []LabeledFlow, cfg DetectorConfig) V2GateResult { return V2GateResult{} }
