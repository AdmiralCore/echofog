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
		crow, cfind := classifierRowWith(c.name, cc.Flows, browser, cfg, inconclusiveFor(c.name))
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

// inconclusiveFor names the detectors whose reference or control cannot judge a carrier yet. They
// are an artifact of the lab, not a finding of the product (acceptance of T15.1):
//   - D7 compares record sizes and gaps with a control of 20 short GETs; reality2-tcp and ws2 carry
//     long-lived multiplexed sessions, which a control of short pages does not represent (the control
//     needs a streaming flow and at least 60 flows);
//   - D6 (cross-layer round trip) reads the timing of the model and is noise on this many flows;
//   - D4 compares the outer ClientHello with the browser profiles; ws2 offers http/1.1 only, as a
//     WebSocket dial must, and browserprofiles holds no Chrome http/1.1 hello (it needs a real
//     capture, T15.2).
func inconclusiveFor(carrier string) map[string]string {
	// the same input gives an AUC from 0.37 to 0.93 between runs: the detector reads the timing of the
	// loopback model on 24 flows against 20 control flows, not a real path
	const d6 = "reads the timing of the loopback model: the AUC of the same input varies from 0.37 to 0.93 (a real path in T15.2)"
	const d7 = "the control is 20 short GETs: no streaming flow, fewer than 60 flows"
	switch carrier {
	case "reality2-tcp":
		return map[string]string{"D7": d7, "D6": d6}
	case "ws2":
		return map[string]string{"D7": d7, "D6": d6, "D4": "browserprofiles holds no Chrome http/1.1 JA4 (a real capture is needed, T15.2)"}
	}
	return nil
}
