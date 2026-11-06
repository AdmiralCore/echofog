package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/censor"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// labRun starts the lab server on loopback and gives the test a way to record what a client mode
// puts on the wire.
type labRun struct {
	addr string
	mu   sync.Mutex
	recs []*censor.Recorder
}

func startLab(t *testing.T) *labRun {
	t.Helper()
	r := &labRun{addr: freeAddr(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveCtx(ctx, r.addr, "") }()
	t.Cleanup(func() {
		cancel()
		<-done
		recordConn = func(c net.Conn) net.Conn { return c }
	})
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", r.addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recordConn = func(c net.Conn) net.Conn {
		rec := censor.NewRecorder(c, time.Now())
		r.mu.Lock()
		r.recs = append(r.recs, rec)
		r.mu.Unlock()
		return rec
	}
	return r
}

// take returns the flows recorded since the last call.
func (r *labRun) take() []censor.Flow {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []censor.Flow
	for _, rec := range r.recs {
		out = append(out, rec.Flow())
	}
	r.recs = nil
	return out
}

func label(fs []censor.Flow, l censor.Label, suite string) []censor.LabeledFlow {
	out := make([]censor.LabeledFlow, len(fs))
	for i, f := range fs {
		out[i] = censor.LabeledFlow{Flow: f, Label: l, Suite: suite, Synthetic: l == censor.LabelCarrier}
		// each lab connection is its own flow; give them distinct start times like a real run
		out[i].Start = time.Unix(1_700_000_000, 0).Add(time.Duration(i) * 5 * time.Second)
	}
	return out
}

// Real traffic, not generators: Go's crypto/tls nested in itself, and a Chrome-preset client making
// HTTP requests, both through the lab server on loopback. D5 has to find the first and leave the
// second alone.
func TestD5SeesRealNestedTLSAndNotARealBrowserVisit(t *testing.T) {
	lab := startLab(t)

	res, err := labVisit(lab.addr, "cover.lab", 40, 1)
	require.NoError(t, err)
	assert.Equal(t, 40, res.Connections)
	browser := label(lab.take(), censor.LabelBrowser, "utls-chrome-visit")
	require.GreaterOrEqual(t, len(browser), 40)

	var tunnels []censor.Flow
	for i := 0; i < 30; i++ {
		_, err := tlsInTLS(lab.addr, "cover.lab", int64(i+1))
		require.NoError(t, err)
		tunnels = append(tunnels, lab.take()...)
	}
	tun := label(tunnels, censor.LabelCarrier, censor.SuiteTLSInTLS)

	m := censor.EvaluateDetectors(append(tun, browser...), censor.DetectorConfig{})["D5"]
	require.NotNil(t, m.Recall)
	t.Logf("D5 on real traffic: recall %.2f (%d), FPR %.3f (%d browser flows)", *m.Recall, m.Positives, m.FPR, m.BrowserNegatives)
	assert.GreaterOrEqual(t, *m.Recall, 0.90, "real nested TLS")
	assert.LessOrEqual(t, m.FPR, 0.01, "real browser visits")
}

func TestD7SeesRealFixedSizeRecords(t *testing.T) {
	lab := startLab(t)
	_, err := labVisit(lab.addr, "cover.lab", 30, 2)
	require.NoError(t, err)
	browser := label(lab.take(), censor.LabelBrowser, "utls-chrome-visit")

	var fixed []censor.Flow
	for i := 0; i < 8; i++ {
		_, err := constSize(lab.addr, "cover.lab", int64(i+1))
		require.NoError(t, err)
		fixed = append(fixed, lab.take()...)
	}
	m := censor.EvaluateDetectors(append(label(fixed, censor.LabelCarrier, censor.SuiteConstSize), browser...), censor.DetectorConfig{})["D7"]
	require.NotNil(t, m.Recall)
	t.Logf("D7 on real traffic: recall %.2f, FPR %.3f", *m.Recall, m.FPR)
	assert.GreaterOrEqual(t, *m.Recall, 0.90)
	assert.LessOrEqual(t, m.FPR, 0.05, "30 browser flows calibrate a coarse threshold")
}

func TestConnStormOpensManyConnectionsQuickly(t *testing.T) {
	lab := startLab(t)
	res, err := connStorm(lab.addr, "cover.lab", 25)
	require.NoError(t, err)
	assert.Equal(t, 25, res.Connections)
	assert.Len(t, lab.take(), 25)
}

func TestVisitsHaveVaryingAnswerSizes(t *testing.T) {
	lab := startLab(t)
	_, err := labVisit(lab.addr, "cover.lab", 6, 3)
	require.NoError(t, err)
	sizes := map[int]bool{}
	for _, f := range lab.take() {
		sizes[f.PayDn] = true
	}
	assert.Greater(t, len(sizes), 3, "a browser control whose every visit is the same size is not a control")
}
