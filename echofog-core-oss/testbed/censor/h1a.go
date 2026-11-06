package censor

// H1aResult is the measurement of H1a: how far the external records of the first 16 KB of an
// exit flow are from the browser control.
type H1aResult struct {
	KS        float64 `json:"ks"`        // distance of the pooled exit records to the pooled browser records
	Threshold float64 `json:"threshold"` // what the browser control allows (CalibrateKS)
	Flows     int     `json:"flows"`
	Pass      bool    `json:"pass"`
}

// H1aMeasure compares the record sizes of the exit flows (what a border sees of the first 16 KB)
// with the browser control. The padding is T10.1/T10.2's; this only measures.
func H1aMeasure(exit, browser []Flow) H1aResult { return H1aResult{} }
