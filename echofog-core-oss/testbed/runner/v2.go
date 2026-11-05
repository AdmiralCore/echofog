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
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	v2Phases     = flag.String("phases", "", "v2: comma-separated phases (censor,net,services); default depends on the scenario")
	v2Profile    = flag.String("profile", "desktop", "v2: desktop or android")
	v2Network    = flag.String("network", "lab", "v2: lab, ru-mobile, cn or ir")
	v2Pcap       = flag.Bool("pcap", false, "v2: keep the packet capture until the analysis is green (it is deleted after)")
	v2PcapBudget = &byteSizeFlag{v: maxPcapBudget}
)

func init() {
	flag.Var(v2PcapBudget, "pcap-budget", "v2: total packet-capture budget per run (default and maximum 2GB)")
}

func isV2Scenario(s string) bool { return strings.HasPrefix(s, "v2-") }

func validateV2Flags(scenario string) error {
	if !isV2Scenario(scenario) {
		return nil
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
	return nil
}

func v2PhaseList(scenario string) ([]string, error) {
	var phases []string
	if *v2Phases != "" {
		phases = strings.Split(*v2Phases, ",")
	} else {
		switch scenario {
		case "v2-basic":
			phases = []string{"net", "services"}
		case "v2-censor", "v2-carrier":
			phases = []string{"censor"}
		default:
			return nil, fmt.Errorf("unknown v2 scenario %q (v2-basic, v2-censor, v2-carrier)", scenario)
		}
	}
	for i := range phases {
		phases[i] = strings.TrimSpace(phases[i])
		switch phases[i] {
		case "censor", "net", "services":
		default:
			return nil, fmt.Errorf("unknown phase %q (censor, net, services)", phases[i])
		}
	}
	return phases, nil
}

// ---------------------------------------------------------------------------- run

type harnessLabel struct {
	Suite string
	Label censor.Label
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
	stamp.Note = fmt.Sprintf("captured for country/network lab/%s; real Chrome is used when installed, otherwise the uTLS Chrome preset, curl and quic-go are recorded honestly", *v2Network)
	cfg := detectorConfig(db)

	var labeled []censor.LabeledFlow
	var capture *v2Capture
	var carrier carrierInfo
	carrier.Name = "none"

	for _, phase := range phases {
		switch phase {
		case "net":
			v2NetPhase(nodes, v, behaviour)
		case "services":
			v2ServicesPhase(nodes, v, behaviour)
		case "censor":
			labeled, capture, carrier = v2CensorPhase(nodes, v, out, seed)
		}
	}

	metrics := map[string]censor.DetectorMetric{}
	if len(labeled) > 0 {
		metrics = censor.EvaluateDetectors(labeled, cfg)
	}
	agg := aggregateFor(labeled)
	flows := countFlows(labeled)

	for id, m := range metrics {
		if m.FPR > 0.01 {
			line := fmt.Sprintf("%s false-positive rate %.3f on the browser control (gate 0.01)", id, m.FPR)
			findings = append(findings, line)
			v.check(false, "%s", line)
		}
		if m.Positives > 0 && m.Recall < 1 {
			line := fmt.Sprintf("%s recall %.2f on its control flows (want 1)", id, m.Recall)
			findings = append(findings, line)
			v.check(false, "%s", line)
		}
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
		findings = append(findings, "the v2 carrier is synthetic: T0.3 (efv2) and T2.4 (noise3) are not built yet; udp-noise is a stand-in with the same wire shape, and the v1 product flows are the real traffic in this run")
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
		logf("  %-3s recall %.2f fpr %.3f n=%d", id, m.Recall, m.FPR, m.N)
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
	cap, err := startV2Capture(gw, out, v2PcapBudget.v)
	if err != nil {
		v.check(false, "start the capture on %s: %v", gw, err)
		return nil, nil, carrierInfo{Name: "none"}
	}

	carrier, labels, err := runCarrierHarness(nodes, v, seed)
	if err != nil {
		v.check(false, "v2 carrier harness: %v", err)
	}
	runBrowserControl(v)
	runProductTraffic(nodes, v)
	time.Sleep(2 * time.Second)
	cap.stop()

	path := cap.path()
	if path == "" {
		v.check(false, "no capture was copied from %s", gw)
		return nil, cap, carrier
	}
	if cap.overBudget() {
		v.check(false, "the capture went over its budget of %s", humanBytes(cap.budget))
	}
	flows, err := censor.Analyze(path)
	if err != nil {
		v.check(false, "analyze the capture of %s: %v", gw, err)
		return nil, cap, carrier
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
	logf("captured %d flows on %s (%s on disk)", len(labeled), gw, humanBytes(cap.bytes()))
	return labeled, cap, carrier
}

// ---------------------------------------------------------------------------- harness

func runCarrierHarness(nodes map[string]*node, v *verdict, seed int64) (carrierInfo, map[uint16]harnessLabel, error) {
	info := carrierInfo{Name: "v2carrier (synthetic noise3 stand-in)", Synthetic: true,
		Note: "noise3 is not merged into v2 (T2.4) and efv2 does not exist (T0.3): udp-noise has the same wire shape and the same detectors are used"}
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
	specs := []struct {
		mode, sni, label, suite string
		conns                   int
		port                    uint16
	}{
		{"udp-noise", "cover.lab", string(censor.LabelCarrier), "udp-noise", 1, 4341},
		{"tcp-random", "cover.lab", string(censor.LabelCarrier), "tcp-random", 6, 4342},
		{"tls-go", "foreign.example", string(censor.LabelTool), "tls-go", 1, 4343},
		{"tls-chrome", "cover.lab", string(censor.LabelBrowser), "utls-chrome", 1, 4344},
		{"quic-go", "blocked.example", string(censor.LabelTool), "quic-go", 1, 4345},
		{"quic-chrome", "cover.lab", string(censor.LabelBrowser), "quic-go-chromeparrot", 1, 4346},
	}
	for _, s := range specs {
		labels[s.port] = harnessLabel{Suite: s.suite, Label: censor.Label(s.label)}
		ready := fmt.Sprintf("/tmp/v2carrier-%d.ready", s.port)
		_, _ = run("docker", "exec", container("x1"), "rm", "-f", ready)
		if out, err := run("docker", "exec", "-d", container("x1"), "/tmp/v2carrier",
			"-mode", "serve", "-listen", fmt.Sprintf(":%d", s.port), "-ready", ready); err != nil {
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
		out, err := run("docker", full...)
		if err != nil {
			v.check(false, "carrier mode %s: %v %s", s.mode, err, out)
			continue
		}
		var res struct {
			Bytes   int64 `json:"bytes"`
			Packets int64 `json:"packets"`
		}
		_ = json.Unmarshal([]byte(out), &res)
		info.Modes = append(info.Modes, s.mode)
		info.Bytes += res.Bytes
		info.Packets += res.Packets
		if s.mode == "udp-noise" || s.mode == "tcp-random" {
			logf("carrier mode %s: %d bytes in %d packets", s.mode, res.Bytes, res.Packets)
		}
	}
	return info, labels, nil
}

func buildCarrierBinary() (string, error) {
	out := filepath.Join(os.TempDir(), "echofog-v2carrier-linux")
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./testbed/v2carrier")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOWORK=off")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build testbed/v2carrier: %v\n%s", err, raw)
	}
	return out, nil
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
		lf.Label, lf.Suite = lab.Label, lab.Suite
		if lab.Label == censor.LabelCarrier {
			lf.Synthetic = true
		}
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
		{"P7/P14 UDP shape", "D10", "", ""},
	}
	for i := range rows {
		m, ok := metrics[rows[i].Detector]
		if !ok {
			rows[i].Verdict = "no data"
			continue
		}
		switch {
		case m.FPR > 0.01:
			rows[i].Verdict = "finding: browser FPR above 1%"
		case m.Positives > 0 && m.Recall < 1:
			rows[i].Verdict = "finding: missed its control flows"
		case rows[i].Detector == "D10" && carrier.Synthetic:
			rows[i].Verdict = "caught the synthetic stand-in (expected)"
		default:
			rows[i].Verdict = "control works"
		}
		rows[i].Note = fmt.Sprintf("recall %.2f, browser FPR %.3f, n=%d", m.Recall, m.FPR, m.N)
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

type v2Capture struct {
	gw        string
	out       string
	container string
	hostPath  string
	budget    int64
	size      int64
	violated  bool
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
}

func startV2Capture(gw, out string, budget int64) (*v2Capture, error) {
	for i := 0; i < 30; i++ {
		if o, err := run("docker", "exec", container(gw), "cat", "/tmp/wan_if"); err == nil && o != "" {
			break
		}
		time.Sleep(time.Second)
	}
	_, _ = run("docker", "exec", container(gw), "sh", "-c", "pkill -INT tcpdump 2>/dev/null; rm -f /tmp/wan.pcap; true")
	if o, err := run("docker", "exec", "-d", container(gw), "tcpdump", "-i", wanIf(gw), "-s", "0", "-U", "-w", "/tmp/wan.pcap", "ip"); err != nil {
		return nil, fmt.Errorf("%v %s", err, o)
	}
	c := &v2Capture{gw: gw, out: out, container: container(gw), budget: budget, done: make(chan struct{})}
	go c.monitor()
	time.Sleep(time.Second)
	return c, nil
}

func (c *v2Capture) monitor() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			out, err := run("docker", "exec", c.container, "stat", "-c", "%s", "/tmp/wan.pcap")
			if err != nil {
				continue
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
			c.mu.Lock()
			c.size = n
			over := n > c.budget && !c.violated
			if over {
				c.violated = true
			}
			c.mu.Unlock()
			if over {
				logf("pcap budget %s exceeded (%s); stopping the capture", humanBytes(c.budget), humanBytes(n))
				c.stop()
				return
			}
		}
	}
}

func (c *v2Capture) stop() {
	c.once.Do(func() {
		close(c.done)
		_, _ = run("docker", "exec", c.container, "pkill", "-INT", "tcpdump")
		time.Sleep(time.Second)
		path := filepath.Join(c.out, ".wan-"+c.gw+".pcap")
		if out, err := run("docker", "cp", c.container+":/tmp/wan.pcap", path); err != nil {
			logf("copy the capture of %s: %v %s", c.gw, err, out)
			return
		}
		c.mu.Lock()
		c.hostPath = path
		c.mu.Unlock()
		if fi, err := os.Stat(path); err == nil {
			c.mu.Lock()
			c.size = fi.Size()
			if c.size > c.budget {
				c.violated = true
			}
			c.mu.Unlock()
		}
	})
}

func (c *v2Capture) path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hostPath
}

func (c *v2Capture) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

func (c *v2Capture) overBudget() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.violated
}

// finalizeInfo renames or deletes the capture once, after the analysis decided green or red.
func (c *v2Capture) finalizeInfo(requested, green bool) pcapInfo {
	c.mu.Lock()
	size, violated, path := c.size, c.violated, c.hostPath
	c.mu.Unlock()
	info := pcapInfo{Requested: requested, BudgetBytes: c.budget, CapturedBytes: size}
	if violated {
		info.Reason = "budget exceeded; the capture was stopped"
	}
	keep := requested && !green && path != ""
	if keep {
		final := filepath.Join(c.out, "wan-"+c.gw+".pcap")
		if err := os.Rename(path, final); err == nil {
			info.Kept = true
			info.Files = []string{filepath.Base(final)}
			c.mu.Lock()
			c.hostPath = final
			c.mu.Unlock()
		}
	} else if path != "" {
		_ = os.Remove(path)
		info.Deleted = true
		switch {
		case !requested:
			info.Reason = "no -pcap; analysis used a temporary capture"
		case green:
			info.Reason = "deleted after a green analysis"
		}
		c.mu.Lock()
		c.hostPath = ""
		c.mu.Unlock()
	}
	return info
}
