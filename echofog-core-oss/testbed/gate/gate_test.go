package gate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

func f(v float64) *float64 { return &v }

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// goodRun is a run inside every threshold.
func goodRun(t *testing.T) *RunReport {
	t.Helper()
	const raw = `{
	 "schema":2,"when":"2026-11-07T10:00:00Z","commit":"abc1234","spec":"v2@052e1e3","scenario":"v2-censor","profile":"desktop","network":"lab",
	 "detectors":{
	  "D1":{"recall":1,"fpr":0,"n":20,"positives":6},"D2":{"recall":1,"fpr":0,"n":8,"positives":1},
	  "D3":{"recall":1,"fpr":0,"n":16,"positives":2},"D4":{"recall":1,"fpr":0,"n":16,"positives":8},
	  "D5":{"recall":0.97,"fpr":0,"n":400,"positives":100},"D6":{"recall":0.99,"fpr":0.008,"auc":0.99,"n":400,"positives":100},
	  "D7":{"recall":1,"fpr":0.004,"n":400,"positives":50},"D8":{"recall":1,"fpr":0,"n":400,"positives":40},
	  "D9":{"recall":0.8,"fpr":0.01,"auc":0.93,"tpr_at_1pct_fpr":0.8,"n":400,"positives":100},
	  "D10":{"recall":1,"fpr":0,"n":13,"positives":1}},
	 "aggregate":{"visibility_pct":0,"strict_visibility_pct":0},
	 "v2_gate":{"rows":[
	  {"detector":"D5","n":60,"flagged":1,"share":0.016,"measured":true,"pass":true},
	  {"detector":"D6","n":60,"auc":0.52,"measured":true,"pass":true},
	  {"detector":"D7","n":60,"flagged":2,"share":0.03,"measured":true,"pass":true},
	  {"detector":"D8","n":60,"flagged":0,"share":0,"measured":true,"pass":true},
	  {"detector":"D4","n":60,"flagged":3,"share":0.05,"measured":true,"pass":true},
	  {"detector":"D10","n":60,"flagged":0,"share":0,"measured":true,"pass":true}]},
	 "h1a":{"ks":0.03,"threshold":0.06,"flows":60,"pass":true},
	 "probes":[{"target":"ordinary","outcomes":[
	  {"detector":"D11","case":"at once"},{"detector":"D12","case":"garbage"},{"detector":"D13","case":"page"},
	  {"detector":"D14","case":"16 bytes"},{"detector":"D15","case":"valid token","own":true,"expected":true},{"detector":"D15","case":"foreign token"}]}],
	 "findings":["browser profiles 2026.11.1: chrome"]}`
	var r RunReport
	require.NoError(t, json.Unmarshal([]byte(raw), &r))
	return &r
}

func row(res Result, id, metric string) Row {
	for _, r := range res.Rows {
		if r.ID == id && (metric == "" || r.Metric == metric) {
			return r
		}
	}
	return Row{ID: "missing:" + id + "/" + metric}
}

func TestStatusNames(t *testing.T) {
	assert.Equal(t, "green", Green.String())
	assert.Equal(t, "yellow", Yellow.String())
	assert.Equal(t, "red", Red.String())
	assert.Equal(t, "not measured", NotMeasured.String())
	assert.Equal(t, "info", Info.String())
}

func TestARunInsideEveryThresholdIsGreenAndBlocksNothing(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Now: now, Commit: "deadbee"})
	for _, r := range res.Rows {
		assert.NotEqual(t, Red, r.Status, "%s %s: %s against %s", r.ID, r.Metric, r.Value, r.Limit)
	}
	assert.False(t, res.Blocks)
	assert.Equal(t, now, res.SignedAt, "the thresholds are signed with the date of the run of the gate")
	assert.Equal(t, "deadbee", res.Commit)
	// a report alone leaves the baseline, the speed and the behaviour unmeasured: not green, not zero
	assert.Equal(t, NotMeasured, res.Verdict)
	assert.Equal(t, 2, ExitCode(res, false))
	assert.Equal(t, 0, ExitCode(res, true), "unless the person who runs it says that is what they want")
	for _, id := range []string{"D1", "D2", "D3", "D4", "D5", "D6", "D7", "D8", "D10", "D11", "D12", "D13", "D14", "D15", "visibility", "H1a"} {
		found := false
		for _, r := range res.Rows {
			found = found || r.ID == id
		}
		assert.True(t, found, "a row for %s", id)
	}
	d9 := row(res, "D9", "")
	assert.Equal(t, Info, d9.Status, "D9 is outside the gate")
}

