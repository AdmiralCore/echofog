package main

// censor: what EchoFog's traffic looks like to DPI at the border. The routers record what leaves
// their networks while people use the VPN over QUIC (a1, b2) and over the TCP fallback (c1),
// write to each other (a2, c2), and one of them visits the cover site as a browser would (b1, the
// control). Each capture is read the way a censor reads it (testbed/censor), and by nDPI when its
// image is there (testbed/ndpigen).

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/testbed/censor"
)

var (
	extraEnv []string // environment for the nodes the runner starts after s1 and s2
	strict   = flag.Bool("strict", false, "censor: fail when an EchoFog flow is at risk (today's baseline fails)")
	// the shape of the flows: one long connection that carries a lot does not look like a visit to
	// a site, however its packets look (step 6 of the anti-DPI plan is measured with these)
	censorFor  = flag.Duration("censor-for", 0, "censor: also keep downloading through the exit this long (a1 over QUIC, c1 over TCP), next to a browser's short visits")
	heavyAfter = flag.Duration("heavy-after", 2*time.Minute, "censor: a flow this long ...")
	heavyMB    = flag.Int("heavy-mb", 20, "censor: ... that carried this many MB counts as long and heavy")
)

var labNames = map[string]string{
	"203.0.113.11": "s1", "203.0.113.12": "s2", "203.0.113.13": "p1", "203.0.113.14": "p2",
	"203.0.113.15": "x1", "203.0.113.16": "x2", "203.0.113.17": "h1", "203.0.113.18": "x3", "203.0.113.30": "web",
	"203.0.113.21": "gw_a", "203.0.113.22": "gw_b", "203.0.113.23": "gw_c",
}

func labLabel(a netip.AddrPort) string { return labNames[a.Addr().String()] }

var captureStarted bool

// startCensorCapture records what leaves each network, from before any node behind it starts: a
// censor learns most from the first packets of a connection, and a connection a node opened while
// the test bed came up would otherwise show only its middle, which no rule can tell from noise.
func startCensorCapture() {
	for _, gw := range gateways {
		// the router writes the name of its outside interface once it is configured
		for i := 0; i < 30; i++ {
			if o, err := run("docker", "exec", container(gw), "cat", "/tmp/wan_if"); err == nil && o != "" {
				break
			}
			time.Sleep(time.Second)
		}
		_, _ = run("docker", "exec", "-d", container(gw), "tcpdump", "-i", wanIf(gw), "-s", "0", "-U", "-w", "/tmp/wan.pcap", "ip")
		if *keep {
			// a second recording that outlives the scenario (the phases of the launchpad stop every
			// tcpdump by name; this copy has another): it shows the connections of the whole run
			// from their first packet, so a phase that follows can tell a new flow from the middle
			// of an old one
			_, _ = run("docker", "exec", container(gw), "sh", "-c", "cp $(command -v tcpdump) /tmp/lpall")
			_, _ = run("docker", "exec", "-d", container(gw), "/tmp/lpall", "-i", wanIf(gw), "-s", "0", "-U", "-w", "/tmp/all.pcap", "ip")
		}
	}
	time.Sleep(2 * time.Second)
	captureStarted = true
}

