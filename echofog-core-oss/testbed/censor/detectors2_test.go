package censor

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metricsFor(flows []LabeledFlow, cfg DetectorConfig) map[string]DetectorMetric {
	return EvaluateDetectors(flows, cfg)
}

func hasNote(m DetectorMetric, sub string) bool {
	for _, n := range m.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------- scoring

func TestKSDistance(t *testing.T) {
	cases := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"same", []float64{1, 2, 3, 4}, []float64{1, 2, 3, 4}, 0},
		{"disjoint", []float64{1, 2, 3}, []float64{10, 11, 12}, 1},
		{"half shifted", []float64{1, 2, 3, 4}, []float64{3, 4, 5, 6}, 0.5},
		{"empty", nil, []float64{1}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, KS(tc.a, tc.b), 1e-9)
			assert.InDelta(t, KS(tc.b, tc.a), KS(tc.a, tc.b), 1e-12, "symmetric")
		})
	}
}

func TestAUCAndTPRAtFPR(t *testing.T) {
	assert.InDelta(t, 1.0, AUC([]float64{5, 6, 7}, []float64{1, 2, 3}), 1e-9)
	assert.InDelta(t, 0.0, AUC([]float64{1, 2}, []float64{5, 6}), 1e-9)
	assert.InDelta(t, 0.5, AUC([]float64{3}, []float64{3}), 1e-9, "a tie counts half")

	neg := make([]float64, 100)
	for i := range neg {
		neg[i] = float64(i)
	}
	pos := []float64{50, 98.5, 99.5, 150}
	// 1% of 100 negatives is one: the threshold sits at 98, so only the negative 99 scores above it
	// and three of the four positives do
	assert.InDelta(t, 0.75, TPRAtFPR(pos, neg, 0.01), 1e-9)
	assert.InDelta(t, 0.0, TPRAtFPR(nil, neg, 0.01), 1e-9)
}

func TestCalibrateKSIsTheBrowserQuantile(t *testing.T) {
	var sets [][]float64
	for i := 0; i < 200; i++ {
		f := synthFlow(kBrowser, int64(i+1), i)
		sets = append(sets, recordSizes(f.Flow))
	}
	thr := CalibrateKS(sets, 0.01)
	assert.Greater(t, thr, 0.0)
	assert.Less(t, thr, 1.0, "browsers are not all as far from each other as possible")
	over := 0
	pool := pooled(sets)
	for _, s := range sets {
		if KS(s, pool) > thr {
			over++
		}
	}
	assert.LessOrEqual(t, over, 4, "about 1% of the control may be above the threshold it set")
}

// ---------------------------------------------------------------------------- D5

func TestD5CatchesATLSInsideTLSAndLeavesBrowsersAlone(t *testing.T) {
	flows := append(synthSet(kTunnel, 100, 100), synthSet(kBrowser, 9000, 400)...)
	m := metricsFor(flows, DetectorConfig{})["D5"]
	require.NotNil(t, m.Recall)
	assert.GreaterOrEqual(t, *m.Recall, 0.90, "recall on nested TLS (gate H1a, synthetic side)")
	assert.LessOrEqual(t, m.FPR, 0.01, "FPR on browsers")
	assert.Equal(t, 100, m.Positives)
	assert.Equal(t, 400, m.BrowserNegatives)
}

func TestD5DoesNotApplyToUDPOrShortFlows(t *testing.T) {
	f := synthFlow(kTunnel, 1, 1)
	f.Proto = "udp"
	assert.False(t, tlsInTLSDetector{}.Applies(f))
	g := synthFlow(kTunnel, 1, 2)
	g.Records = g.Records[:3]
	assert.False(t, tlsInTLSDetector{}.Applies(g))
	assert.True(t, tlsInTLSDetector{}.Applies(synthFlow(kTunnel, 1, 3)))
}

// ---------------------------------------------------------------------------- D6

