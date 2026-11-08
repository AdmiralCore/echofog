package gate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

// The thresholds. Each row says where its limit comes from. The green limits are the design's
// (03-launchpad-v2.md §5a) and the decisions behind it; the yellow bands are a proposal that the
// M4 audit signs, and no number is fitted to a run.
const (
	srcDesign = "03-launchpad-v2 §5a"
	srcH1a    = "H1a (two-sided gate)"
	srcH15    = "H15 (decisions.md 2026-11-05)"
	srcAD21   = "AD21 (speed regression)"
	srcLab    = "T15.0c findings (the product, TEO)"
	srcReg    = "T15.0e regression rule"
)

// worse orders the statuses that decide a verdict.
func worse(a, b Status) Status {
	rank := func(s Status) int {
		switch s {
		case Red:
			return 3
		case Yellow:
			return 2
		case Green:
			return 1
		}
		return 0 // not measured and info decide nothing
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func pct(v float64) string { return fmt.Sprintf("%.2f%%", 100*v) }

// Evaluate puts every metric against its threshold, compares the run with the baseline and builds
// the checklist.
func Evaluate(in Input) Result {
	res := Result{SignedAt: in.Now, Commit: in.Commit}
	if in.Run != nil {
		res.Rows = append(res.Rows, detectorRows(in.Run)...)
		res.Rows = append(res.Rows, visibilityRows(in.Run)...)
		res.Rows = append(res.Rows, v2Rows(in.Run)...)
		res.Rows = append(res.Rows, h1aRow(in.Run))
		res.Rows = append(res.Rows, probeRows(in.Run)...)
	}
	res.Rows = append(res.Rows, behaviourRows(in.Behaviour)...)
	prod := productRows(in.Behaviour)
	res.Rows = append(res.Rows, prod...)
	for _, r := range prod {
		if r.Status == Red {
			res.Waiting = append(res.Waiting, r)
		}
	}
	if in.Baseline != nil && in.Run != nil {
		res.Changes = regress(&in.Baseline.Run, in.Run)
	}
	res.Rows = append(res.Rows, regressionRow(in, res.Changes))
	res.Rows = append(res.Rows, speedRows(in)...)
	res.Rows = append(res.Rows, scenarioRows(in.Scenarios)...)

	res.Verdict = verdictOf(res.Rows)
	for _, r := range res.Rows {
		if r.Status == Red {
			res.Blocks = true
		}
	}
	res.Policy = policy
	res.Checklist = checklist(res)
	return res
}

// policy is what the thresholds decide and what they leave to the audit; it is part of every
// result and of the checklist, so the person who reads a verdict reads the rules with it.
var policy = []string{
	"yellow does not block the release; only red does (decision of the M4 audit)",
	"the yellow bands and the regression tolerances are a proposal that the M4 audit signs; the green limits are the design's",
	"green means everything the gate reads was measured: anything not measured makes the verdict 'not measured' and the exit code 2, unless -allow-unmeasured says a partial run is wanted",
	"a run in which no detector, probe or behaviour metric was measured exits 2 whatever is allowed",
}

// verdictOf is red if any row is red; otherwise 'not measured' if any row could not be measured or
// nothing was measured at all; otherwise yellow if any row is yellow; otherwise green.
func verdictOf(rows []Row) Status {
	var red, yellow, green, missing bool
	for _, r := range rows {
		switch r.Status {
		case Red:
			red = true
		case Yellow:
			yellow = true
		case Green:
			green = true
		case NotMeasured:
			missing = true
		}
	}
	switch {
	case red:
		return Red
	case missing || !(green || yellow):
		return NotMeasured
	case yellow:
		return Yellow
	}
	return Green
}

func sortedIDs(m map[string]DetectorEntry) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := len(ids[i]), len(ids[j])
		if a != b {
			return a < b
		}
		return ids[i] < ids[j]
	})
	return ids
}

