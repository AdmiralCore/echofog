package censor

// KS is the two-sample Kolmogorov–Smirnov distance between a and b.
func KS(a, b []float64) float64 { return 0 }

// AUC is the probability that a random positive scores above a random negative (ties count half).
func AUC(pos, neg []float64) float64 { return 0 }

// TPRAtFPR is the share of positives above the lowest threshold that lets at most fpr of the
// negatives through.
func TPRAtFPR(pos, neg []float64, fpr float64) float64 { return 0 }

// CalibrateKS returns the KS distance a browser flow may have to the rest of the browser control
// without being flagged: the (1 - fpr) quantile of each flow's distance to all the others.
func CalibrateKS(sets [][]float64, fpr float64) float64 { return 1 }

// recordSizes lists the sizes of a flow's records (the first 16 KB each way).
func recordSizes(f Flow) []float64 { return nil }

// pooled concatenates the size sets.
func pooled(sets [][]float64) []float64 { return nil }
