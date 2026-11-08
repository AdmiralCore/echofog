package main

// v2-* scenarios: the launchpad's single entry point for the v2 protocol. The phases are censor,
// net and services; one run writes one testbed/out/<time>-<scenario>/ with report.json, flows.csv
// and summary.txt, and a copy in testbed/results/ for compare.
//
// The v2 carrier is not in the v2 branch yet (T0.3 gives efv2, T2.4 gives noise3), so the censor
// phase uses testbed/v2carrier, a separate binary, in an explicitly synthetic udp-noise mode plus
// the v1 product flows. The capture -> budget -> detectors path does not change when the real
// carrier lands.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/testbed/browserprofiles"
	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/probe"
)

const webIP = "203.0.113.30"

type byteSizeFlag struct{ v int64 }

func (b *byteSizeFlag) String() string { return humanBytes(b.v) }
func (b *byteSizeFlag) Set(s string) error {
	v, err := parseByteSize(s)
	if err != nil {
		return err
	}
	b.v = v
	return nil
}

var (
	v2Phases     = flag.String("phases", "", "v2: comma-separated phases (censor,net,services,echo,probe); default depends on the scenario")
	v2Profile    = flag.String("profile", "desktop", "v2: desktop or android")
	v2Network    = flag.String("network", "lab", "v2: lab, ru-mobile, cn or ir")
	v2Pcap       = flag.Bool("pcap", false, "v2: keep the packet capture until the analysis is green (it is deleted after)")
	v2PcapBudget = &byteSizeFlag{v: maxPcapBudget}
	v2EchoBytes  = flag.Int64("v2-echo-bytes", 1<<30, "v2-basic: bytes sent through the echo stream (default 1 GiB)")
	v2ProbeSpec  = flag.String("probe-spec", "", "v2: JSON spec of the nodes the probe phase asks (D11-D15, from gw_b); see testbed/probe")
)

func init() {
	flag.Var(v2PcapBudget, "pcap-budget", "v2: total packet-capture budget per run (default and maximum 2GB)")
}

func isV2Scenario(s string) bool { return strings.HasPrefix(s, "v2-") }

func validateV2Flags(scenario string) error {
	if !isV2Scenario(scenario) || isV2LiveScenario(scenario) {
		return nil // the live scenarios of T15.1 take none of the launchpad flags (v2live.go)
	}
	switch *v2Profile {
	case "desktop", "android":
	default:
		return fmt.Errorf("profile %q: want desktop or android", *v2Profile)
	}
	switch *v2Network {
	case "lab", "ru-mobile", "cn", "ir":
	default:
		return fmt.Errorf("network %q: want lab, ru-mobile, cn or ir", *v2Network)
	}
	if err := validatePcapBudget(v2PcapBudget.v); err != nil {
		return err
	}
	if _, err := v2PhaseList(scenario); err != nil {
		return err
	}
	if *v2EchoBytes <= 0 {
		return fmt.Errorf("-v2-echo-bytes %d: want a positive count", *v2EchoBytes)
	}
	return nil
}

func v2PhaseList(scenario string) ([]string, error) {
	var phases []string
	if *v2Phases != "" {
		phases = strings.Split(*v2Phases, ",")
	} else {
		switch scenario {
		case "v2-basic":
			phases = []string{"echo"}
		case "v2-censor", "v2-carrier":
			phases = []string{"censor"}
		default:
			return nil, fmt.Errorf("unknown v2 scenario %q (v2-basic, v2-censor, v2-carrier)", scenario)
		}
	}
	for i := range phases {
		phases[i] = strings.TrimSpace(phases[i])
		switch phases[i] {
		case "censor", "net", "services", "echo":
		case "probe":
			if *v2ProbeSpec == "" && !*v2LiveLab {
				return nil, fmt.Errorf("phase probe needs -probe-spec: the nodes to ask are not known to the lab")
			}
		default:
			return nil, fmt.Errorf("unknown phase %q (censor, net, services, echo, probe)", phases[i])
		}
	}
	return phases, nil
}

// ---------------------------------------------------------------------------- run

type harnessLabel struct {
	Suite     string
	Label     censor.Label
	Synthetic bool // a control made for a detector, not a v2 carrier
}

