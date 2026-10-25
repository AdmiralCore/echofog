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
)

var labNames = map[string]string{
	"203.0.113.11": "s1", "203.0.113.12": "s2", "203.0.113.13": "p1", "203.0.113.14": "p2",
	"203.0.113.15": "x1", "203.0.113.16": "x2", "203.0.113.17": "h1", "203.0.113.30": "web",
	"203.0.113.21": "gw_a", "203.0.113.22": "gw_b", "203.0.113.23": "gw_c",
}

func labLabel(a netip.AddrPort) string { return labNames[a.Addr().String()] }

func censorScenario(nodes map[string]*node, v *verdict, out string) {
	// record what leaves each network, from before any connection of the test opens: a censor
	// learns most from the first packets of a connection
	for _, gw := range gateways {
		_, _ = run("docker", "exec", "-d", container(gw), "tcpdump", "-i", wanIf(gw), "-s", "0", "-U", "-w", "/tmp/wan.pcap", "ip")
	}
	time.Sleep(2 * time.Second)
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
		censor.CheckSNI(flows, map[netip.Addr][]string{
			netip.MustParseAddr("203.0.113.15"): {"cover.lab"},
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
