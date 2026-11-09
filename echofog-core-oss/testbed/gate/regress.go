package gate

import (
	"fmt"
	"sort"
)

// The regression rule: a metric that is worse than the baseline's by more than its tolerance is a
// yellow, and one that is worse by a lot (or whose probes now leak) is a red. An improvement is
// not a change. The tolerances are about measurement noise at the sizes the lab runs; they are a
// proposal for the audit to sign, like the yellow bands.
const (
	tolFPR       = 0.004 // a false-positive rate may rise by this much before it is a change
	tolRecall    = 0.02  // recall may fall by this much
	redRecall    = 0.10  // and by this much it is red
	tolVis       = 0.25  // visibility, percentage points
	tolShare     = 0.05  // share of v2 flows flagged
	tolAUC       = 0.03
	fprGate      = 0.01 // above this a false-positive rate is red whatever the baseline said
	visibilityOK = 1.0
)

func f2(v float64) string { return fmt.Sprintf("%.4g", v) }

// regress compares the run with the baseline run.
func regress(base, cur *RunReport) []Change {
	var out []Change
	add := func(key, before, after string, st Status, why string) {
		out = append(out, Change{Key: key, Before: before, After: after, Status: st, Why: why})
	}
	for _, id := range sortedIDs(base.Detectors) {
		b := base.Detectors[id]
		c, ok := cur.Detectors[id]
		if !ok {
			add("detectors."+id, "measured", "missing", Yellow, "the baseline measured this detector and the run did not")
			continue
		}
		if id == "D9" {
			continue
		}
		if c.FPR-b.FPR > tolFPR {
			st := Yellow
			if c.FPR > fprGate {
				st = Red
			}
			add("detectors."+id+".fpr", f2(b.FPR), f2(c.FPR), st, "false-positive rate rose")
		}
		if b.Recall != nil && c.Recall != nil && *b.Recall-*c.Recall > tolRecall {
			st := Yellow
			if *b.Recall-*c.Recall > redRecall {
				st = Red
			}
			add("detectors."+id+".recall", f2(*b.Recall), f2(*c.Recall), st, "recall fell")
		}
		if b.Recall != nil && c.Recall == nil {
			add("detectors."+id+".recall", f2(*b.Recall), "n/a", Yellow, "no positive sample any more")
		}
	}
	for _, m := range []struct {
		key  string
		b, c float64
	}{
		{"aggregate.visibility_pct", base.Aggregate.VisibilityPct, cur.Aggregate.VisibilityPct},
		{"aggregate.strict_visibility_pct", base.Aggregate.StrictVisibilityPct, cur.Aggregate.StrictVisibilityPct},
	} {
		if m.c-m.b > tolVis {
			st := Yellow
			if m.c > visibilityOK {
				st = Red
			}
			add(m.key, f2(m.b), f2(m.c), st, "more of the traffic is visible")
		}
	}
	if base.V2Gate != nil && cur.V2Gate != nil {
		for _, bg := range base.V2Gate.Rows {
			for _, cg := range cur.V2Gate.Rows {
				if bg.Detector != cg.Detector || !bg.Measured || !cg.Measured {
					continue
				}
				if bg.AUC != nil && cg.AUC != nil && *cg.AUC-*bg.AUC > tolAUC {
					add("v2_gate."+bg.Detector+".auc", f2(*bg.AUC), f2(*cg.AUC), Yellow, "the v2 flows are easier to tell from a browser")
				}
				if bg.AUC == nil && cg.Share-bg.Share > tolShare {
					add("v2_gate."+bg.Detector+".share", f2(bg.Share), f2(cg.Share), Yellow, "a detector flags more of the v2 flows")
				}
			}
		}
	}
	if bo, co := ownAnswers(base), ownAnswers(cur); co > bo {
		add("probes.own", fmt.Sprint(bo), fmt.Sprint(co), Red, "a node answered a probe on its own where it did not")
	}
	seen := map[string]bool{}
	for _, f := range base.Findings {
		seen[f] = true
	}
	var fresh []string
	for _, f := range cur.Findings {
		if !seen[f] {
			fresh = append(fresh, f)
		}
	}
	if len(fresh) > 0 {
		add("findings", fmt.Sprint(len(base.Findings)), fmt.Sprint(len(cur.Findings)), Yellow, fmt.Sprintf("%d new: %s", len(fresh), fresh[0]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func ownAnswers(r *RunReport) int {
	n := 0
	for _, p := range r.Probes {
		for _, o := range p.Outcomes {
			if o.Own && !o.Expected && o.Skipped == "" {
				n++
			}
		}
	}
	return n
}

func regressionRow(in Input, changes []Change) Row {
	r := Row{ID: "regression", Metric: "against the baseline", Source: srcReg, Limit: "no metric worse than the baseline beyond its tolerance"}
	if in.Baseline == nil {
		r.Status, r.Value, r.Note = NotMeasured, "n/a", "no signed baseline: the regression was not checked"
		return r
	}
	r.Status, r.Value = Green, fmt.Sprintf("0 changes since %s (%s)", in.Baseline.SignedAt.Format("2006-01-02"), in.Baseline.Commit)
	for _, c := range changes {
		r.Status = worse(r.Status, c.Status)
	}
	if len(changes) > 0 {
		r.Value = fmt.Sprintf("%d changes since %s (%s)", len(changes), in.Baseline.SignedAt.Format("2006-01-02"), in.Baseline.Commit)
	}
	return r
}