func detectorRows(run *RunReport) []Row {
	var out []Row
	for _, id := range sortedIDs(run.Detectors) {
		d := run.Detectors[id]
		fpr := Row{ID: id, Metric: "fpr", Value: pct(d.FPR), Limit: "<= 0.1 % green, <= 1 % yellow", Source: srcDesign}
		switch {
		case id == "D9":
			fpr.Status, fpr.Note = Info, "outside the gate"
		case d.BrowserNegatives != nil && *d.BrowserNegatives == 0:
			fpr.Status, fpr.Note = NotMeasured, "no browser control in the run"
		case d.FPR <= 0.001:
			fpr.Status = Green
		case d.FPR <= 0.01:
			fpr.Status = Yellow
		default:
			fpr.Status = Red
		}
		out = append(out, fpr)

		rec := Row{ID: id, Metric: "recall", Source: srcDesign}
		if d.Recall == nil {
			rec.Status, rec.Value, rec.Note = NotMeasured, "n/a", "no positive sample is not recall 1"
			out = append(out, rec)
			continue
		}
		v := *d.Recall
		rec.Value = pct(v)
		switch id {
		case "D1", "D2", "D3", "D4", "D10":
			rec.Limit = "100 % of the control flows"
			rec.Status = Red
			if v >= 1 {
				rec.Status = Green
			}
		case "D5":
			rec.Limit, rec.Source = ">= 90 % green, >= 80 % yellow", srcH1a
			switch {
			case v >= 0.9:
				rec.Status = Green
			case v >= 0.8:
				rec.Status = Yellow
			default:
				rec.Status = Red
			}
		default:
			rec.Status, rec.Note = Info, "the detector's own test; reported, not gated"
		}
		out = append(out, rec)
	}
	return out
}

func visibilityRows(run *RunReport) []Row {
	band := func(metric string, v float64) Row {
		r := Row{ID: "visibility", Metric: metric, Value: fmt.Sprintf("%.2f%%", v), Limit: "0 % green, <= 1 % yellow", Source: srcDesign}
		switch {
		case v == 0:
			r.Status = Green
		case v <= 1:
			r.Status = Yellow
		default:
			r.Status = Red
		}
		return r
	}
	return []Row{band("visibility_pct", run.Aggregate.VisibilityPct), band("strict_visibility_pct", run.Aggregate.StrictVisibilityPct)}
}