func runV2(nodes map[string]*node, v *verdict, out string) {
	phases, err := v2PhaseList(*scenario)
	if err != nil {
		fatalf("%v", err)
	}
	seed := *seed
	if seed == 0 {
		seed = time.Now().UnixNano()
		logf("seed %d (-seed %d repeats the run)", seed, seed)
	}
	findings := []string{}
	behaviour := map[string]any{"seed": seed, "profile": *v2Profile, "network": *v2Network}

	profilePath := filepath.Join("testbed", "browserprofiles", "profiles.json")
	// The capture is made on this machine, so it is always a lab capture; a run for another
	// network uses the lab family and says so, instead of pretending the bytes came from there.
	db, err := browserprofiles.LoadOrCapture(context.Background(), profilePath, "lab", "lab")
	if err != nil {
		v.check(false, "browser profile database: %v", err)
		db = &browserprofiles.Database{Version: browserprofiles.DBVersion}
	}
	stamp := browserProfileStamp{Version: db.Version, Entries: len(db.Entries)}
	stamp.Sources = profileSources(db)
	stamp.Note = browserProfileNote(db, *v2Network)
	cfg := detectorConfig(db)

	var labeled []censor.LabeledFlow
	var capture *v2Capture
	var carrier carrierInfo
	var probes []probe.Report
	carrier.Name = "none"

	for _, phase := range phases {
		switch phase {
		case "net":
			v2NetPhase(nodes, v, behaviour)
		case "services":
			v2ServicesPhase(nodes, v, behaviour)
		case "censor":
			labeled, capture, carrier = v2CensorPhase(nodes, v, out, seed)
		case "probe":
			if *v2LiveLab {
				probes = liveProbePhase(v, out, seed)
			} else {
				probes = v2ProbePhase(v, out, *v2ProbeSpec)
			}
		}
	}

	metrics := map[string]censor.DetectorMetric{}
	var v2gate *censor.V2GateResult
	var h1a *censor.H1aResult
	if len(labeled) > 0 {
		metrics = censor.EvaluateDetectors(labeled, cfg)
		g := censor.V2Gate(labeled, cfg)
		v2gate = &g
		if exit, browser := h1aFlows(labeled); len(exit) > 0 {
			r := censor.H1aMeasure(exit, browser)
			h1a = &r
			if !r.Pass {
				findings = append(findings, fmt.Sprintf("H1a: the external records of the exit flows are KS %.3f from the browser control (threshold %.3f)", r.KS, r.Threshold))
			}
		} else {
			findings = append(findings, "H1a not measured: no v2 TCP exit flow with TLS records in this run (the v2 carriers of the lab are UDP; reality2-tcp and ws2 join with T0.3)")
		}
		for _, row := range g.Rows {
			if !row.Measured && (row.Detector == "D5" || row.Detector == "D6" || row.Detector == "D7") {
				findings = append(findings, fmt.Sprintf("%s not measured on v2: no v2 flow of the kind it reads (TCP with records) was in this run", row.Detector))
			}
		}
	}
	if *v2LiveLab && len(labeled) > 0 {
		behaviour["by_group"] = byGroup(labeled, cfg)
	}
	agg := aggregateFor(labeled)
	flows := countFlows(labeled)

	gate := censor.V2GateResult{}
	if v2gate != nil {
		gate = *v2gate
	}
	for _, line := range probeFindings(probes) {
		findings = append(findings, line)
		v.check(false, "%s", line)
	}
	for _, line := range gateFindings(metrics, gate) {
		findings = append(findings, line)
		v.check(false, "%s", line)
	}
	// The pcap is evidence only for a red run. A green analysis deletes it, even with -pcap.
	green := len(v.failed) == 0
	pcap := pcapInfo{Requested: *v2Pcap, BudgetBytes: v2PcapBudget.v}
	if capture != nil {
		pcap = capture.finalizeInfo(*v2Pcap, green)
		findings = append(findings, pcapFinding(pcap)...)
		behaviour["pcap_kept"] = pcap.Kept
	}

	resources := map[string]any{}
	for _, n := range []string{"b1", "b2", "x1"} {
		if _, ok := nodes[n]; ok {
			if m := metricsOf(n); len(m) > 0 {
				resources[n] = m
			}
		}
	}

	if carrier.Synthetic {
		findings = append(findings, "the v2 carrier is synthetic: the real noise3 mode did not run; udp-noise is a stand-in with the same wire shape, and the v1 product flows are the real traffic in this run")
	}
	if len(db.Entries) > 0 {
		findings = append(findings, fmt.Sprintf("browser profiles %s: %s", db.Version, strings.Join(stamp.Sources, ", ")))
	} else {
		findings = append(findings, "no browser profile could be captured on this machine")
	}

	report := buildReport(reportInput{
		Scenario: *scenario, Seed: seed, Profile: *v2Profile, Network: *v2Network,
		Phases: phases, Detectors: metrics, Aggregate: agg,
		Coverage: coverageFor(metrics, carrier), Behaviour: behaviour, Resources: resources,
		Carrier: carrier, BrowserProfiles: stamp, Pcap: pcap, Flows: flows, Findings: findings,
		V2Gate: v2gate, H1a: h1a, Probes: probes,
	})
	if err := writeJSON(filepath.Join(out, "report.json"), report); err != nil {
		v.check(false, "write report.json: %v", err)
	}
	if f, err := os.Create(filepath.Join(out, "flows.csv")); err == nil {
		if err := writeFlowsCSV(f, labeled, 200); err != nil {
			v.check(false, "write flows.csv: %v", err)
		}
		_ = f.Close()
	} else {
		v.check(false, "write flows.csv: %v", err)
	}
	var summary strings.Builder
	writeSummary(&summary, report, sortedByBytes(labeled))
	if err := os.WriteFile(filepath.Join(out, "summary.txt"), []byte(summary.String()), 0o644); err != nil {
		v.check(false, "write summary.txt: %v", err)
	}
	resultsDir := filepath.Join("testbed", "results")
	if path, err := saveResult(report, resultsDir); err != nil {
		v.check(false, "save results copy: %v", err)
	} else {
		logf("results copy: %s", path)
	}

	logf("report: %s", filepath.Join(out, "report.json"))
	logf("flows: %d total, %d labelled; visibility %.1f%%", flows.Total, flows.Labelled, agg.VisibilityPct)
	for id, m := range metrics {
		logf("  %-3s recall %s fpr %.3f n=%d", id, recallText(m.Recall), m.FPR, m.N)
	}
}