func TestD6SeparatesAProxyWithAFarSideFromABrowser(t *testing.T) {
	flows := append(synthSet(kTunnel, 100, 100), synthSet(kBrowser, 9000, 300)...)
	m := metricsFor(flows, DetectorConfig{})["D6"]
	require.NotNil(t, m.AUC)
	assert.GreaterOrEqual(t, *m.AUC, 0.90, "the detector has to work on a naive proxy, or its AUC on v2 means nothing")
	require.NotNil(t, m.TPRAt1)
}

func TestD6CannotSeparateAnExitThatAnswersFromTheEdge(t *testing.T) {
	flows := append(synthSet(kTunnelEdge, 100, 100), synthSet(kBrowser, 9000, 300)...)
	var pos, neg []float64
	d := crossLayerRTTDetector{}
	for _, f := range flows {
		if !d.Applies(f) {
			continue
		}
		if f.Label == LabelBrowser {
			neg = append(neg, d.Score(f, DetectorConfig{}))
		} else {
			pos = append(pos, d.Score(f, DetectorConfig{}))
		}
	}
	require.NotEmpty(t, pos)
	require.NotEmpty(t, neg)
	assert.LessOrEqual(t, AUC(pos, neg), 0.65, "gate: D6 AUC on v2-like traffic")
}

func TestD6ScoreIsTheExtraLatencyOfTheAnswer(t *testing.T) {
	far := synthFlow(kTunnel, 5, 1)
	near := synthFlow(kBrowser, 5, 2)
	d := crossLayerRTTDetector{}
	assert.Greater(t, d.Score(far, DetectorConfig{}), d.Score(near, DetectorConfig{}))
	assert.GreaterOrEqual(t, d.Score(near, DetectorConfig{}), 0.0)
}

// ---------------------------------------------------------------------------- D7

func TestD7FlagsFixedSizeRecordsAndCalibratesOnTheBrowserControl(t *testing.T) {
	flows := append(synthSet(kConstSize, 100, 50), synthSet(kBrowser, 9000, 400)...)
	m := metricsFor(flows, DetectorConfig{})["D7"]
	require.NotNil(t, m.Recall)
	assert.GreaterOrEqual(t, *m.Recall, 0.90)
	assert.LessOrEqual(t, m.FPR, 0.02, "the threshold is the browser quantile, so FPR is what it was set to")
	assert.True(t, hasNote(m, "KS threshold"), "the calibrated threshold is written down as a proposal: %v", m.Notes)
}

func TestD7UsesTheConfiguredThreshold(t *testing.T) {
	f := synthFlow(kConstSize, 3, 1)
	set := synthSet(kBrowser, 9000, 50)
	d := &sizeTimingDetector{}
	cfg := DetectorConfig{KSThreshold: 0.999}
	d.Begin(append(set, f), cfg)
	assert.False(t, d.Flag(f, cfg))
	cfg.KSThreshold = 0.2
	d.Begin(append(set, f), cfg)
	assert.True(t, d.Flag(f, cfg))
}

// ---------------------------------------------------------------------------- D8

func TestD8FindsBulkTunnelsAndStormsNotChatsOrBrowsers(t *testing.T) {
	flows := append(synthSet(kBulk, 100, 10), stormFlows(30)...)
	flows = append(flows, synthSet(kBrowser, 9000, 300)...)
	chat := synthFlow(kBrowser, 77, 900)
	chat.Duration, chat.BytesUp, chat.BytesDn = 30*time.Minute, 1<<10, 1<<20 // long but light
	flows = append(flows, chat)
	m := metricsFor(flows, DetectorConfig{})["D8"]
	require.NotNil(t, m.Recall)
	assert.InDelta(t, 1.0, *m.Recall, 1e-9)
	assert.Equal(t, 0.0, m.FPR)
	assert.False(t, flows[len(flows)-1].Flags["D8"])
}