func TestTheFalsePositiveBandsAreGreenYellowRed(t *testing.T) {
	for _, c := range []struct {
		fpr  float64
		want Status
	}{{0, Green}, {0.0009, Green}, {0.004, Yellow}, {0.01, Yellow}, {0.0101, Red}, {0.05, Red}} {
		run := goodRun(t)
		d := run.Detectors["D4"]
		d.FPR = c.fpr
		run.Detectors["D4"] = d
		res := Evaluate(Input{Run: run, Now: now})
		assert.Equal(t, c.want, row(res, "D4", "fpr").Status, "FPR %.4f", c.fpr)
	}
}

func TestRecallRulesPerDetector(t *testing.T) {
	set := func(id string, recall float64) Result {
		run := goodRun(t)
		d := run.Detectors[id]
		d.Recall = f(recall)
		run.Detectors[id] = d
		return Evaluate(Input{Run: run, Now: now})
	}
	assert.Equal(t, Red, row(set("D1", 0.99), "D1", "recall").Status, "a random-TCP control must be found every time")
	assert.Equal(t, Red, row(set("D2", 0.9), "D2", "recall").Status)
	assert.Equal(t, Red, row(set("D10", 0.5), "D10", "recall").Status)
	assert.Equal(t, Green, row(set("D5", 0.9), "D5", "recall").Status, "nested TLS: at least 90 %")
	assert.Equal(t, Yellow, row(set("D5", 0.85), "D5", "recall").Status)
	assert.Equal(t, Red, row(set("D5", 0.7), "D5", "recall").Status)
	// recall of D6, D7, D8 is the detector's own test and is reported, not gated
	assert.Equal(t, Info, row(set("D7", 0.2), "D7", "recall").Status)

	run := goodRun(t)
	d := run.Detectors["D1"]
	d.Recall, d.Positives = nil, 0
	run.Detectors["D1"] = d
	assert.Equal(t, NotMeasured, row(Evaluate(Input{Run: run, Now: now}), "D1", "recall").Status, "no positive sample is not recall 1")
}

func TestVisibilityBands(t *testing.T) {
	for _, c := range []struct {
		pct  float64
		want Status
	}{{0, Green}, {0.5, Yellow}, {1, Yellow}, {1.01, Red}, {49.6, Red}} {
		run := goodRun(t)
		run.Aggregate.VisibilityPct = c.pct
		assert.Equal(t, c.want, row(Evaluate(Input{Run: run, Now: now}), "visibility", "visibility_pct").Status, "%.2f", c.pct)
	}
}

