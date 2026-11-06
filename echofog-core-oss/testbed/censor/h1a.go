package censor

// H1aResult is the measurement of H1a: how far the external records of the first 16 KB of an
// exit flow are from the browser control.
type H1aResult struct {
	KS        float64 `json:"ks"`        // distance of the pooled exit records to the pooled browser records
	Threshold float64 `json:"threshold"` // how far two samples of the browser control itself wander apart
	Flows     int     `json:"flows"`
	Pass      bool    `json:"pass"`
}

// h1aDraws is how many random splits of the browser control set the threshold.
const h1aDraws = 200

// H1aMeasure compares the record sizes of the exit flows (what a border sees of the first 16 KB)
// with the browser control. The threshold is the largest distance between a sample of the control
// of the same number of flows and the rest of it, so it is a property of the control and of the
// sample size, not a number picked to pass. The padding is T10.1/T10.2's; this only measures.
// Without a browser control (fewer than two flows more than the exit has) nothing is a pass.
func H1aMeasure(exit, browser []Flow) H1aResult {
	res := H1aResult{Flows: len(exit)}
	var exitSets, browserSets [][]float64
	for _, f := range exit {
		exitSets = append(exitSets, recordSizes(f))
	}
	for _, f := range browser {
		browserSets = append(browserSets, recordSizes(f))
	}
	pe, pb := pooled(exitSets), pooled(browserSets)
	if len(pe) == 0 || len(pb) == 0 || len(browser) <= len(exit)+1 {
		res.KS, res.Threshold = KS(pe, pb), 1
		return res
	}
	res.KS = KS(pe, pb)
	res.Threshold = nullMax(browserSets, len(exit), h1aDraws)
	res.Pass = res.KS <= res.Threshold
	return res
}
