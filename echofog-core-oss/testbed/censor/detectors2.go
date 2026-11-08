package censor

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Scorer is a detector that ranks flows instead of only flagging them: D6 and D9 report AUC and
// TPR at 1% FPR, and D7 keeps its distance.
type Scorer interface {
	Score(LabeledFlow, DetectorConfig) float64
}

// A Contextual detector needs the whole run before it can judge one flow: D6 and D7 calibrate on
// the browser control, D8 counts connections to an address, D9 trains on the flows of the run.
type Contextual interface {
	Begin(flows []LabeledFlow, cfg DetectorConfig)
}

// A Noter adds notes to its metric: the thresholds it calibrated, what it could not do.
type Noter interface {
	Notes() []string
}

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
	defaultConnWindow    = 10 * time.Second
)

// browserOf picks the flows a detector calibrates on: the browser control it applies to.
func browserOf(flows []LabeledFlow, d Detector) []LabeledFlow {
	var out []LabeledFlow
	for _, f := range flows {
		if f.Label == LabelBrowser && d.Applies(f) {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------- D5

type tlsInTLSDetector struct{}

func (tlsInTLSDetector) ID() string   { return "D5" }
func (tlsInTLSDetector) Name() string { return "TLS-in-TLS (an inner handshake in the first records)" }
func (tlsInTLSDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "tcp" && len(f.Records) >= 6
}
func (tlsInTLSDetector) Truth(f LabeledFlow, _ DetectorConfig) bool { return f.Suite == SuiteTLSInTLS }
func (tlsInTLSDetector) Flag(f LabeledFlow, _ DetectorConfig) bool  { return innerHandshake(f.Records) }

// innerHandshake looks for the shape of a second handshake riding in the first application data
// (the rule the TLS-in-TLS papers publish): after the outer Finished, the first thing the client
// sends is the size of a ClientHello plus overhead, the server answers with a flight of more than a
// kilobyte (ServerHello, certificate, Finished), and the client replies at once with about the size
// of a CCS and a Finished. A browser's first exchange has the first two, but what follows it is a
// small window update, or a request after the page has been read.
//
// "Thing" is a burst: the records one write produces arrive together, and a ClientHello that does
// not fit one TLS record is two records of one burst.
func innerHandshake(recs []Record) bool {
	i := -1
	for k, r := range recs {
		if r.Up && r.Type == 23 {
			i = k // the outer Finished
			break
		}
	}
	if i < 0 {
		return false
	}
	rest := recs[i+1:]
	a := -1
	for k, r := range rest {
		if r.Up {
			a = k
			break
		}
	}
	if a < 0 || rest[a].Type != 23 {
		return false
	}
	hello, k := 0, a
	for ; k < len(rest) && rest[k].Up && (k == a || rest[k].At-rest[k-1].At <= burstRecordGap); k++ {
		hello += rest[k].Len
	}
	if hello < 200 || hello > 2600 {
		return false
	}
	flight, last := 0, rest[k-1].At
	for ; k < len(rest) && !rest[k].Up; k++ {
		flight += rest[k].Len
		last = rest[k].At
	}
	if flight < 1200 || k >= len(rest) || rest[k].At-last > 20*time.Millisecond {
		return false
	}
	// the reply: the first records of the burst, up to the point they add up to a Finished
	reply := 0
	for j := k; j < len(rest) && rest[j].Up && (j == k || rest[j].At-rest[j-1].At <= burstRecordGap); j++ {
		reply += rest[j].Len
		if reply >= 70 {
			break
		}
	}
	return reply >= 70 && reply <= 160
}

// burstRecordGap is how close two records are to be one write.
const burstRecordGap = 3 * time.Millisecond

// ---------------------------------------------------------------------------- D6

type crossLayerRTTDetector struct{ thr float64 }

func (*crossLayerRTTDetector) ID() string { return "D6" }
func (*crossLayerRTTDetector) Name() string {
	return "cross-layer RTT (answer latency against transport ACK latency)"
}
func (*crossLayerRTTDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "tcp" && len(f.Pkts) >= 12
}
func (*crossLayerRTTDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == SuiteTLSInTLS
}

// Begin sets the threshold at the 99th percentile of the browser control; with no control it
// stays at 60 ms.
func (d *crossLayerRTTDetector) Begin(flows []LabeledFlow, _ DetectorConfig) {
	var scores []float64
	for _, f := range browserOf(flows, d) {
		scores = append(scores, d.Score(f, DetectorConfig{}))
	}
	d.thr = 60
	if len(scores) >= 20 {
		sort.Float64s(scores)
		d.thr = scores[int(math.Ceil(0.99*float64(len(scores))))-1]
	}
}

func (d *crossLayerRTTDetector) Score(f LabeledFlow, _ DetectorConfig) float64 {
	v, _ := crossLayerExtra(f.Pkts)
	return v
}

func (d *crossLayerRTTDetector) Flag(f LabeledFlow, cfg DetectorConfig) bool {
	thr := d.thr
	if thr == 0 {
		thr = 60
	}
	return d.Score(f, cfg) > thr
}

// burstGap is how close two upstream data packets are to be one write.
const burstGap = 2 * time.Millisecond

// crossLayerExtra is the median, in milliseconds, of how much later the first data packet of an
// answer arrives than the transport ACK of the request: the transport answers from the server's
// kernel, the application answers after it has done its work. For an origin server the gap is its
// think time; for a proxy it also holds the round trip to the real destination, which is the
// layer mismatch D6 reads. An exchange whose answer carries the ACK has no gap to read.
func crossLayerExtra(pk []Pkt) (float64, bool) {
	var extras []float64
	for i, p := range pk {
		if !p.Up || p.Payload == 0 {
			continue
		}
		if i > 0 && pk[i-1].Up && pk[i-1].Payload > 0 && p.At-pk[i-1].At < burstGap {
			continue // the rest of one burst
		}
		end := p.Seq + uint32(p.Payload)
		ackAt, dataAt := time.Duration(-1), time.Duration(-1)
		for j := i + 1; j < len(pk); j++ {
			q := pk[j]
			if q.Up {
				if q.Payload > 0 {
					break // the next request
				}
				continue
			}
			if q.Payload == 0 {
				if ackAt < 0 && int32(q.Ack-end) >= 0 {
					ackAt = q.At
				}
				continue
			}
			dataAt = q.At
			break
		}
		if ackAt >= 0 && dataAt >= ackAt {
			extras = append(extras, float64(dataAt-ackAt)/1e6)
		}
	}
	if len(extras) == 0 {
		return 0, false
	}
	sort.Float64s(extras)
	return extras[len(extras)/2], true
}

// ---------------------------------------------------------------------------- D7

type sizeTimingDetector struct {
	poolSize, poolGap []float64
	thrSize, thrGap   float64
	notes             []string
}

func (*sizeTimingDetector) ID() string { return "D7" }
func (*sizeTimingDetector) Name() string {
	return "record sizes and timings (KS distance to the browser control)"
}
func (*sizeTimingDetector) Applies(f LabeledFlow) bool { return len(f.Records) >= 6 }
func (*sizeTimingDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == SuiteTLSInTLS || f.Suite == SuiteConstSize
}

func (d *sizeTimingDetector) Begin(flows []LabeledFlow, cfg DetectorConfig) {
	d.notes = nil
	var sizes, gaps [][]float64
	for _, f := range browserOf(flows, d) {
		sizes = append(sizes, sortedCopy(recordSizes(f.Flow)))
		gaps = append(gaps, sortedCopy(recordGaps(f.Flow)))
	}
	d.poolSize, d.poolGap = sortedCopy(pooled(sizes)), sortedCopy(pooled(gaps))
	switch {
	case cfg.KSThreshold > 0:
		d.thrSize, d.thrGap = cfg.KSThreshold, cfg.KSThreshold
		d.notes = append(d.notes, fmt.Sprintf("KS threshold %.3f from the configuration", cfg.KSThreshold))
	case len(sizes) >= 10:
		// half of the false-positive budget for each of the two distances
		d.thrSize, d.thrGap = CalibrateKS(sizes, 0.005), CalibrateKS(gaps, 0.005)
		d.notes = append(d.notes, fmt.Sprintf("KS threshold %.3f (sizes) and %.3f (gaps), calibrated on %d browser flows of this run: a proposal from the control, not a fact", d.thrSize, d.thrGap, len(sizes)))
	default:
		d.thrSize, d.thrGap = 1, 1
		d.notes = append(d.notes, "KS threshold unavailable: fewer than 10 browser flows to calibrate on, so D7 flags nothing")
	}
}

func (d *sizeTimingDetector) Notes() []string { return d.notes }

func (d *sizeTimingDetector) distances(f LabeledFlow) (size, gap float64) {
	if len(d.poolSize) == 0 {
		return 0, 0
	}
	return KS(recordSizes(f.Flow), d.poolSize), KS(recordGaps(f.Flow), d.poolGap)
}

func (d *sizeTimingDetector) Score(f LabeledFlow, _ DetectorConfig) float64 {
	s, g := d.distances(f)
	return math.Max(s, g)
}

func (d *sizeTimingDetector) Flag(f LabeledFlow, _ DetectorConfig) bool {
	if d.thrSize == 0 && d.thrGap == 0 {
		return false
	}
	s, g := d.distances(f)
	return s > d.thrSize || g > d.thrGap
}

// ---------------------------------------------------------------------------- D8

type longHeavyDetector struct {
	storm map[int]bool
}

func (*longHeavyDetector) ID() string { return "D8" }
func (*longHeavyDetector) Name() string {
	return "long and heavy flows, many connections to one address"
}
func (*longHeavyDetector) Applies(f LabeledFlow) bool { return f.Proto == "tcp" || f.Proto == "udp" }
func (*longHeavyDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == SuiteBulkTunnel || f.Suite == SuiteConnStorm
}

// Begin finds the storms: more than MaxConnsPerIP connections from one client to one server
// address that start inside one ConnWindow. Every flow of such a window is part of it.
func (d *longHeavyDetector) Begin(flows []LabeledFlow, cfg DetectorConfig) {
	maxConns, window := cfg.MaxConnsPerIP, cfg.ConnWindow
	if maxConns == 0 {
		maxConns = defaultMaxConnsPerIP
	}
	if window == 0 {
		window = defaultConnWindow
	}
	type pair struct{ c, s string }
	groups := map[pair][]int{}
	for i, f := range flows {
		if f.Start.IsZero() {
			continue
		}
		k := pair{f.Client.Addr().String(), f.Server.Addr().String()}
		groups[k] = append(groups[k], i)
	}
	d.storm = map[int]bool{}
	for _, idx := range groups {
		sort.Slice(idx, func(a, b int) bool { return flows[idx[a]].Start.Before(flows[idx[b]].Start) })
		j := 0
		for i := range idx {
			if j < i {
				j = i
			}
			for j < len(idx) && flows[idx[j]].Start.Sub(flows[idx[i]].Start) < window {
				j++
			}
			if j-i > maxConns {
				for _, k := range idx[i:j] {
					d.storm[k+1] = true
				}
			}
		}
	}
}

func (d *longHeavyDetector) Flag(f LabeledFlow, cfg DetectorConfig) bool {
	minDur, minBytes := cfg.LongMin, cfg.LongBytes
	if minDur == 0 {
		minDur = defaultLongMin
	}
	if minBytes == 0 {
		minBytes = defaultLongBytes
	}
	if f.Duration >= minDur && f.BytesUp+f.BytesDn >= minBytes {
		return true
	}
	return f.id > 0 && d.storm[f.id]
}

// ---------------------------------------------------------------------------- D9

type flowPhysicsDetector struct {
	full  *forest
	oof   map[int]float64
	thr   float64
	notes []string
}

func (*flowPhysicsDetector) ID() string { return "D9" }
func (*flowPhysicsDetector) Name() string {
	return "ML flow physics (random forest on six features; outside the gate)"
}
func (*flowPhysicsDetector) Applies(f LabeledFlow) bool { return len(f.Pkts) >= 12 }
func (*flowPhysicsDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return isTunnelControl(f)
}

// Begin trains on the flows of the run: the tunnel controls against the browser control. Every
// training flow is scored by a forest that did not see it (two folds); other flows, such as the
// real v2 flows, are scored by the forest trained on everything.
func (d *flowPhysicsDetector) Begin(flows []LabeledFlow, cfg DetectorConfig) {
	d.full, d.oof, d.thr = nil, map[int]float64{}, 0
	d.notes = []string{"outside the gate: it trains on the flows of this run, so it measures how much a model could learn here, not a promise"}
	var x [][]float64
	var y []bool
	var ids []int
	for i, f := range flows {
		if !d.Applies(f) {
			continue
		}
		switch {
		case d.Truth(f, cfg):
			y = append(y, true)
		case f.Label == LabelBrowser:
			y = append(y, false)
		default:
			continue
		}
		x = append(x, physics(f))
		ids = append(ids, i+1)
	}
	pos := 0
	for _, v := range y {
		if v {
			pos++
		}
	}
	if pos == 0 || pos == len(y) {
		d.notes = append(d.notes, "no training set: it needs both tunnel controls and browser flows")
		return
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = 1
	}
	d.full = trainForest(x, y, seed)
	for fold := 0; fold < 2; fold++ {
		var tx [][]float64
		var ty []bool
		for i := range x {
			if i%2 != fold {
				tx, ty = append(tx, x[i]), append(ty, y[i])
			}
		}
		m := trainForest(tx, ty, seed+int64(fold)+1)
		for i := range x {
			if i%2 == fold {
				d.oof[ids[i]] = m.score(x[i])
			}
		}
	}
	var neg []float64
	for i := range x {
		if !y[i] {
			neg = append(neg, d.oof[ids[i]])
		}
	}
	sort.Float64s(neg)
	if len(neg) > 0 {
		d.thr = neg[int(math.Ceil(0.99*float64(len(neg))))-1]
	}
	d.notes = append(d.notes, fmt.Sprintf("trained on %d tunnel flows and %d browser flows, %d trees, 2-fold out-of-fold scores", pos, len(y)-pos, forestTrees))
}

func (d *flowPhysicsDetector) Notes() []string { return d.notes }

func (d *flowPhysicsDetector) Score(f LabeledFlow, _ DetectorConfig) float64 {
	if s, ok := d.oof[f.id]; ok && f.id > 0 {
		return s
	}
	return d.full.score(physics(f))
}

func (d *flowPhysicsDetector) Flag(f LabeledFlow, cfg DetectorConfig) bool {
	return d.full != nil && d.Score(f, cfg) > d.thr
}

// ---------------------------------------------------------------------------- the run

// Exposure says how many of the flows with the label (not the synthetic positive controls, which
// are the detector's test, not v2's) the detector flags among those it applies to: where a flagged
// v2 flow is a leak. Call it after EvaluateDetectors.
func Exposure(flows []LabeledFlow, label Label, id string) (flagged, n int) {
	for _, f := range flows {
		if f.Label != label || f.Synthetic {
			continue
		}
		v, ok := f.Flags[id]
		if !ok {
			continue
		}
		n++
		if v {
			flagged++
		}
	}
	return flagged, n
}

// verdictOrder is the severity order FinalVerdict walks: the detectors whose flag puts the flow at
// risk come first, then the ones that make it suspicious, then the one outside the gate.
var verdictOrder = []struct{ id, class string }{
	{"D1", "AT RISK"}, {"D2", "AT RISK"}, {"D3", "AT RISK"}, {"D5", "AT RISK"}, {"D10", "AT RISK"},
	{"D4", "suspicious"}, {"D6", "suspicious"}, {"D7", "suspicious"}, {"D8", "suspicious"},
	{"D9", "suspicious (outside the gate)"},
}

// FinalVerdict gives a flow exactly one verdict: the most severe of its detector flags, else the
// plain verdict of Flow.Verdict. The same flow always gets the same string.
func FinalVerdict(f LabeledFlow) string {
	names := map[string]string{}
	for _, d := range Detectors() {
		names[d.ID()] = d.Name()
	}
	for _, v := range verdictOrder {
		if f.Flags[v.id] {
			return fmt.Sprintf("%s: %s, %s", v.class, v.id, names[v.id])
		}
	}
	return f.Flow.Verdict()
}

// gateFPR lists the detectors whose false-positive rate on the browser control the gate bounds.
var gateFPR = []string{"D1", "D2", "D3", "D4", "D5", "D6", "D7", "D8", "D10"}

// GateFailures lists the two-sided rules of the gate that the metrics break on the synthetic side:
// FPR above 1% on the browser control, D1 and D2 recall below 100%, D5 recall below 90%. D6's AUC
// is the v2 side of the gate and comes from V2Gate (here it is the test of D6 on a naive proxy,
// and must be high). The one case read here: a D6 metric without positive controls. D9 is not in
// the gate.
func GateFailures(m map[string]DetectorMetric) []string {
	var out []string
	for _, id := range gateFPR {
		if x, ok := m[id]; ok && x.FPR > 0.01 {
			out = append(out, fmt.Sprintf("%s false-positive rate %.3f on the browser control (gate 0.01)", id, x.FPR))
		}
	}
	for _, c := range []struct {
		id  string
		min float64
	}{{"D1", 1}, {"D2", 1}, {"D5", 0.9}} {
		if x, ok := m[c.id]; ok && x.Recall != nil && *x.Recall < c.min {
			out = append(out, fmt.Sprintf("%s recall %.2f on its control flows (gate %.2f)", c.id, *x.Recall, c.min))
		}
	}
	if x, ok := m["D6"]; ok && x.AUC != nil && x.Positives == 0 && *x.AUC > v2AUC {
		out = append(out, fmt.Sprintf("D6 AUC %.2f (gate %.2f)", *x.AUC, v2AUC))
	}
	sort.Strings(out)
	return out
}

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

// The limits of the v2 side of the gate.
const (
	v2Share = 0.10
	v2AUC   = 0.65
)

// V2Gate evaluates the flows against the v2 side of the gate: of the real v2 flows (labelled
// carrier, not synthetic) each detector may flag at most 10%, and D6's AUC against the browser
// control must be at most 0.65. A detector with no v2 flow in its scope is reported as not
// measured, never as a pass.
func V2Gate(flows []LabeledFlow, cfg DetectorConfig) V2GateResult {
	return GateFor(flows, cfg, LabelCarrier)
}

// GateFor is V2Gate for the flows of another label: the live lab judges the v1 product (LabelProduct)
// by the same limits, for comparison with the v2 carriers.
func GateFor(flows []LabeledFlow, cfg DetectorConfig, label Label) V2GateResult {
	EvaluateDetectors(flows, cfg)
	var res V2GateResult
	for _, id := range gateFPR {
		if id == "D6" {
			continue
		}
		flagged, n := Exposure(flows, label, id)
		row := V2GateRow{Detector: id, N: n, Flagged: flagged, Measured: n > 0}
		if n > 0 {
			row.Share = float64(flagged) / float64(n)
			row.Pass = row.Share <= v2Share
			if !row.Pass {
				res.Failures = append(res.Failures, fmt.Sprintf("%s flags %.0f%% of the v2 flows (%d of %d; the gate allows %.0f%%)", id, 100*row.Share, flagged, n, 100*v2Share))
			}
		}
		res.Rows = append(res.Rows, row)
	}
	d6 := &crossLayerRTTDetector{}
	var pos, neg []float64
	for _, f := range flows {
		if !d6.Applies(f) {
			continue
		}
		switch {
		case f.Label == LabelBrowser:
			neg = append(neg, d6.Score(f, cfg))
		case f.Label == label && !f.Synthetic:
			pos = append(pos, d6.Score(f, cfg))
		}
	}
	row := V2GateRow{Detector: "D6", N: len(pos), Measured: len(pos) > 0 && len(neg) > 0}
	if row.Measured {
		auc := AUC(pos, neg)
		row.AUC = &auc
		row.Pass = auc <= v2AUC
		if !row.Pass {
			res.Failures = append(res.Failures, fmt.Sprintf("D6 AUC %.2f on the v2 flows against the browser control (the gate allows %.2f)", auc, v2AUC))
		}
	}
	res.Rows = append(res.Rows, row)
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Detector < res.Rows[j].Detector })
	sort.Strings(res.Failures)
	return res
}