func TestTheV2SideOfTheGate(t *testing.T) {
	bad := goodRun(t)
	bad.V2Gate.Rows[0].Share, bad.V2Gate.Rows[0].Flagged = 0.60, 36 // D5 flags most of the v2 flows
	bad.V2Gate.Rows[1].AUC = f(0.90)                                // D6 separates them
	res := Evaluate(Input{Run: bad, Now: now})
	assert.Equal(t, Red, row(res, "D5", "v2 share").Status)
	assert.Equal(t, Red, row(res, "D6", "v2 auc").Status)
	assert.Equal(t, Green, row(res, "D7", "v2 share").Status)
	assert.True(t, res.Blocks)

	yellow := goodRun(t)
	yellow.V2Gate.Rows[0].Share = 0.15
	yellow.V2Gate.Rows[1].AUC = f(0.70)
	res = Evaluate(Input{Run: yellow, Now: now})
	assert.Equal(t, Yellow, row(res, "D5", "v2 share").Status)
	assert.Equal(t, Yellow, row(res, "D6", "v2 auc").Status)

	none := goodRun(t)
	none.V2Gate.Rows[0].Measured, none.V2Gate.Rows[1].Measured = false, false
	res = Evaluate(Input{Run: none, Now: now})
	assert.Equal(t, NotMeasured, row(res, "D5", "v2 share").Status, "a lab with no v2 TCP carrier proves nothing about D5")
	assert.Equal(t, NotMeasured, row(res, "D6", "v2 auc").Status)
	assert.False(t, res.Blocks, "not measured is not red")
	assert.Equal(t, 2, ExitCode(res, false), "a gate that did not measure D5 and D6 is not a pass")
	assert.Equal(t, NotMeasured, res.Verdict)
	assert.Equal(t, 0, ExitCode(res, true))

	noGate := goodRun(t)
	noGate.V2Gate, noGate.H1a = nil, nil
	res = Evaluate(Input{Run: noGate, Now: now})
	assert.Equal(t, NotMeasured, row(res, "H1a", "").Status)
}

// The gate reads the whole v2 side of H1a, not a fixed list of four detectors: a red D4 or D10 on
// the real v2 flows blocks the release (the hole the T15.0f-2 acceptance found).
func TestRedD4OrD10OnTheV2SideBlocks(t *testing.T) {
	run := goodRun(t)
	red := func(id string, n, flagged int) {
		for i := range run.V2Gate.Rows {
			if run.V2Gate.Rows[i].Detector == id {
				run.V2Gate.Rows[i].N = n
				run.V2Gate.Rows[i].Flagged = flagged
				run.V2Gate.Rows[i].Share = float64(flagged) / float64(n)
				run.V2Gate.Rows[i].Pass = false
			}
		}
	}
	red("D4", 27, 9)
	red("D10", 18, 6)
	res := Evaluate(Input{Run: run, Now: now})
	assert.Equal(t, Red, row(res, "D4", "v2 share").Status)
	assert.Equal(t, Red, row(res, "D10", "v2 share").Status)
	assert.True(t, res.Blocks, "33 % of the v2 flows flagged by D4 and D10 blocks the release")
	assert.Equal(t, Red, res.Verdict)
	for _, r := range res.Rows {
		if r.Status == Red {
			assert.Contains(t, []string{"D4", "D10"}, r.ID, "only D4 and D10 are red: %s %s", r.ID, r.Metric)
		}
	}

	green := Evaluate(Input{Run: goodRun(t), Now: now})
	assert.Equal(t, Green, row(green, "D4", "v2 share").Status)
	assert.Equal(t, Green, row(green, "D10", "v2 share").Status)
	assert.False(t, green.Blocks, "a run inside every threshold blocks nothing")
}

// A report that predates the D4 and D10 rows says nothing about them: the gate calls that not
// measured (exit 2 without -allow-unmeasured), never a silent pass. Blocks itself is unchanged,
// so old reports keep the verdict they had.
func TestAReportWithoutTheD4AndD10RowsIsNotASilentPass(t *testing.T) {
	run := goodRun(t)
	kept := run.V2Gate.Rows[:0]
	for _, r := range run.V2Gate.Rows {
		if r.Detector != "D4" && r.Detector != "D10" {
			kept = append(kept, r)
		}
	}
	run.V2Gate.Rows = kept
	res := Evaluate(Input{Run: run, Now: now})
	assert.Equal(t, NotMeasured, row(res, "D4", "v2 share").Status)
	assert.Equal(t, NotMeasured, row(res, "D10", "v2 share").Status)
	assert.False(t, res.Blocks, "a report that predates the rows does not turn red by itself")
	assert.Equal(t, 2, ExitCode(res, false), "but the gate does not call it green")
}