func v2Rows(run *RunReport) []Row {
	var out []Row
	for _, id := range []string{"D5", "D6", "D7", "D8"} {
		metric, limit := "v2 share", "<= 10 % green, <= 20 % yellow"
		if id == "D6" {
			metric, limit = "v2 auc", "<= 0.65 green, <= 0.75 yellow"
		}
		r := Row{ID: id, Metric: metric, Limit: limit, Source: srcH1a, Status: NotMeasured, Value: "n/a",
			Note: "no real v2 flow in the detector's scope in this run"}
		if run.V2Gate != nil {
			for _, g := range run.V2Gate.Rows {
				if g.Detector != id || !g.Measured {
					continue
				}
				r.Note = ""
				if id == "D6" && g.AUC != nil {
					r.Value = fmt.Sprintf("%.2f", *g.AUC)
					r.Status = pick(*g.AUC, 0.65, 0.75)
				} else if id != "D6" {
					r.Value = fmt.Sprintf("%s (%d of %d)", pct(g.Share), g.Flagged, g.N)
					r.Status = pick(g.Share, 0.10, 0.20)
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// pick is green up to green, yellow up to yellow, red above.
func pick(v, green, yellow float64) Status {
	switch {
	case v <= green:
		return Green
	case v <= yellow:
		return Yellow
	}
	return Red
}

func h1aRow(run *RunReport) Row {
	r := Row{ID: "H1a", Metric: "records KS", Limit: "KS <= the browser control's own spread", Source: srcH1a, Status: NotMeasured, Value: "n/a",
		Note: "no v2 exit flow with TLS records in this run"}
	if run.H1a == nil {
		return r
	}
	r.Value = fmt.Sprintf("%.3f against %.3f over %d flows", run.H1a.KS, run.H1a.Threshold, run.H1a.Flows)
	r.Note = ""
	r.Status = Red
	if run.H1a.Pass {
		r.Status = Green
	}
	return r
}

func probeRows(run *RunReport) []Row {
	var out []Row
	for _, id := range []string{"D11", "D12", "D13", "D14", "D15"} {
		own, n := 0, 0
		for _, p := range run.Probes {
			for _, o := range p.Outcomes {
				if o.Detector != id || o.Skipped != "" {
					continue
				}
				n++
				if o.Own && !o.Expected {
					own++
				}
			}
		}
		r := Row{ID: id, Metric: "own answers", Limit: "0 answers of the node's own outside the cover", Source: srcDesign}
		switch {
		case n == 0:
			r.Status, r.Value, r.Note = NotMeasured, "n/a", "no probe of this kind ran"
		case own > 0:
			r.Status, r.Value = Red, fmt.Sprintf("%d of %d", own, n)
		default:
			r.Status, r.Value = Green, fmt.Sprintf("0 of %d", n)
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------- behaviour

// h15 is the sequence H15 allows to a pair that stays blocked: 120 s doubling to 30 minutes.
var h15 = []time.Duration{0, 2 * time.Minute, 6 * time.Minute, 14 * time.Minute, 30 * time.Minute}

func behaviourRows(b *behaviour.Report) []Row {
	d24 := Row{ID: "D24", Metric: "attempts", Limit: "attempts no earlier than 0, 2, 6, 14 and 30 minutes, then 30 minutes apart (29 to 31)", Source: srcH15,
		Status: NotMeasured, Value: "n/a", Note: "no behaviour report, or no never-lifted block played"}
	d21 := Row{ID: "D21", Metric: "carriers per pair", Limit: "<= 2 open, the AND rule never fires", Source: srcH1a, Status: NotMeasured, Value: "n/a", Note: "no behaviour report"}
	d25 := Row{ID: "D25", Metric: "443 down", Limit: "first success <= 10 s", Source: srcDesign, Status: NotMeasured, Value: "n/a", Note: "no behaviour report"}
	if b == nil {
		return []Row{d24, d21, d25}
	}
	if at := b.PenaltyNever.Attempts; len(at) >= len(h15) {
		d24.Note, d24.Value = "", fmt.Sprint(at)
		d24.Status = Green
		// the first attempt is at the start; each of the next four may be late, never early
		if at[0] != 0 {
			d24.Status = Red
		}
		for i := 1; i < len(h15); i++ {
			if at[i] < h15[i] {
				d24.Status = Red
			}
		}
		// after the fifth, every interval is 30 minutes, a minute either way
		for i := len(h15); i < len(at); i++ {
			if d := at[i] - at[i-1]; d < 29*time.Minute || d > 31*time.Minute {
				d24.Status = Red
			}
		}
	}
	if p, ok := b.Parallel["product"]; ok {
		d21.Note, d21.Value = "", fmt.Sprintf("%d open, fired=%v", p.MaxOpen, p.Fired)
		d21.Status = Green
		if p.MaxOpen > 2 || p.Fired {
			d21.Status = Red
		}
	}
	if d, ok := b.Dial["port-443-down"]; ok {
		d25.Note, d25.Value = "", fmt.Sprintf("ok=%v after %s", d.OK, d.Elapsed.Round(time.Millisecond))
		d25.Status = Green
		if !d.OK || d.Elapsed > 10*time.Second {
			d25.Status = Red
		}
	}
	return []Row{d24, d21, d25}
}

const waits = "waits for a fix of the product (TEO), not of the lab"

func productRows(b *behaviour.Report) []Row {
	freeze := Row{ID: "product:freeze-no-limit", Metric: "1 GiB under a freeze with UDP gone", Limit: "the transfer completes", Source: srcLab, Status: NotMeasured, Value: "n/a"}
	pmtu := Row{ID: "product:noise3-pmtu", Metric: "noise3 on a path of 1280 and 1200 bytes", Limit: "connects", Source: srcLab, Status: NotMeasured, Value: "n/a"}
	if b == nil {
		return []Row{freeze, pmtu}
	}
	if len(b.Freeze) > 0 {
		freeze.Status, freeze.Value = Green, "completes"
		for _, f := range b.Freeze {
			if !f.Completed {
				freeze.Status, freeze.Value = Red, fmt.Sprintf("row %s stops at %d bytes", f.Rule, f.FrozenAt)
				freeze.Note = waits + ": Rotate is set but the row has no byte limit"
				break
			}
		}
	}
	seen, bad := 0, []string{}
	for _, k := range []string{"mtu 1280", "mtu 1200"} {
		if t, ok := b.Net[k]; ok {
			seen++
			if !t.Connected {
				bad = append(bad, k)
			}
		}
	}
	if seen > 0 {
		pmtu.Status, pmtu.Value = Green, "connects"
		if len(bad) > 0 {
			pmtu.Status, pmtu.Value = Red, strings.Join(bad, ", ")+": no connection"
			pmtu.Note = waits + ": the first flight is 1271 bytes"
		}
	}
	return []Row{freeze, pmtu}
}

// ---------------------------------------------------------------------------- speed

func speedRows(in Input) []Row {
	none := Row{ID: "speed", Metric: "efv2 echo", Limit: "no carrier more than 10 % below the baseline", Source: srcAD21, Status: NotMeasured, Value: "n/a",
		Note: "no speed baseline of T0.3, or efv2 echo has not run"}
	if in.Baseline == nil || len(in.Baseline.Speed) == 0 {
		return []Row{none}
	}
	carriers := make([]string, 0, len(in.Baseline.Speed))
	for c := range in.Baseline.Speed {
		carriers = append(carriers, c)
	}
	sort.Strings(carriers)
	var out []Row
	for _, c := range carriers {
		base := in.Baseline.Speed[c]
		r := Row{ID: "speed", Metric: c, Limit: ">= 90 % of " + fmt.Sprintf("%.1f Mbit/s", base), Source: srcAD21}
		cur, ok := in.Speed[c]
		switch {
		case !ok:
			r.Status, r.Value, r.Note = NotMeasured, "n/a", "this carrier did not run"
		case cur < base*0.9:
			r.Status, r.Value = Red, fmt.Sprintf("%.1f Mbit/s (%.1f %% down)", cur, 100*(1-cur/base))
		default:
			r.Status, r.Value = Green, fmt.Sprintf("%.1f Mbit/s", cur)
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------- the rest

// MakeBaseline signs a run as the reference.
func MakeBaseline(run RunReport, speed map[string]float64, now time.Time, commit string) Baseline {
	return Baseline{SignedAt: now, Commit: commit, Run: run, Speed: speed}
}

// ExitCode is what the nightly command returns: 1 when something is red; 2 when the verdict is
// 'not measured' (a baseline, a speed, a behaviour report or a probe is missing) unless the person
// allowed a partial run, and always 2 when nothing at all was measured; 0 otherwise.
func ExitCode(r Result, allowUnmeasured bool) int {
	if r.Blocks {
		return 1
	}
	measured := false
	for _, row := range r.Rows {
		if row.ID != "visibility" && (row.Status == Green || row.Status == Yellow || row.Status == Red) {
			measured = true
		}
	}
	if !measured || (r.Verdict == NotMeasured && !allowUnmeasured) {
		return 2
	}
	return 0
}

// BehaviourRows is the gate's reading of a behaviour report (D24, D21, D25), for the scenarios that
// take their rows from the same limits instead of keeping a copy.
func BehaviourRows(b *behaviour.Report) []Row { return behaviourRows(b) }