// ---------------------------------------------------------------------------- phases

func v2NetPhase(nodes map[string]*node, v *verdict, behaviour map[string]any) {
	meshUp(nodes, 3*time.Minute)
	s, err := statusOf("b1")
	if err != nil {
		v.check(false, "b1 status: %v", err)
		return
	}
	behaviour["b1_peers"] = s.Peers
	if s.Peers < 1 {
		v.check(false, "the mesh did not form (b1 has %d peers)", s.Peers)
	}
}

func v2ServicesPhase(nodes map[string]*node, v *verdict, behaviour map[string]any) {
	l := &ledger{}
	for i := 0; i < 3; i++ {
		l.sendDM(nodes, "a1", "a2", fmt.Sprintf("v2 baseline %d", i))
		l.sendDM(nodes, "a2", "a1", fmt.Sprintf("v2 baseline back %d", i))
	}
	missing, dups := waitDelivered(nodes, l, time.Minute)
	total := len(l.dms)
	delivered := total - len(missing)
	pct := 100.0
	if total > 0 {
		pct = 100 * float64(delivered) / float64(total)
	}
	behaviour["service_delivery_pct"] = pct
	behaviour["service_duplicates"] = dups
	v.check(len(missing) == 0 && dups == 0, "services: %d of %d messages delivered exactly once", delivered, total)
}

// v2CensorPhase captures what leaves the b network, drives the harness and the controls, and feeds
// the same testbed/censor detectors the v1 censor scenario uses.
func v2CensorPhase(nodes map[string]*node, v *verdict, out string, seed int64) ([]censor.LabeledFlow, *v2Capture, carrierInfo) {
	gw := "gw_b" // b1 (harness + browser) and b2 (the v1 product) are behind it
	cap, err := startV2Capture(gw, out, v2PcapBudget.v, *v2Pcap)
	if err != nil {
		v.check(false, "start the capture on %s: %v", gw, err)
		return nil, nil, carrierInfo{Name: "none"}
	}

	carrier, labels, err := runCarrierHarness(nodes, v, seed)
	if err != nil {
		v.check(false, "v2 carrier harness: %v", err)
	}
	runBrowserControl(v)
	if !*v2LiveLab { // the live lab runs the v1 product beside the carriers, for as long as they run
		runProductTraffic(nodes, v)
	}
	time.Sleep(2 * time.Second)
	cap.stop()

	if cap.bytes() == 0 {
		v.check(false, "no packets were captured on %s", gw)
		return nil, cap, carrier
	}
	if cap.overBudget() {
		v.check(false, "the capture reached its budget of %s; analysis uses the first %s", humanBytes(cap.budget), humanBytes(cap.bytes()))
	}
	flows, err := cap.sink.Finish()
	if err != nil {
		v.check(false, "analyze the capture of %s: %v", gw, err)
		return nil, cap, carrier
	}
	if n := cap.sink.dropped; n > 0 {
		v.check(false, "the capture has more than %d flows: %d flows past that were not analysed", censor.MaxFlows, n)
	}
	hosted := labHosted()
	censor.CheckSNI(flows, hosted)
	labeled := make([]censor.LabeledFlow, 0, len(flows))
	for _, f := range flows {
		labeled = append(labeled, labelFlow(f, labels))
	}
	if len(labeled) == 0 {
		v.check(false, "the capture of %s has no flows", gw)
	}
	logf("captured %d flows on %s (%s captured)", len(labeled), gw, humanBytes(cap.bytes()))
	return labeled, cap, carrier
}

// ---------------------------------------------------------------------------- harness