func TestH1aAndTheProbes(t *testing.T) {
	run := goodRun(t)
	run.H1a.Pass = false
	run.H1a.KS = 0.3
	assert.Equal(t, Red, row(Evaluate(Input{Run: run, Now: now}), "H1a", "").Status)

	run = goodRun(t)
	run.Probes[0].Outcomes[1].Own = true // D12: the node answered a malformed first record on its own
	res := Evaluate(Input{Run: run, Now: now})
	assert.Equal(t, Red, row(res, "D12", "").Status)
	assert.Equal(t, Green, row(res, "D11", "").Status)
	assert.Equal(t, Green, row(res, "D15", "").Status, "the expected answer to a valid token is not a leak")
	assert.True(t, res.Blocks)

	run = goodRun(t)
	run.Probes = nil
	res = Evaluate(Input{Run: run, Now: now})
	for _, id := range []string{"D11", "D12", "D13", "D14", "D15"} {
		assert.Equal(t, NotMeasured, row(res, id, "").Status, id)
	}
}

// A red gate is reproduced on a swapped report: the same run, one number changed.
func TestARedGateIsReproducedOnASwappedReport(t *testing.T) {
	run := goodRun(t)
	require.False(t, Evaluate(Input{Run: run, Now: now}).Blocks)
	d := run.Detectors["D3"]
	d.FPR = 0.07
	run.Detectors["D3"] = d
	res := Evaluate(Input{Run: run, Now: now})
	assert.True(t, res.Blocks)
	assert.Equal(t, Red, res.Verdict)
	assert.Equal(t, 1, ExitCode(res, false), "the nightly command fails and the release is blocked")
}

// ---------------------------------------------------------------------------- the baseline

func TestABaselineIsSignedWithADateAndACommit(t *testing.T) {
	b := MakeBaseline(*goodRun(t), map[string]float64{"noise3": 80}, now, "deadbee")
	assert.Equal(t, now, b.SignedAt)
	assert.Equal(t, "deadbee", b.Commit)
	assert.Equal(t, "abc1234", b.Run.Commit)
	assert.Equal(t, 80.0, b.Speed["noise3"])
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	var back Baseline
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, b, back)
}

func TestWithoutABaselineTheRegressionIsNotMeasured(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Now: now})
	assert.Equal(t, NotMeasured, row(res, "regression", "").Status)
	assert.False(t, res.Blocks)
	assert.Equal(t, 2, ExitCode(res, false), "a release gate needs a baseline")
}

func TestRegressionAgainstTheBaseline(t *testing.T) {
	base := MakeBaseline(*goodRun(t), nil, now.Add(-24*time.Hour), "old")
	same := Evaluate(Input{Run: goodRun(t), Baseline: &base, Now: now})
	assert.Empty(t, same.Changes)
	assert.Equal(t, Green, row(same, "regression", "").Status)

	cur := goodRun(t)
	d := cur.Detectors["D7"]
	d.FPR = 0.009 // inside the gate, but 0.005 worse than the baseline
	cur.Detectors["D7"] = d
	cur.Aggregate.VisibilityPct = 0.4
	cur.Findings = append(cur.Findings, "a new finding")
	res := Evaluate(Input{Run: cur, Baseline: &base, Now: now})
	keys := map[string]Change{}
	for _, c := range res.Changes {
		keys[c.Key] = c
	}
	require.Contains(t, keys, "detectors.D7.fpr")
	assert.Equal(t, Yellow, keys["detectors.D7.fpr"].Status)
	require.Contains(t, keys, "aggregate.visibility_pct")
	assert.Equal(t, Yellow, keys["aggregate.visibility_pct"].Status)
	require.Contains(t, keys, "findings")
	assert.Equal(t, Yellow, keys["findings"].Status)
	assert.Equal(t, Yellow, row(res, "regression", "").Status)
	assert.False(t, res.Blocks)

	worse := goodRun(t)
	d = worse.Detectors["D5"]
	d.Recall = f(0.80) // from 0.97: more than the 0.10 a regression may cost
	worse.Detectors["D5"] = d
	res = Evaluate(Input{Run: worse, Baseline: &base, Now: now})
	assert.Equal(t, Red, row(res, "regression", "").Status)
	assert.True(t, res.Blocks, "a regression blocks the release")

	better := goodRun(t)
	d = better.Detectors["D7"]
	d.FPR = 0
	better.Detectors["D7"] = d
	res = Evaluate(Input{Run: better, Baseline: &base, Now: now})
	assert.Empty(t, res.Changes, "an improvement is not a regression")
}