func TestD8CountsConnectionsPerAddressInAWindow(t *testing.T) {
	few := stormFlows(8) // 8 in 2.4 s is a busy page, not a storm
	for i := range few {
		few[i].Suite = ""
	}
	metricsFor(few, DetectorConfig{})
	for _, f := range few {
		assert.False(t, f.Flags["D8"])
	}
	many := stormFlows(30)
	metricsFor(many, DetectorConfig{})
	flagged := 0
	for _, f := range many {
		if f.Flags["D8"] {
			flagged++
		}
	}
	assert.GreaterOrEqual(t, flagged, 20, "the flows past the limit are the storm")
}

// ---------------------------------------------------------------------------- D9

func TestD9ReportsTPRAtFPRAndIsOutsideTheGate(t *testing.T) {
	flows := append(synthSet(kTunnel, 100, 60), synthSet(kConstSize, 300, 30)...)
	flows = append(flows, synthSet(kBulk, 500, 10)...)
	flows = append(flows, synthSet(kBrowser, 9000, 400)...)
	m := metricsFor(flows, DetectorConfig{Seed: 7})["D9"]
	require.NotNil(t, m.AUC)
	require.NotNil(t, m.TPRAt1)
	assert.Greater(t, *m.AUC, 0.8, "the forest should see what a human sees in these flows")
	assert.True(t, hasNote(m, "outside the gate"), "%v", m.Notes)

	bad := map[string]DetectorMetric{"D9": {FPR: 1}}
	assert.Empty(t, GateFailures(bad), "the gate does not look at D9, whatever it does")
}

func TestD9IsDeterministicForASeed(t *testing.T) {
	mk := func() []LabeledFlow {
		return append(synthSet(kTunnel, 100, 30), synthSet(kBrowser, 9000, 120)...)
	}
	a := metricsFor(mk(), DetectorConfig{Seed: 3})["D9"]
	b := metricsFor(mk(), DetectorConfig{Seed: 3})["D9"]
	require.NotNil(t, a.AUC)
	require.NotNil(t, b.AUC)
	assert.Equal(t, *a.AUC, *b.AUC)
}

// ---------------------------------------------------------------------------- the run

func TestEvaluateCoversD1ToD10AndOneVerdictPerFlow(t *testing.T) {
	flows := append(synthSet(kTunnel, 100, 20), synthSet(kBrowser, 9000, 60)...)
	m := metricsFor(flows, DetectorConfig{})
	for _, id := range []string{"D1", "D2", "D3", "D4", "D5", "D6", "D7", "D8", "D9", "D10"} {
		_, ok := m[id]
		assert.True(t, ok, id)
	}
	for _, f := range flows {
		v := FinalVerdict(f)
		require.NotEmpty(t, v)
		assert.Equal(t, v, FinalVerdict(f), "one flow, one verdict, every time")
	}
	var tun LabeledFlow
	for _, f := range flows {
		if f.Suite == SuiteTLSInTLS && f.Flags["D5"] {
			tun = f
			break
		}
	}
	require.NotEmpty(t, tun.Suite, "at least one nested TLS flow is flagged by D5")
	assert.Contains(t, FinalVerdict(tun), "AT RISK")
	assert.Contains(t, FinalVerdict(tun), "D5")
}

func TestExposureCountsOnlyTheRealFlowsNotTheControls(t *testing.T) {
	flows := synthSet(kTunnel, 100, 10) // synthetic controls, labelled carrier
	real := synthSet(kTunnelEdge, 700, 10)
	for i := range real {
		real[i].Suite, real[i].Synthetic = "reality2-tcp", false
	}
	flows = append(flows, real...)
	flows = append(flows, synthSet(kBrowser, 9000, 100)...)
	metricsFor(flows, DetectorConfig{})
	flagged, n := Exposure(flows, LabelCarrier, "D5")
	assert.Equal(t, 10, n, "synthetic controls are not v2 traffic")
	assert.GreaterOrEqual(t, flagged, 0)
	assert.LessOrEqual(t, flagged, n)
}