func runCarrierHarness(nodes map[string]*node, v *verdict, seed int64) (carrierInfo, map[uint16]harnessLabel, error) {
	info := carrierInfo{Name: "v2carrier (real noise3 + controls)", Synthetic: false,
		Note: "noise3 is the real carrier of internal/v2; udp-noise stays as the synthetic D10 control, and tls-in-tls, const-size and conn-storm are the controls of D5-D8; efv2 (T0.3) does not exist yet, so reality2 and ws2 are not in the lab"}
	labels := map[uint16]harnessLabel{}
	bin, err := buildCarrierBinary()
	if err != nil {
		return info, labels, err
	}
	for _, n := range []string{"x1", "b1"} {
		if out, err := run("docker", "cp", bin, container(n)+":/tmp/v2carrier"); err != nil {
			return info, labels, fmt.Errorf("copy the carrier to %s: %v %s", n, err, out)
		}
		_, _ = run("docker", "exec", container(n), "chmod", "+x", "/tmp/v2carrier")
	}

	// One port per mode: a flow's label then comes from the server port, not from a wall-clock
	// window (the capture timestamps are taken in the container, and a window would be a guess).
	type carrierSpec struct {
		mode, sni, label, suite string
		conns                   int
		port                    uint16
		synthetic               bool   // a control made for a detector, not a v2 carrier
		serve                   string // the server mode; "serve" unless the carrier has its own listener
		repeat                  int    // how many times the client runs (one flow each)
	}
	specs := []carrierSpec{
		{mode: "noise3", sni: "cover.lab", label: string(censor.LabelCarrier), suite: "noise3", conns: 1, port: 4347, serve: "noise3-serve"},
		{mode: "udp-noise", sni: "cover.lab", label: string(censor.LabelCarrier), suite: "udp-noise", conns: 1, port: 4341, synthetic: true},
		{mode: "tcp-random", sni: "cover.lab", label: string(censor.LabelCarrier), suite: "tcp-random", conns: 6, port: 4342, synthetic: true},
		{mode: "tls-go", sni: "foreign.example", label: string(censor.LabelTool), suite: "tls-go", conns: 1, port: 4343},
		{mode: "visit", sni: "cover.lab", label: string(censor.LabelBrowser), suite: "utls-chrome", conns: 25, port: 4344},
		{mode: "quic-go", sni: "blocked.example", label: string(censor.LabelTool), suite: "quic-go", conns: 1, port: 4345},
		{mode: "quic-chrome", sni: "cover.lab", label: string(censor.LabelBrowser), suite: "quic-go-chromeparrot", conns: 1, port: 4346},
		{mode: "tls-in-tls", sni: "cover.lab", label: string(censor.LabelCarrier), suite: censor.SuiteTLSInTLS, conns: 1, port: 4348, synthetic: true, repeat: 20},
		{mode: "const-size", sni: "cover.lab", label: string(censor.LabelCarrier), suite: censor.SuiteConstSize, conns: 1, port: 4349, synthetic: true, repeat: 6},
		{mode: "conn-storm", sni: "cover.lab", label: string(censor.LabelCarrier), suite: censor.SuiteConnStorm, conns: 30, port: 4350, synthetic: true},
	}
	if *v2LiveLab {
		// the live lab drives noise3 through the same page traffic as the other carriers
		kept := specs[:0]
		for _, s := range specs {
			if s.mode != "noise3" {
				kept = append(kept, s)
			}
		}
		specs = kept
	}
	noise3OK := false
	for _, s := range specs {
		labels[s.port] = harnessLabel{Suite: s.suite, Label: censor.Label(s.label), Synthetic: s.synthetic}
		serveMode := s.serve
		if serveMode == "" {
			serveMode = "serve"
		}
		ready := fmt.Sprintf("/tmp/v2carrier-%d.ready", s.port)
		_, _ = run("docker", "exec", container("x1"), "rm", "-f", ready)
		if out, err := run("docker", "exec", "-d", container("x1"), "/tmp/v2carrier",
			"-mode", serveMode, "-listen", fmt.Sprintf(":%d", s.port), "-seed", strconv.FormatInt(seed, 10), "-ready", ready); err != nil {
			return info, labels, fmt.Errorf("start the carrier server on %d: %v %s", s.port, err, out)
		}
	}
	defer func() {
		_, _ = run("docker", "exec", container("x1"), "pkill", "-f", "/tmp/v2carrier")
	}()
	for _, s := range specs {
		ready := fmt.Sprintf("/tmp/v2carrier-%d.ready", s.port)
		ok := waitFor(fmt.Sprintf("the carrier server on %d", s.port), 20*time.Second, func() bool {
			_, err := run("docker", "exec", container("x1"), "sh", "-c", "test -f "+ready)
			return err == nil
		})
		if !ok {
			return info, labels, fmt.Errorf("the carrier server on %d did not become ready", s.port)
		}
	}

	for _, s := range specs {
		peer := fmt.Sprintf("203.0.113.15:%d", s.port) // x1
		args := []string{"/tmp/v2carrier", "-mode", s.mode, "-peer", peer, "-sni", s.sni,
			"-duration", "4s", "-conns", strconv.Itoa(s.conns), "-seed", strconv.FormatInt(seed, 10)}
		full := append([]string{"exec", container("b1")}, args...)
		runs := max(1, s.repeat)
		for k := 0; k < runs; k++ {
			if s.repeat > 0 {
				// every repetition is a flow of its own with its own seed
				full = append([]string{"exec", container("b1")}, withSeed(args, seed+int64(k))...)
			}
			out, err := run("docker", full...)
			if err != nil {
				v.check(false, "carrier mode %s: %v %s", s.mode, err, out)
				break
			}
			var res struct {
				Bytes   int64 `json:"bytes"`
				Packets int64 `json:"packets"`
			}
			_ = json.Unmarshal([]byte(out), &res)
			if k == 0 {
				info.Modes = append(info.Modes, s.mode)
			}
			info.Bytes += res.Bytes
			info.Packets += res.Packets
			if s.mode == "noise3" {
				noise3OK = true
			}
			if s.mode == "udp-noise" || s.mode == "tcp-random" || s.mode == "noise3" {
				logf("carrier mode %s: %d bytes in %d packets", s.mode, res.Bytes, res.Packets)
			}
		}
	}
	if *v2LiveLab {
		if err := liveHarness(nodes, v, seed, &info, labels); err != nil {
			return info, labels, err
		}
		noise3OK = true
		info.Name = "v2carrier (live lab: noise3, reality2-tcp, reality2-quic, ws2 + controls)"
		info.Note = "the four real v2 carriers of internal/v2 as servers on x1 and clients on b1 across gw_b; the v1 product beside them; the synthetic modes are controls of the detectors"
	}
	// without the real carrier the run says so, and D10 is then judged on the stand-in alone
	info.Synthetic = !noise3OK
	return info, labels, nil
}