func TestARegressionInProbesOrInWhatWasMeasured(t *testing.T) {
	base := MakeBaseline(*goodRun(t), nil, now, "old")
	cur := goodRun(t)
	cur.Probes[0].Outcomes[0].Own = true
	res := Evaluate(Input{Run: cur, Baseline: &base, Now: now})
	assert.Equal(t, Red, row(res, "regression", "").Status)

	cur = goodRun(t)
	delete(cur.Detectors, "D8")
	res = Evaluate(Input{Run: cur, Baseline: &base, Now: now})
	var lost *Change
	for i := range res.Changes {
		if res.Changes[i].Key == "detectors.D8" {
			lost = &res.Changes[i]
		}
	}
	require.NotNil(t, lost, "a detector the baseline had and the run lacks")
	assert.Equal(t, Yellow, lost.Status)
}

// AD21: the speed of every carrier against the baseline of T0.3; a fall over 10 % is red.
func TestSpeedRegressionOverTenPercentIsRed(t *testing.T) {
	base := MakeBaseline(*goodRun(t), map[string]float64{"noise3": 100, "reality2-tcp": 40}, now, "t0.3")
	res := Evaluate(Input{Run: goodRun(t), Baseline: &base, Speed: map[string]float64{"noise3": 91, "reality2-tcp": 35}, Now: now})
	assert.Equal(t, Green, row(res, "speed", "noise3").Status, "9 % down is inside the 10 %")
	assert.Equal(t, Red, row(res, "speed", "reality2-tcp").Status, "35 of 40 is 12.5 % down")
	assert.True(t, res.Blocks)
}

func TestSpeedAtTenPercentAndMissing(t *testing.T) {
	base := MakeBaseline(*goodRun(t), map[string]float64{"noise3": 100, "ws2": 20}, now, "t0.3")
	res := Evaluate(Input{Run: goodRun(t), Baseline: &base, Speed: map[string]float64{"noise3": 89.9}, Now: now})
	assert.Equal(t, Red, row(res, "speed", "noise3").Status)
	assert.Equal(t, NotMeasured, row(res, "speed", "ws2").Status, "a carrier that did not run is not measured")
	assert.True(t, res.Blocks)

	none := Evaluate(Input{Run: goodRun(t), Baseline: &base, Now: now})
	assert.Equal(t, NotMeasured, row(none, "speed", "").Status, "efv2 echo has not run: the speed gate says so")
}

// ---------------------------------------------------------------------------- behaviour and the product

func behaviourReport(defects bool) *behaviour.Report {
	never := behaviour.PenaltyResult{Attempts: []time.Duration{0, 2 * time.Minute, 6 * time.Minute, 14 * time.Minute, 30 * time.Minute, 60 * time.Minute, 90 * time.Minute, 120 * time.Minute}}
	r := &behaviour.Report{
		PenaltyNever: never,
		Dial:         map[string]behaviour.DialResult{"port-443-down": {OK: true, Elapsed: 210 * time.Millisecond}},
		Parallel:     map[string]behaviour.ParallelResult{"product": {MaxOpen: 2}},
		Net:          map[string]behaviour.TransferResult{"mtu 1500": {Connected: true}, "mtu 1280": {Connected: !defects}, "mtu 1200": {Connected: !defects}},
		Freeze: []behaviour.FreezeResult{
			{Rule: "ru-mobile-foreign", Via: "entry-in-country", Completed: true},
			{Rule: "ru-wired-foreign", Via: map[bool]string{true: "frozen", false: "tcp-chunked"}[defects], Completed: !defects},
		},
	}
	return r
}