func censorScenario(nodes map[string]*node, v *verdict, out string) {
	if !captureStarted {
		startCensorCapture()
	}
	// c1 speaks TCP only, and only x1 has a TCP listener (with a cover site): it joins through x1,
	// as a client whose network blocks UDP joins through a node with the fallback
	if err := callT("c1", "POST", "/dial", map[string]string{"addr": joinAddr(nodes, "x1")}, nil, 40*time.Second); err != nil {
		logf("      c1 dials x1 over TCP: %v", err)
	}
	meshUp(nodes, 2*time.Minute)

	var wg sync.WaitGroup
	browse := func(c string) {
		defer wg.Done()
		start := time.Now()
		if err := startVPN(c, joinAddr(nodes, "x1")); err != nil {
			logf("      %s: %v", c, err)
			return
		}
		_, ok, _ := untilWorks(c, *recoverMax)
		got := 0
		for i := 0; ok && i < 6; i++ {
			if r := fetch(c, blobURL(1<<20, fmt.Sprintf("censor-%s-%d", c, i)), 30*time.Second); r.OK {
				got++
			}
			_ = fetch(c, blobURL(8<<10, fmt.Sprintf("page-%s-%d", c, i)), 10*time.Second)
		}
		logf("      %s browsed through x1: %d of 6 downloads in %s", c, got, time.Since(start).Round(time.Second))
	}
	logf("a1 and b2 browse through x1 over QUIC, c1 over the TCP fallback; b1 visits the cover site as a browser; a2 and c2 write to each other")
	for _, c := range []string{"a1", "b2", "c1"} {
		wg.Add(1)
		go browse(c)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			_, _ = run("docker", "exec", container("b1"), "curl", "-sk", "-o", "/dev/null", "--resolve", "cover.lab:443:203.0.113.30",
				fmt.Sprintf("https://cover.lab/blob?size=%d&seed=control-%d", 1<<20, i))
		}
	}()
	l := &ledger{}
	for i := 0; i < 5; i++ {
		l.sendDM(nodes, "a2", "c2", fmt.Sprintf("censor %d", i))
		l.sendDM(nodes, "c2", "a2", fmt.Sprintf("censor back %d", i))
	}
	wg.Wait()
	if *censorFor > 0 {
		sustained(nodes, *censorFor)
	}
	waitDelivered(nodes, l, time.Minute)
	time.Sleep(3 * time.Second)

	var all []censor.Flow
	var report strings.Builder
	for _, gw := range gateways {
		_, _ = run("docker", "exec", container(gw), "pkill", "-INT", "tcpdump")
		time.Sleep(time.Second)
		pcap := filepath.Join(out, "wan-"+gw+".pcap")
		if o, err := run("docker", "cp", container(gw)+":/tmp/wan.pcap", pcap); err != nil {
			v.check(false, "copy the capture of %s: %v %s", gw, err, o)
			continue
		}
		flows, err := censor.Analyze(pcap)
		if err != nil {
			v.check(false, "read the capture of %s: %v", gw, err)
			continue
		}
		// what DNS would say each lab address hosts: x1 mimics the cover site, which is web's
		censor.MarkLongHeavy(flows, *heavyAfter, *heavyMB<<20)
		censor.CheckSNI(flows, map[netip.Addr][]string{
			netip.MustParseAddr("203.0.113.11"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.12"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.13"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.14"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.15"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.16"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.17"): {"cover.lab"},
			netip.MustParseAddr("203.0.113.30"): {"cover.lab"},
		})
		fmt.Fprintf(&report, "== leaving %s (%s)\n%s\n", gw, strings.Join(lanOfGW[gw], ", "), censor.Report(flows, labLabel))
		all = append(all, flows...)
		ndpi(pcap, &report)
	}
	_ = os.WriteFile(filepath.Join(out, "censor-report.txt"), []byte(report.String()), 0o644)

	// the analyzer must see an ordinary HTTPS visit as one, or nothing it says counts
	control := false
	for _, f := range all {
		if labLabel(f.Server) == "web" && f.Looks == "TLS" && f.SNI == "cover.lab" {
			control = true
		}
	}
	v.check(control, "the control, a browser visiting the cover site, is seen as TLS to cover.lab")

	shapeReport(all)

	// sum EchoFog's flows up by what a censor would make of them, bytes weighted
	type agg struct{ flows, bytes int }
	by := map[string]*agg{}
	atRisk, total := 0, 0
	for _, f := range all {
		if l := labLabel(f.Server); l == "web" || l == "" || f.BytesUp+f.BytesDn < 2048 || f.Looks == "no data" {
			continue // the control, the lab's own traffic, and stray packets
		}
		key := f.Proto + " " + f.Looks + ": " + f.Verdict()
		if by[key] == nil {
			by[key] = &agg{}
		}
		by[key].flows++
		by[key].bytes += f.BytesUp + f.BytesDn
		total += f.BytesUp + f.BytesDn
		if strings.HasPrefix(f.Verdict(), "AT RISK") {
			atRisk += f.BytesUp + f.BytesDn
		}
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return by[keys[i]].bytes > by[keys[j]].bytes })
	fmt.Println()
	logf("EchoFog traffic as a censor sees it (flows over 2 KB, all three borders):")
	for _, k := range keys {
		share := 0.0
		if total > 0 {
			share = 100 * float64(by[k].bytes) / float64(total)
		}
		logf("  %5.1f%%  %3d flows  %s", share, by[k].flows, k)
	}
	risk := 0.0
	if total > 0 {
		risk = 100 * float64(atRisk) / float64(total)
	}
	logf("visibility score: %.0f%% of EchoFog's bytes are in flows a GFW-like censor could single out", risk)
	logf("per-flow details: %s", filepath.Join(out, "censor-report.txt"))
	if *strict {
		v.check(risk == 0, "no EchoFog flow is at risk (%.0f%% of bytes are)", risk)
	}
}