// withSeed returns args with the value after -seed replaced.
func withSeed(args []string, seed int64) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-seed" {
			out[i+1] = strconv.FormatInt(seed, 10)
		}
	}
	return out
}

func buildCarrierBinary() (string, error) {
	return buildLinuxBinary("echofog-v2carrier-linux", "./testbed/v2carrier")
}

// buildLinuxBinary builds a package of the module for the lab containers.
func buildLinuxBinary(name, pkg string) (string, error) {
	out := filepath.Join(os.TempDir(), name)
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOWORK=off")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s: %v\n%s", pkg, err, raw)
	}
	return out, nil
}

// v2ProbePhase runs the active probes D11-D15 from the border: the prober is copied into gw_b and
// asks the nodes of the spec from there, never from a node. The spec comes from the person who
// started the run (the lab does not run a v2 node of its own until efv2 exists, T0.3).
func v2ProbePhase(v *verdict, out, specPath string) []probe.Report {
	const gw = "gw_b"
	bin, err := buildLinuxBinary("echofog-launchpad-probe-linux", "./testbed/probe/cmd/launchpad-probe")
	if err != nil {
		v.check(false, "probe phase: %v", err)
		return nil
	}
	c := container(gw)
	if o, err := run("docker", "cp", bin, c+":/tmp/launchpad-probe"); err != nil {
		v.check(false, "probe phase: copy the prober to %s: %v %s", gw, err, o)
		return nil
	}
	if o, err := run("docker", "cp", specPath, c+":/tmp/probe.json"); err != nil {
		v.check(false, "probe phase: copy the spec to %s: %v %s", gw, err, o)
		return nil
	}
	_, _ = run("docker", "exec", c, "chmod", "+x", "/tmp/launchpad-probe")
	// exit 1 means "an answer of the node's own was found": the reports are still on stdout
	raw, err := run("docker", "exec", c, "/tmp/launchpad-probe", "-spec", "/tmp/probe.json")
	var reps []probe.Report
	if jerr := json.Unmarshal([]byte(raw), &reps); jerr != nil {
		v.check(false, "probe phase: no reports from %s: %v %v", gw, err, jerr)
		return nil
	}
	_ = writeJSON(filepath.Join(out, "probes.json"), reps)
	return reps
}

func runBrowserControl(v *verdict) {
	for i := 0; i < 6; i++ {
		_, err := run("docker", "exec", container("b1"), "curl", "-sk", "-o", "/dev/null", "--max-time", "10",
			"--resolve", "cover.lab:443:"+webIP,
			fmt.Sprintf("https://cover.lab/blob?size=%d&seed=control-%d", 1<<20, i))
		if err != nil {
			v.check(false, "browser control visit %d: %v", i, err)
			return
		}
	}
}

func runProductTraffic(nodes map[string]*node, v *verdict) {
	if err := startVPN("b2", joinAddr(nodes, "x1")); err != nil {
		logf("v1 product traffic skipped: %v", err)
		return
	}
	_, ok, _ := untilWorks("b2", 45*time.Second)
	if !ok {
		logf("v1 product traffic: the VPN did not come up")
		_ = call("b2", "POST", "/vpn/stop", nil, nil)
		return
	}
	for i := 0; i < 2; i++ {
		if r := fetch("b2", blobURL(1<<20, fmt.Sprintf("v1-product-%d", i)), 30*time.Second); !r.OK {
			logf("v1 product traffic: download %d failed: %s", i, r.Error)
		}
	}
	_ = call("b2", "POST", "/vpn/stop", nil, nil)
}

// ---------------------------------------------------------------------------- labeling and config

func labelFlow(f censor.Flow, labels map[uint16]harnessLabel) censor.LabeledFlow {
	lf := censor.LabeledFlow{Flow: f, Label: censor.LabelLab}
	if lab, ok := labels[f.Server.Port()]; ok {
		lf.Label, lf.Suite, lf.Synthetic = lab.Label, lab.Suite, lab.Synthetic
		return lf
	}
	switch {
	case f.Server.Port() == 443 && f.Server.Addr().String() == webIP:
		// curl is an honest small tool, not a browser: the browser control is the uTLS Chrome mode.
		lf.Label, lf.Suite = censor.LabelTool, "curl-control"
	case f.Server.Port() == 4242 && isExitNode(f.Server.Addr()):
		lf.Label, lf.Suite = censor.LabelProduct, "v1-product"
	}
	return lf
}