func TestD24FollowsH15NotTheOldTable(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Behaviour: behaviourReport(false), Now: now})
	d24 := row(res, "D24", "")
	assert.Equal(t, Green, d24.Status, "attempts at 0, 2, 6, 14 and 30 minutes then every 30: %s", d24.Value)
	assert.Contains(t, d24.Source, "H15")
	assert.NotContains(t, d24.Limit, "no growth", "the old 'at most 120 s, no growth' criterion is gone")

	b := behaviourReport(false)
	b.PenaltyNever.Attempts = []time.Duration{0, 2 * time.Minute, 4 * time.Minute, 6 * time.Minute, 8 * time.Minute} // flat 120 s: hammering
	assert.Equal(t, Red, row(Evaluate(Input{Run: goodRun(t), Behaviour: b, Now: now}), "D24", "").Status)

	b = behaviourReport(false)
	b.PenaltyNever.Attempts = []time.Duration{0, 2 * time.Minute, 6 * time.Minute, 14 * time.Minute, 30 * time.Minute, 150 * time.Minute}
	assert.Equal(t, Red, row(Evaluate(Input{Run: goodRun(t), Behaviour: b, Now: now}), "D24", "").Status, "a gap over 30 minutes after the fifth attempt")

	b = behaviourReport(false)
	b.PenaltyNever = behaviour.PenaltyResult{}
	assert.Equal(t, NotMeasured, row(Evaluate(Input{Run: goodRun(t), Behaviour: b, Now: now}), "D24", "").Status)
	assert.Equal(t, NotMeasured, row(Evaluate(Input{Run: goodRun(t), Now: now}), "D24", "").Status, "no behaviour report")
}

func TestBehaviourRowsOfTheProduct(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Behaviour: behaviourReport(false), Now: now})
	assert.Equal(t, Green, row(res, "D21", "").Status)
	assert.Equal(t, Green, row(res, "D25", "").Status)

	b := behaviourReport(false)
	b.Parallel["product"] = behaviour.ParallelResult{MaxOpen: 5, Fired: true}
	b.Dial["port-443-down"] = behaviour.DialResult{OK: true, Elapsed: 12 * time.Second}
	res = Evaluate(Input{Run: goodRun(t), Behaviour: b, Now: now})
	assert.Equal(t, Red, row(res, "D21", "").Status)
	assert.Equal(t, Red, row(res, "D25", "").Status, "over the 10 s of the criterion")
}

// The two findings of T15.0c are the product's, and the product is TEO's: they stay red, named,
// until the product is fixed. They block the release; they are not the lab's fault.
func TestProductDefectsStayRedUntilTheProductIsFixed(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Behaviour: behaviourReport(true), Now: now})
	require.Len(t, res.Waiting, 2)
	ids := map[string]Row{}
	for _, w := range res.Waiting {
		ids[w.ID] = w
		assert.Equal(t, Red, w.Status)
		assert.Contains(t, w.Note, "waits for a fix of the product", w.ID)
	}
	assert.Contains(t, ids, "product:freeze-no-limit")
	assert.Contains(t, ids, "product:noise3-pmtu")
	assert.True(t, res.Blocks)
	assert.Equal(t, 1, ExitCode(res, false))

	fixed := Evaluate(Input{Run: goodRun(t), Behaviour: behaviourReport(false), Now: now})
	assert.Empty(t, fixed.Waiting, "when the product is fixed the gates turn green by themselves")
	assert.False(t, fixed.Blocks)

	none := Evaluate(Input{Run: goodRun(t), Now: now})
	assert.Empty(t, none.Waiting, "without a behaviour report there is nothing to say about the product")
	assert.Equal(t, NotMeasured, row(none, "product:freeze-no-limit", "").Status)
	assert.Equal(t, NotMeasured, row(none, "product:noise3-pmtu", "").Status)
}

