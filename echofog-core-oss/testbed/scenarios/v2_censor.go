package scenarios

import (
	"context"
	"fmt"

	"github.com/echofog/echofog-core/testbed/gate"
)

func censorScenario() Scenario {
	return Scenario{Name: "v2-censor", Title: "Censor: four carriers, the classifier does not single them out, the probe sees the cover", Live: liveCensor, Run: runCensor}
}

// runCensor puts the real listeners and dialers of the four carriers on loopback behind a capture
// tap, loads pages through each, reads the pcap with the censor's analyzer, runs the
// detectors on the labelled flows against a browser control, and runs the active prober against
// each listener. A row is green only for what was measured within its limit.
func runCensor(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-censor", Mode: "model", Seed: env.Seed}
	cfg, cfgNote := censorConfig()
	if cfgNote != "" {
		res.NotChecked = append(res.NotChecked, cfgNote)
	}
	res.NotChecked = append(res.NotChecked,
		"the tap is a model of the wire: fake addresses, loopback timing, one pure ACK per burst; D6 (cross-layer RTT) and D9 read loopback timing, not a real path; no DPI box, no real operator network (T15.2)")

	const bulk = 0 // see censor_load.go: pages only
	browser, berr := captureBrowser(ctx, 20, env.Seed)
	if berr != nil {
		res.NotChecked = append(res.NotChecked, "browser control: "+berr.Error())
	}

	for _, c := range []struct {
		name string
		run  func(context.Context, int, int) capturedCarrier
		n    int
	}{
		{"noise3", captureNoise3, 40},
		{"reality2-quic", captureReality2QUIC, 12},
		{"reality2-tcp", captureReality2TCP, 24},
		{"ws2", captureWS2, 24},
	} {
		cc := c.run(ctx, c.n, bulk)
		res.NotChecked = append(res.NotChecked, cc.NotChecked...)
		if cc.Err != nil {
			why := fmt.Sprintf("the carrier run did not complete: %v", cc.Err)
			res.NotChecked = append(res.NotChecked, c.name+": "+why)
			res.Rows = append(res.Rows,
				newRow("v2-censor/"+c.name+"/classifier", "classifier", "not run", classifierLimit, gate.NotMeasured, why),
				newRow("v2-censor/"+c.name+"/probe", "active probe", "not run", probeLimit, gate.NotMeasured, why))
			continue
		}
		crow, cfind := classifierRow(c.name, cc.Flows, browser, cfg)
		prow, pfind := probeRow(c.name, cc.Probe)
		res.Rows = append(res.Rows, crow, prow)
		for _, f := range []string{cfind, pfind} {
			if f != "" {
				res.Findings = append(res.Findings, f)
			}
		}
	}
	if berr != nil {
		res.Rows = append(res.Rows, newRow("v2-censor/control", "browser control", "not run", controlLimit, gate.NotMeasured, berr.Error()))
	} else {
		res.Rows = append(res.Rows, controlRow(browser, cfg))
	}
	return res
}