// isExitNode is x1/x2: the public exits the v1 product dials. The other lab nodes' own traffic
// (bootstrap, relay, DHT) is not product traffic.
func isExitNode(a netip.Addr) bool {
	s := a.String()
	return s == "203.0.113.15" || s == "203.0.113.16"
}

func labHosted() map[netip.Addr][]string {
	hosted := map[netip.Addr][]string{}
	for _, ip := range []string{"203.0.113.11", "203.0.113.12", "203.0.113.13", "203.0.113.14",
		"203.0.113.15", "203.0.113.16", "203.0.113.17", "203.0.113.30"} {
		hosted[netip.MustParseAddr(ip)] = []string{"cover.lab"}
	}
	return hosted
}

func detectorConfig(db *browserprofiles.Database) censor.DetectorConfig {
	hosted := labHosted()
	serverASN := map[netip.Addr]string{}
	for ip := range hosted {
		serverASN[ip] = "AS64500"
	}
	return censor.DetectorConfig{
		BlockedSNI: map[string]bool{"blocked.example": true},
		Hosted:     hosted,
		ServerASN:  serverASN,
		DNS: map[string]censor.ResolvedName{
			"cover.lab":       {IPs: []netip.Addr{netip.MustParseAddr("203.0.113.15"), netip.MustParseAddr(webIP)}, ASN: "AS64500"},
			"blocked.example": {IPs: []netip.Addr{netip.MustParseAddr("198.51.100.20")}, ASN: "AS64501"},
			"foreign.example": {IPs: []netip.Addr{netip.MustParseAddr("198.51.100.7")}, ASN: "AS64501"},
		},
		BrowserJA4: db.BrowserJA4(),
		BrowserJA3: db.BrowserJA3(),
	}
}

func profileSources(db *browserprofiles.Database) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range db.Entries {
		if !seen[p.Source] {
			seen[p.Source] = true
			out = append(out, p.Source)
		}
	}
	sort.Strings(out)
	return out
}

// browserProfileNote says where the capture really came from. -network only labels a run; it does
// not turn a lab capture into a capture on a mobile operator's network.
func browserProfileNote(db *browserprofiles.Database, runNetwork string) string {
	if len(db.Entries) == 0 {
		return fmt.Sprintf("no browser profile entries; run network label: %s; real captures belong to T15.2", runNetwork)
	}
	seen := map[string]bool{}
	var nets []string
	for _, p := range db.Entries {
		k := p.Country + "/" + p.Network
		if !seen[k] {
			seen[k] = true
			nets = append(nets, k)
		}
	}
	sort.Strings(nets)
	return fmt.Sprintf("profile entries: country/network %s; run network label: %s; real %s captures belong to T15.2",
		strings.Join(nets, ", "), runNetwork, runNetwork)
}

// ---------------------------------------------------------------------------- aggregates

func aggregateFor(flows []censor.LabeledFlow) aggregate {
	var a aggregate
	atRisk, total, strict := 0, 0, 0
	for _, f := range flows {
		if f.Label != censor.LabelCarrier && f.Label != censor.LabelProduct {
			continue
		}
		b := f.BytesUp + f.BytesDn
		total += b
		if strings.HasPrefix(f.Verdict(), "AT RISK") {
			atRisk += b
			a.AtRiskFlows++
		}
		if f.Verdict() != "blends in" && f.Looks != "no data" {
			strict += b
		}
		if f.Label == censor.LabelCarrier {
			a.CarrierFlows++
			a.CarrierBytes += b
		}
	}
	if total > 0 {
		a.VisibilityPct = 100 * float64(atRisk) / float64(total)
		a.StrictVisibilityPct = 100 * float64(strict) / float64(total)
	}
	return a
}

func countFlows(flows []censor.LabeledFlow) flowCounts {
	out := flowCounts{Total: len(flows)}
	for _, f := range flows {
		switch f.Label {
		case censor.LabelCarrier:
			out.Carrier++
		case censor.LabelProduct:
			out.Product++
		case censor.LabelBrowser:
			out.Browser++
		case censor.LabelTool:
			out.Tool++
		}
	}
	out.Labelled = out.Carrier + out.Product + out.Browser + out.Tool
	return out
}