// ---------------------------------------------------------------------------- the checklist

func TestTheChecklistIsBuiltFromTheNumbers(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Behaviour: behaviourReport(true), Now: now, Commit: "deadbee"})
	require.NotEmpty(t, res.Checklist)
	byText := map[string]Item{}
	for _, it := range res.Checklist {
		byText[it.Text] = it
		assert.NotEmpty(t, it.Evidence, it.Text)
	}
	assert.Equal(t, Green, byText["visibility_pct is 0 %"].Status)
	assert.Equal(t, Green, byText["no detector flags more than 1 % of the browser control"].Status)
	assert.Equal(t, Red, byText["the product is fixed where the lab found it broken (freeze without a limit, PMTU)"].Status)
	assert.Equal(t, Green, byText["no node answers a probe on its own outside the cover (D11-D15)"].Status)

	md := RenderChecklist(res)
	assert.Contains(t, md, "deadbee")
	assert.Contains(t, md, "2026-11-07")
	assert.Contains(t, md, "- [x] visibility_pct is 0 %")
	assert.Contains(t, md, "- [ ] the product is fixed")
	assert.True(t, strings.Contains(md, "blocks the release"), "a red checklist says so")
}

// fullInput is a run with everything the gate can read, all inside the thresholds.
func fullInput(t *testing.T) Input {
	t.Helper()
	run := goodRun(t)
	base := MakeBaseline(*run, map[string]float64{"noise3": 100}, now.Add(-24*time.Hour), "old")
	return Input{Run: run, Behaviour: behaviourReport(false), Baseline: &base, Speed: map[string]float64{"noise3": 99}, Scenarios: both(), Now: now, Commit: "deadbee"}
}

func zeroFPR(in *Input) {
	for id, d := range in.Run.Detectors {
		d.FPR = 0
		in.Run.Detectors[id] = d
	}
}

// M2 of the T15.0e acceptance: green means measured. Only a complete input is green and exits 0.
func TestOnlyAFullyMeasuredRunIsGreen(t *testing.T) {
	res := Evaluate(fullInput(t))
	assert.Equal(t, Yellow, res.Verdict, "D6 and D7 sit in the yellow band of the false-positive rate")
	assert.Equal(t, 0, ExitCode(res, false))

	in := fullInput(t)
	zeroFPR(&in)
	res = Evaluate(in)
	assert.Equal(t, Green, res.Verdict)
	assert.Equal(t, 0, ExitCode(res, false))

	for name, drop := range map[string]func(*Input){
		"no baseline":  func(i *Input) { i.Baseline = nil },
		"no behaviour": func(i *Input) { i.Behaviour = nil },
		"no speed":     func(i *Input) { i.Speed = nil },
		"no probes":    func(i *Input) { i.Run.Probes = nil },
		"no h1a":       func(i *Input) { i.Run.H1a = nil },
	} {
		in := fullInput(t)
		zeroFPR(&in)
		drop(&in)
		res := Evaluate(in)
		assert.Equal(t, NotMeasured, res.Verdict, name)
		assert.Equal(t, 2, ExitCode(res, false), name)
		assert.Equal(t, 0, ExitCode(res, true), name)
	}
}

func TestARunWithNothingMeasuredNeverExitsZero(t *testing.T) {
	run := goodRun(t)
	zero := 0
	for id, d := range run.Detectors {
		d.Recall, d.Positives, d.BrowserNegatives = nil, 0, &zero
		run.Detectors[id] = d
	}
	run.V2Gate, run.H1a, run.Probes = nil, nil, nil
	res := Evaluate(Input{Run: run, Now: now})
	assert.Equal(t, NotMeasured, res.Verdict)
	assert.Equal(t, 2, ExitCode(res, false))
	assert.Equal(t, 2, ExitCode(res, true), "an empty set is not a run, whatever the person says")
}