// ndpi classifies a capture with nDPI when the image of testbed/ndpigen exists.
func ndpi(pcap string, report *strings.Builder) {
	if o, err := run("docker", "image", "inspect", "-f", "{{.Id}}", "echofog-ndpi"); err != nil || o == "" {
		fmt.Fprintf(report, "(nDPI: build testbed/ndpigen as echofog-ndpi to classify with it)\n\n")
		return
	}
	abs, _ := filepath.Abs(filepath.Dir(pcap))
	o, err := run("docker", "run", "--rm", "--entrypoint", "ndpiReader", "-v", abs+":/cap", "echofog-ndpi",
		"-i", "/cap/"+filepath.Base(pcap), "-v", "2")
	if err != nil {
		fmt.Fprintf(report, "(nDPI failed: %v)\n\n", err)
		return
	}
	// keep the per-protocol summary and the flow lines that name a lab address
	var keep []string
	for _, line := range strings.Split(o, "\n") {
		t := strings.TrimSpace(line)
		if strings.Contains(t, "203.0.113.") && (strings.Contains(t, "[proto:") || strings.Contains(t, "<->")) {
			keep = append(keep, t)
		}
	}
	fmt.Fprintf(report, "-- nDPI on %s\n%s\n\n", filepath.Base(pcap), strings.Join(firstN(keep, 60), "\n"))
}

// sustained keeps a1 (QUIC) and c1 (TCP fallback) downloading through x1 for d, and b1 visiting
// the cover site as a browser does, a new connection for each page, for the same time.
func sustained(nodes map[string]*node, d time.Duration) {
	logf("for %s: a1 and c1 keep downloading through x1, b1 keeps browsing the cover site", d)
	end := time.Now().Add(d)
	var wg sync.WaitGroup
	for _, c := range []string{"a1", "c1"} {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			got := 0
			for i := 0; time.Now().Before(end); i++ {
				if r := fetch(c, blobURL(4<<20, fmt.Sprintf("long-%s-%d", c, i)), 60*time.Second); r.OK {
					got += 4
				}
			}
			logf("      %s downloaded %d MB through x1", c, got)
		}(c)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; time.Now().Before(end); i++ {
			_, _ = run("docker", "exec", container("b1"), "curl", "-sk", "-o", "/dev/null", "--resolve", "cover.lab:443:203.0.113.30",
				fmt.Sprintf("https://cover.lab/blob?size=%d&seed=browse-%d", 256<<10, i))
			time.Sleep(time.Second)
		}
	}()
	wg.Wait()
}

// shapeReport prints, for EchoFog's flows to the exits and for the browser control, the longest
// flow, the biggest, and how many count as long and heavy: what step 6 has to bring down to the
// control's level.
func shapeReport(all []censor.Flow) {
	type row struct {
		flows, heavy, heavyBytes, bytes int
		longest                         time.Duration
		biggest                         int
	}
	rows := map[string]*row{}
	var order []string
	for _, f := range all {
		var group string
		switch labLabel(f.Server) {
		case "web":
			group = "browser control (tcp)"
		case "x1", "x2":
			group = "EchoFog to an exit (" + f.Proto + ")"
		default:
			continue
		}
		if f.Looks == "no data" {
			continue
		}
		r := rows[group]
		if r == nil {
			r = &row{}
			rows[group] = r
			order = append(order, group)
		}
		b := f.BytesUp + f.BytesDn
		r.flows++
		r.bytes += b
		r.longest = max(r.longest, f.Duration)
		r.biggest = max(r.biggest, b)
		if f.LongHeavy {
			r.heavy++
			r.heavyBytes += b
		}
	}
	sort.Strings(order)
	fmt.Println()
	logf("flow shape (long and heavy: at least %s and %d MB in one connection):", *heavyAfter, *heavyMB)
	logf("  %-28s %6s %9s %9s %11s %s", "", "flows", "longest", "biggest", "long+heavy", "share of bytes in them")
	for _, g := range order {
		r := rows[g]
		share := 0.0
		if r.bytes > 0 {
			share = 100 * float64(r.heavyBytes) / float64(r.bytes)
		}
		logf("  %-28s %6d %9s %8.1fMB %11d %5.1f%%", g, r.flows, r.longest.Round(time.Second), float64(r.biggest)/(1<<20), r.heavy, share)
	}
}
