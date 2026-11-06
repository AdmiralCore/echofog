package censor

import (
	"math"
	"math/rand"
	"sort"
)

// A small random forest for D9. It needs no GPU and no library: bagged decision trees on six
// numbers per flow, which is the class of model the published flow-physics detectors use.

type tree struct {
	feature int
	thresh  float64
	left    *tree
	right   *tree
	leaf    bool
	p       float64 // share of positives in a leaf
}

type forest struct{ trees []*tree }

const (
	forestTrees = 40
	treeDepth   = 5
	treeMinLeaf = 3
)

func (t *tree) predict(x []float64) float64 {
	for !t.leaf {
		if x[t.feature] <= t.thresh {
			t = t.left
		} else {
			t = t.right
		}
	}
	return t.p
}

func (f *forest) score(x []float64) float64 {
	if f == nil || len(f.trees) == 0 {
		return 0
	}
	var s float64
	for _, t := range f.trees {
		s += t.predict(x)
	}
	return s / float64(len(f.trees))
}

// trainForest fits the forest on rows x with labels y (true = positive), balancing the classes by
// drawing as many negatives as positives into each bag.
func trainForest(x [][]float64, y []bool, seed int64) *forest {
	rng := rand.New(rand.NewSource(seed))
	var pos, neg []int
	for i, v := range y {
		if v {
			pos = append(pos, i)
		} else {
			neg = append(neg, i)
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return nil
	}
	f := &forest{}
	bag := max(len(pos), len(neg))
	for k := 0; k < forestTrees; k++ {
		idx := make([]int, 0, 2*bag)
		for i := 0; i < bag; i++ {
			idx = append(idx, pos[rng.Intn(len(pos))], neg[rng.Intn(len(neg))])
		}
		f.trees = append(f.trees, grow(x, y, idx, 0, rng))
	}
	return f
}

func share(y []bool, idx []int) float64 {
	if len(idx) == 0 {
		return 0
	}
	n := 0
	for _, i := range idx {
		if y[i] {
			n++
		}
	}
	return float64(n) / float64(len(idx))
}

func gini(p float64) float64 { return 2 * p * (1 - p) }

func grow(x [][]float64, y []bool, idx []int, depth int, rng *rand.Rand) *tree {
	p := share(y, idx)
	if depth >= treeDepth || len(idx) < 2*treeMinLeaf || p == 0 || p == 1 {
		return &tree{leaf: true, p: p}
	}
	nf := len(x[idx[0]])
	// each split looks at a random half of the features, which is what makes the trees differ
	feats := rng.Perm(nf)[:max(1, nf/2)]
	bestGain, bestF, bestT := 0.0, -1, 0.0
	parent := gini(p)
	for _, fi := range feats {
		vals := make([]float64, len(idx))
		for j, i := range idx {
			vals[j] = x[i][fi]
		}
		sort.Float64s(vals)
		for j := treeMinLeaf; j <= len(vals)-treeMinLeaf; j += max(1, len(vals)/16) {
			if vals[j-1] == vals[j] {
				continue
			}
			th := (vals[j-1] + vals[j]) / 2
			var l, r []int
			for _, i := range idx {
				if x[i][fi] <= th {
					l = append(l, i)
				} else {
					r = append(r, i)
				}
			}
			if len(l) < treeMinLeaf || len(r) < treeMinLeaf {
				continue
			}
			g := parent - (float64(len(l))*gini(share(y, l))+float64(len(r))*gini(share(y, r)))/float64(len(idx))
			if g > bestGain {
				bestGain, bestF, bestT = g, fi, th
			}
		}
	}
	if bestF < 0 {
		return &tree{leaf: true, p: p}
	}
	var l, r []int
	for _, i := range idx {
		if x[i][bestF] <= bestT {
			l = append(l, i)
		} else {
			r = append(r, i)
		}
	}
	return &tree{feature: bestF, thresh: bestT, left: grow(x, y, l, depth+1, rng), right: grow(x, y, r, depth+1, rng)}
}

// physics are the six numbers of D9 for one flow: mean and spread of the data packet sizes in each
// direction's terms, the mean gap between data packets, the upstream share of the bytes and the
// log of the packet count.
func physics(f LabeledFlow) []float64 {
	var up, dn, sizes, gaps []float64
	var last float64
	for _, p := range f.Pkts {
		if p.Payload == 0 {
			continue
		}
		if p.Up {
			up = append(up, float64(p.Payload))
		} else {
			dn = append(dn, float64(p.Payload))
		}
		sizes = append(sizes, float64(p.Payload))
		t := float64(p.At) / 1e6
		if last != 0 {
			gaps = append(gaps, t-last)
		}
		last = t
	}
	mean := func(a []float64) float64 {
		if len(a) == 0 {
			return 0
		}
		var s float64
		for _, v := range a {
			s += v
		}
		return s / float64(len(a))
	}
	m := mean(sizes)
	var sd float64
	for _, v := range sizes {
		sd += (v - m) * (v - m)
	}
	if len(sizes) > 1 {
		sd = math.Sqrt(sd / float64(len(sizes)-1))
	}
	upBytes, dnBytes := 0.0, 0.0
	for _, v := range up {
		upBytes += v
	}
	for _, v := range dn {
		dnBytes += v
	}
	share := 0.0
	if upBytes+dnBytes > 0 {
		share = upBytes / (upBytes + dnBytes)
	}
	return []float64{mean(up), mean(dn), sd, mean(gaps), share, math.Log1p(float64(len(sizes)))}
}