func TestARedStillBlocksWhateverIsNotMeasured(t *testing.T) {
	in := fullInput(t)
	in.Baseline = nil
	d := in.Run.Detectors["D3"]
	d.FPR = 0.5
	in.Run.Detectors["D3"] = d
	res := Evaluate(in)
	assert.Equal(t, Red, res.Verdict, "red outranks not measured")
	assert.Equal(t, 1, ExitCode(res, true))
}

func d24Status(t *testing.T, at ...time.Duration) Status {
	t.Helper()
	b := behaviourReport(false)
	b.PenaltyNever.Attempts = at
	return row(Evaluate(Input{Run: goodRun(t), Behaviour: b, Now: now}), "D24", "").Status
}

// M1 of the acceptance: D24 reads the lower edge of every interval after the fifth attempt too, or
// a knock every second would pass.
func TestD24ChecksTheLowerEdgeAfterTheFifthAttempt(t *testing.T) {
	m := time.Minute
	hammer := []time.Duration{0, 2 * m, 6 * m, 14 * m, 30 * m, 30*m + time.Second, 30*m + 2*time.Second, 30*m + 3*time.Second}
	assert.Equal(t, Red, d24Status(t, hammer...), "a knock every second after the fifth attempt is not a backoff")
	assert.Equal(t, Red, d24Status(t, 0, 2*m, 6*m, 14*m, 30*m, 40*m, 50*m), "ten minutes apart is too fast")
	assert.Equal(t, Red, d24Status(t, 0, 2*m, 6*m, 14*m, 30*m, 50*m), "twenty minutes after the fifth")
}

func TestD24LowerEdgeHasAToleranceOfAMinute(t *testing.T) {
	m := time.Minute
	assert.Equal(t, Green, d24Status(t, 0, 2*m, 6*m, 14*m, 30*m, 60*m, 90*m, 120*m))
	assert.Equal(t, Green, d24Status(t, 0, 2*m, 6*m, 14*m, 30*m, 59*m, 89*m), "a minute of slack")
	assert.Equal(t, Red, d24Status(t, 0, 2*m, 6*m, 14*m, 30*m, 58*m+59*time.Second), "29 minutes less a second")
	assert.Equal(t, Red, d24Status(t, 0, 2*m, 6*m, 14*m, 30*m, 61*m+time.Second), "30 minutes and a minute and a second: the interval is broken the other way")
}

// L2: the first five attempts may come late, not early.
func TestD24FirstFiveAttemptsMayBeLateNotEarly(t *testing.T) {
	m := time.Minute
	assert.Equal(t, Green, d24Status(t, 0, 3*m, 9*m, 21*m, 51*m), "later than H15 asks is polite, not a violation")
	assert.Equal(t, Red, d24Status(t, 0, 1*m, 6*m, 14*m, 30*m), "the second attempt before the 120 s")
	assert.Equal(t, Red, d24Status(t, 0, 2*m, 6*m, 14*m, 29*m), "the fifth before its 30 minutes")
	assert.Equal(t, Red, d24Status(t, time.Second, 2*m, 6*m, 14*m, 30*m), "the first is at the start")
}

// L4.
func TestD24WithFewerThanFiveAttemptsIsNotMeasured(t *testing.T) {
	m := time.Minute
	for _, at := range [][]time.Duration{nil, {0}, {0, 2 * m, 6 * m, 14 * m}} {
		assert.Equal(t, NotMeasured, d24Status(t, at...), "%d attempts", len(at))
	}
}

// L3: yellow does not block, and the table says so.
func TestThePolicyIsWrittenDown(t *testing.T) {
	res := Evaluate(fullInput(t))
	all := strings.Join(res.Policy, "\n")
	assert.Contains(t, all, "yellow does not block the release")
	assert.Contains(t, all, "M4 audit")
	assert.Contains(t, all, "proposal")
	assert.Contains(t, RenderChecklist(res), "yellow does not block the release")
	assert.False(t, Evaluate(fullInput(t)).Blocks, "and it does not")
}