func coverageFor(metrics map[string]censor.DetectorMetric, carrier carrierInfo) []coverageRow {
	rows := []coverageRow{
		{"P13 random first payload / GFW FET", "D1", "", ""},
		{"P7 QUIC SNI", "D2", "", ""},
		{"SNI against IP/ASN", "D3", "", ""},
		{"P14 ClientHello fingerprint (JA3/JA4)", "D4", "", ""},
		{"H1a TLS inside TLS", "D5", "", ""},
		{"H1a cross-layer RTT", "D6", "", ""},
		{"H1a record sizes and timings", "D7", "", ""},
		{"long and heavy flows, connection storms", "D8", "", ""},
		{"ML flow physics", "D9", "", ""},
		{"P7/P14 UDP shape", "D10", "", ""},
	}
	for i := range rows {
		id := rows[i].Detector
		m, ok := metrics[id]
		if !ok {
			rows[i].Verdict = "no data"
			continue
		}
		rows[i].Note = fmt.Sprintf("recall %s, browser FPR %.3f, n=%d", recallText(m.Recall), m.FPR, m.N)
		switch {
		case id == "D9":
			if m.AUC == nil || m.TPRAt1 == nil {
				rows[i].Verdict = "outside the gate: no training set in this run"
				break
			}
			rows[i].Verdict = "outside the gate: reported only"
			rows[i].Note = fmt.Sprintf("AUC %.2f, TPR@1%%FPR %.2f, n=%d", *m.AUC, *m.TPRAt1, m.N)
		case id == "D6":
			if m.AUC == nil {
				rows[i].Verdict = "no positive control: AUC unavailable"
				break
			}
			rows[i].Verdict = "control works on a naive proxy; the v2 side is in v2_gate"
			rows[i].Note = fmt.Sprintf("AUC %.2f, TPR@1%%FPR %s, n=%d", *m.AUC, recallText(m.TPRAt1), m.N)
		case m.Positives == 0 || m.Recall == nil:
			rows[i].Verdict = "no positive control: recall unavailable"
		case m.FPR > 0.01:
			rows[i].Verdict = "finding: browser FPR above 1%"
		case id == "D5" && *m.Recall < 0.9:
			rows[i].Verdict = "finding: missed nested TLS"
		case (id == "D7" || id == "D8") && *m.Recall < 0.9:
			rows[i].Verdict = "reported: recall below 90% (not gated)"
		case id != "D5" && id != "D7" && id != "D8" && *m.Recall < 1:
			rows[i].Verdict = "finding: missed its control flows"
		case id == "D10" && carrier.Synthetic:
			rows[i].Verdict = "caught the synthetic stand-in (expected)"
		default:
			rows[i].Verdict = "control works"
		}
	}
	return rows
}

func sortedByBytes(flows []censor.LabeledFlow) []censor.LabeledFlow {
	out := append([]censor.LabeledFlow(nil), flows...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].BytesUp+out[i].BytesDn > out[j].BytesUp+out[j].BytesDn
	})
	return out
}

func pcapFinding(p pcapInfo) []string {
	switch {
	case p.CapturedBytes > p.BudgetBytes:
		return []string{fmt.Sprintf("pcap %s exceeded the %s budget", humanBytes(p.CapturedBytes), humanBytes(p.BudgetBytes))}
	case p.Kept:
		return []string{fmt.Sprintf("pcap kept for a red run: %s", strings.Join(p.Files, ", "))}
	case p.Deleted && p.Requested:
		return []string{"pcap deleted after a green analysis"}
	}
	return nil
}

// ---------------------------------------------------------------------------- capture

var errCaptureLimit = errors.New("pcap budget reached")

// captureSink enforces the pcap budget while the bytes arrive, not by polling a file afterwards.
// It keeps the capture in memory for the detectors and, only with -pcap, tees it to a file. The
// file can never grow past the limit because Write truncates the last chunk.
type captureSink struct {
	mu    sync.Mutex
	limit int64
	n     int64
	over  bool
	buf   bytes.Buffer
	file  *os.File

	// A streaming sink hands the bytes to the censor's stream analyzer instead of keeping them.
	an   io.WriteCloser
	wait func() (censor.Analysis, error)

	dropped int // new flows past censor.MaxFlows
}

func newCaptureSink(limit int64, file *os.File) *captureSink {
	return &captureSink{limit: limit, file: file}
}

func (s *captureSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if s.limit > 0 {
		remaining := s.limit - s.n
		if remaining <= 0 {
			s.over = true
			return 0, errCaptureLimit
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
			s.over = true
		}
	}
	var n int
	var err error
	if s.an != nil {
		n, err = s.an.Write(p)
	} else {
		n, err = s.buf.Write(p)
	}
	if err != nil {
		return n, err
	}
	if s.file != nil {
		if _, err := s.file.Write(p); err != nil {
			return n, err
		}
	}
	s.n += int64(n)
	if s.over {
		return n, errCaptureLimit
	}
	return n, nil
}

func (s *captureSink) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.an != nil {
		return nil
	}
	return append([]byte(nil), s.buf.Bytes()...)
}

func (s *captureSink) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *captureSink) Over() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.over
}

type v2Capture struct {
	gw        string
	out       string
	container string
	tempPath  string // only when -pcap asked for a file
	budget    int64
	sink      *captureSink
	file      *os.File
	cmd       *exec.Cmd
	stderr    bytes.Buffer
	pumpDone  chan struct{}
	once      sync.Once
	mu        sync.Mutex
	violated  bool
}

