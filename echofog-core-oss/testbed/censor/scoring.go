package censor

import (
	"math"
	"math/rand"
	"sort"
)

// KS is the two-sample Kolmogorov–Smirnov distance between a and b: the largest gap between their
// empirical distribution functions. Two empty sets are the same; one empty set is as far as it gets.
func KS(a, b []float64) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 || len(b) == 0 {
		return 1
	}
	x, y := sortedCopy(a), sortedCopy(b)
	return ksSorted(x, y)
}

func sortedCopy(a []float64) []float64 {
	x := append([]float64(nil), a...)
	sort.Float64s(x)
	return x
}

// ksSorted is KS for inputs that are already sorted and not empty.
func ksSorted(x, y []float64) float64 {
	var i, j int
	var d float64
	nx, ny := float64(len(x)), float64(len(y))
	for i < len(x) && j < len(y) {
		v := math.Min(x[i], y[j])
		for i < len(x) && x[i] == v {
			i++
		}
		for j < len(y) && y[j] == v {
			j++
		}
		if g := math.Abs(float64(i)/nx - float64(j)/ny); g > d {
			d = g
		}
	}
	return d
}

// AUC is the probability that a random positive scores above a random negative (ties count half).
// Without both classes it says 0.5: it knows nothing.
func AUC(pos, neg []float64) float64 {
	if len(pos) == 0 || len(neg) == 0 {
		return 0.5
	}
	n := sortedCopy(neg)
	var sum float64
	for _, p := range pos {
		below := sort.SearchFloat64s(n, p)                                // negatives < p
		upto := sort.Search(len(n), func(i int) bool { return n[i] > p }) // negatives <= p
		sum += float64(below) + 0.5*float64(upto-below)
	}
	return sum / (float64(len(pos)) * float64(len(neg)))
}

// TPRAtFPR is the share of positives scoring above the lowest threshold that lets at most fpr of
// the negatives score above it.
func TPRAtFPR(pos, neg []float64, fpr float64) float64 {
	if len(pos) == 0 {
		return 0
	}
	thr := math.Inf(-1)
	if len(neg) > 0 {
		n := sortedCopy(neg)
		allowed := int(math.Floor(fpr * float64(len(n))))
		// the (allowed+1)-th highest negative: at most `allowed` negatives lie above it
		idx := len(n) - 1 - allowed
		if idx < 0 {
			idx = 0
			thr = math.Inf(-1)
		} else {
			thr = n[idx]
		}
	}
	hit := 0
	for _, p := range pos {
		if p > thr {
			hit++
		}
	}
	return float64(hit) / float64(len(pos))
}

// CalibrateKS returns the KS distance a browser flow may have to the rest of the browser control
// without being flagged: the (1 - fpr) quantile of each flow's distance to all the others. The
// threshold is a property of the control, not of the traffic being judged.
func CalibrateKS(sets [][]float64, fpr float64) float64 {
	n := len(sets)
	if n < 2 {
		return 1
	}
	sorted := make([][]float64, n)
	for i, s := range sets {
		sorted[i] = sortedCopy(s)
	}
	dists := make([]float64, 0, n)
	for i := range sets {
		var rest []float64
		for j := range sorted {
			if j != i {
				rest = append(rest, sorted[j]...)
			}
		}
		if len(rest) == 0 || len(sorted[i]) == 0 {
			continue
		}
		sort.Float64s(rest)
		dists = append(dists, ksSorted(sorted[i], rest))
	}
	if len(dists) == 0 {
		return 1
	}
	sort.Float64s(dists)
	idx := int(math.Ceil((1-fpr)*float64(len(dists)))) - 1
	idx = max(0, min(idx, len(dists)-1))
	return dists[idx]
}

// recordSizes lists the sizes of a flow's records (the first 16 KB each way); the sign is the
// direction: positive towards the server, negative towards the client.
func recordSizes(f Flow) []float64 {
	out := make([]float64, 0, len(f.Records))
	for _, r := range f.Records {
		if r.Up {
			out = append(out, float64(r.Len))
		} else {
			out = append(out, -float64(r.Len))
		}
	}
	return out
}

// recordGaps lists the milliseconds between consecutive records.
func recordGaps(f Flow) []float64 {
	if len(f.Records) < 2 {
		return nil
	}
	out := make([]float64, 0, len(f.Records)-1)
	for i := 1; i < len(f.Records); i++ {
		out = append(out, float64(f.Records[i].At-f.Records[i-1].At)/1e6)
	}
	return out
}

// pooled concatenates the size sets.
func pooled(sets [][]float64) []float64 {
	var out []float64
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// nullMax is the largest KS distance between pools of n flows and the rest of the control over
// draws random splits: how far two samples of one distribution wander apart. It is the threshold
// of a pooled comparison (H1a); a fixed seed keeps the report repeatable.
func nullMax(sets [][]float64, n, draws int) float64 {
	if len(sets) < 2 || n < 1 || n >= len(sets) {
		return 1
	}
	rng := rand.New(rand.NewSource(1))
	var worst float64
	for d := 0; d < draws; d++ {
		perm := rng.Perm(len(sets))
		var a, b []float64
		for k, idx := range perm {
			if k < n {
				a = append(a, sets[idx]...)
			} else {
				b = append(b, sets[idx]...)
			}
		}
		if len(a) == 0 || len(b) == 0 {
			continue
		}
		if v := KS(a, b); v > worst {
			worst = v
		}
	}
	return worst
}