func TestGateFailuresAreTheTwoSidedRulesOnly(t *testing.T) {
	rec := func(v float64) *float64 { return &v }
	ok := map[string]DetectorMetric{
		"D1": {Recall: rec(1), FPR: 0.005}, "D5": {Recall: rec(0.95), FPR: 0.005},
		"D6": {AUC: rec(0.5)},
	}
	assert.Empty(t, GateFailures(ok))
	bad := map[string]DetectorMetric{
		"D4": {FPR: 0.05}, "D5": {Recall: rec(0.5), FPR: 0},
		"D6": {AUC: rec(0.9)},
	}
	f := GateFailures(bad)
	require.Len(t, f, 3)
	all := strings.Join(f, "\n")
	for _, id := range []string{"D4", "D5", "D6"} {
		assert.Contains(t, all, id)
	}
}

// ---------------------------------------------------------------------------- the v2 side of the gate

func asV2(ls []LabeledFlow, suite string) []LabeledFlow {
	for i := range ls {
		ls[i].Suite, ls[i].Synthetic, ls[i].Label = suite, false, LabelCarrier
	}
	return ls
}

func rowOf(r V2GateResult, id string) V2GateRow {
	for _, x := range r.Rows {
		if x.Detector == id {
			return x
		}
	}
	return V2GateRow{}
}

func TestV2GatePassesAnExitThatLooksLikeABrowser(t *testing.T) {
	flows := append(asV2(synthSet(kBrowser, 20000, 60), "reality2-tcp"), synthSet(kBrowser, 9000, 300)...)
	res := V2Gate(flows, DetectorConfig{})
	assert.Empty(t, res.Failures)
	for _, id := range []string{"D5", "D6", "D7", "D8"} {
		r := rowOf(res, id)
		assert.True(t, r.Measured, id)
		assert.True(t, r.Pass, id)
	}
	assert.Equal(t, 60, rowOf(res, "D5").N)
	require.NotNil(t, rowOf(res, "D6").AUC)
	assert.LessOrEqual(t, *rowOf(res, "D6").AUC, 0.65)
}

func TestV2GateFailsOnANaiveTunnelAndNamesTheDetectors(t *testing.T) {
	flows := append(asV2(synthSet(kTunnel, 100, 60), "reality2-tcp"), synthSet(kBrowser, 9000, 300)...)
	res := V2Gate(flows, DetectorConfig{})
	require.NotEmpty(t, res.Failures)
	all := strings.Join(res.Failures, "\n")
	assert.Contains(t, all, "D5")
	assert.Contains(t, all, "D6")
	assert.Greater(t, rowOf(res, "D5").Share, 0.10)
	assert.False(t, rowOf(res, "D5").Pass)
}

func TestV2GateSaysNotMeasuredWhenNoV2FlowIsInScope(t *testing.T) {
	udp := synthSet(kBrowser, 20000, 20)
	for i := range udp {
		udp[i].Proto = "udp"
		udp[i].Records, udp[i].Pkts = nil, nil
	}
	flows := append(asV2(udp, "noise3"), synthSet(kBrowser, 9000, 100)...)
	res := V2Gate(flows, DetectorConfig{})
	r := rowOf(res, "D5")
	assert.False(t, r.Measured, "a UDP carrier says nothing to a TCP detector")
	assert.False(t, r.Pass, "not measured is not a pass")
	assert.Empty(t, res.Failures, "and not a failure either")
}

func TestV2GateIgnoresTheSyntheticControls(t *testing.T) {
	flows := append(synthSet(kTunnel, 100, 60), synthSet(kBrowser, 9000, 300)...)
	res := V2Gate(flows, DetectorConfig{})
	assert.Equal(t, 0, rowOf(res, "D5").N)
	assert.Empty(t, res.Failures)
}