func startV2Capture(gw, out string, budget int64, keep bool) (*v2Capture, error) {
	c := &v2Capture{gw: gw, out: out, container: container(gw), budget: budget, pumpDone: make(chan struct{})}
	if keep {
		// The only pcap on disk is the one -pcap asked for. It is deleted after a green analysis
		// and renamed to wan-<gw>.pcap after a red one.
		c.tempPath = filepath.Join(out, ".wan-"+gw+".pcap")
		f, err := os.Create(c.tempPath)
		if err != nil {
			return nil, err
		}
		c.file = f
	}
	c.sink = newStreamingSink(budget, c.file)
	for i := 0; i < 30; i++ {
		if o, err := run("docker", "exec", c.container, "cat", "/tmp/wan_if"); err == nil && o != "" {
			break
		}
		time.Sleep(time.Second)
	}
	cmd := exec.Command("docker", "exec", c.container, "tcpdump", "-i", wanIf(gw), "-s", "0", "-U", "-w", "-", "ip")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.cleanup()
		return nil, err
	}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		c.cleanup()
		return nil, err
	}
	c.cmd = cmd
	go c.pump(stdout)
	time.Sleep(time.Second)
	return c, nil
}

// pump copies the pcap stream into the sink; the sink stops it at the byte where the budget ends.
func (c *v2Capture) pump(r io.Reader) {
	defer close(c.pumpDone)
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := c.sink.Write(buf[:n]); werr != nil {
				if errors.Is(werr, errCaptureLimit) {
					c.mu.Lock()
					c.violated = true
					c.mu.Unlock()
					logf("pcap budget %s reached; stopping the capture at %s", humanBytes(c.budget), humanBytes(c.sink.Size()))
				} else {
					logf("write the capture: %v", werr)
				}
				_, _ = run("docker", "exec", c.container, "pkill", "-INT", "tcpdump")
				// The docker exec client may still be blocked on the pipe; kill it so Wait cannot
				// hang after the reader has stopped.
				if c.cmd != nil && c.cmd.Process != nil {
					_ = c.cmd.Process.Kill()
				}
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *v2Capture) cleanup() {
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
	if c.tempPath != "" {
		_ = os.Remove(c.tempPath)
		c.tempPath = ""
	}
}

func (c *v2Capture) stop() {
	c.once.Do(func() {
		_, _ = run("docker", "exec", c.container, "pkill", "-INT", "tcpdump")
		select {
		case <-c.pumpDone:
		case <-time.After(10 * time.Second):
		}
		// Kill the docker exec client even when the reader has already stopped: otherwise Wait
		// can stay blocked on the pipe after a hard budget stop.
		if c.cmd != nil {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			done := make(chan struct{})
			go func() {
				_ = c.cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
		if c.file != nil {
			_ = c.file.Close()
			c.file = nil
		}
		c.mu.Lock()
		c.violated = c.violated || c.sink.Over()
		c.mu.Unlock()
	})
}

func (c *v2Capture) bytes() int64 { return c.sink.Size() }

func (c *v2Capture) overBudget() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.violated
}

// finalizeInfo renames or deletes the capture once, after the analysis decided green or red.
func (c *v2Capture) finalizeInfo(requested, green bool) pcapInfo {
	size := c.sink.Size()
	violated := c.overBudget()
	info := pcapInfo{Requested: requested, BudgetBytes: c.budget, CapturedBytes: size}
	if violated {
		info.Reason = "budget reached; the capture was stopped at the limit"
	}
	if !requested {
		info.Reason = "no -pcap; the capture stayed in memory and was discarded after analysis"
		return info
	}
	if c.tempPath == "" {
		return info
	}
	if green {
		_ = os.Remove(c.tempPath)
		info.Deleted = true
		info.Reason = "deleted after a green analysis"
		return info
	}
	final := filepath.Join(c.out, "wan-"+c.gw+".pcap")
	if err := os.Rename(c.tempPath, final); err == nil {
		info.Kept = true
		info.Files = []string{filepath.Base(final)}
		c.tempPath = ""
	} else {
		info.Reason = fmt.Sprintf("keep the capture: %v", err)
	}
	return info
}

// newStreamingSink is a captureSink that feeds the censor's stream analyzer instead of keeping the
// capture in memory (T15.0b item 5): memory follows the number of flows, not the budget. The bytes
// still obey the budget and still reach the -pcap file when there is one.
func newStreamingSink(limit int64, file *os.File) *captureSink {
	s := newCaptureSink(limit, file)
	s.an, s.wait = censor.AnalyzeStreamStats()
	return s
}

// buffered is how many bytes of the capture the sink holds in memory.
func (s *captureSink) buffered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// Finish ends a streaming sink and returns the flows it analysed. A capture cut at the budget, or
// in the middle of a record, is read up to there; an empty capture is an error.
func (s *captureSink) Finish() ([]censor.Flow, error) {
	if s.an == nil {
		return nil, errors.New("not a streaming sink")
	}
	_ = s.an.Close()
	a, err := s.wait()
	s.dropped = a.DroppedFlows
	return a.Flows, err
}
