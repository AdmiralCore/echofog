package main

// The live lab of T15.0f: the four real v2 carriers (noise3, reality2-tcp, reality2-quic, ws2) as
// servers on x1 and clients on b1, crossing gw_b and the wan bridge, each with a "visit" (short
// sessions of five pages) and a "long session" (minutes), next to the v1 product on the same path.
// The pcap is taken at gw_b, as for every v2-censor run; flows are labelled by the server port.

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/testbed/censor"
)

var (
	v2LiveLab = flag.Bool("live-lab", false, "v2-censor: add the real v2 carriers (servers on x1, clients on b1), a visit and a long session of each, and the v1 product for comparison")
	v2LongFor = flag.Duration("long-for", 150*time.Second, "v2-censor -live-lab: length of the long session (the heavy-flow detector looks at flows longer than 2 minutes)")
	v2Only    = flag.String("live-only", "", "v2-censor -live-lab: run only this carrier (noise3, reality2-tcp, reality2-quic, ws2); the v1 product stays out of the run")
	v2Visits  = flag.Int("visits", 8, "v2-censor -live-lab: short sessions of each carrier")
)

// activeCarriers is the carriers of this run: all four, or the one of -live-only.
func activeCarriers() []liveCarrier {
	if *v2Only == "" {
		return liveCarriers
	}
	var out []liveCarrier
	for _, c := range liveCarriers {
		if c.suite == *v2Only {
			out = append(out, c)
		}
	}
	return out
}

const pauseBetween = 12 * time.Second

const xIP = "203.0.113.15" // x1, the node of the carrier servers

type liveCarrier struct {
	suite   string
	network string
	port    uint16
}

// the ports are unused by the other modes of the harness (4341-4350)
var liveCarriers = []liveCarrier{
	{"noise3", "udp", 4361},
	{"reality2-tcp", "tcp", 4351},
	{"reality2-quic", "udp", 4352},
	{"ws2", "tcp", 4353},
}

const (
	ws2RefPort  = 4453
	quicRefPort = 4452
	liveCAFile  = "/tmp/ws2-ca.pem"
)

func liveReady(port uint16) string { return fmt.Sprintf("/tmp/v2carrier-%d.ready", port) }

// liveHarness runs the real carriers: servers, visits, then the long sessions of all carriers and
// of the v1 product at once. The servers belong to the caller, which kills them at the end.
func liveHarness(nodes map[string]*node, v *verdict, seed int64, info *carrierInfo, labels map[uint16]harnessLabel) error {
	s := strconv.FormatInt(seed, 10)
	for _, c := range activeCarriers() {
		labels[c.port] = harnessLabel{Suite: c.suite, Label: censor.LabelCarrier}
		_, _ = run("docker", "exec", container("x1"), "rm", "-f", liveReady(c.port))
		args := []string{"exec", "-d", container("x1"), "/tmp/v2carrier", "-mode", "real-serve", "-suite", c.suite,
			"-listen", fmt.Sprintf(":%d", c.port), "-advertise", fmt.Sprintf("%s:%d", xIP, c.port),
			"-cover", webIP + ":443", "-seed", s, "-ready", liveReady(c.port)}
		if c.suite == "ws2" {
			args = append(args, "-ca", "/tmp/ws2-cdn.pem", "-ref-listen", fmt.Sprintf(":%d", ws2RefPort))
		}
		if c.suite == "reality2-quic" {
			args = append(args, "-ref-listen", fmt.Sprintf(":%d", quicRefPort))
		}
		if out, err := run("docker", args...); err != nil {
			return fmt.Errorf("start the %s server: %v %s", c.suite, err, out)
		}
	}
	for _, c := range activeCarriers() {
		ready := liveReady(c.port)
		if !waitFor("the "+c.suite+" server", 30*time.Second, func() bool {
			_, err := run("docker", "exec", container("x1"), "sh", "-c", "test -f "+ready)
			return err == nil
		}) {
			return fmt.Errorf("the %s server did not become ready", c.suite)
		}
	}
	// the CDN certificate of ws2 goes to the client, as a card would carry the pin
	tmp := filepath.Join(os.TempDir(), "ws2-cdn.pem")
	if out, err := run("docker", "cp", container("x1")+":/tmp/ws2-cdn.pem", tmp); err != nil {
		return fmt.Errorf("copy the CDN certificate out of x1: %v %s", err, out)
	}
	if out, err := run("docker", "cp", tmp, container("b1")+":"+liveCAFile); err != nil {
		return fmt.Errorf("copy the CDN certificate to b1: %v %s", err, out)
	}
	_ = os.Remove(tmp)

	client := func(c liveCarrier, sessions int, d time.Duration) (string, error) {
		return run("docker", "exec", container("b1"), "/tmp/v2carrier", "-mode", "real-client", "-suite", c.suite,
			"-peer", fmt.Sprintf("%s:%d", xIP, c.port), "-ca", liveCAFile, "-seed", s,
			"-sessions", strconv.Itoa(sessions), "-duration", d.String())
	}
	count := func(c liveCarrier, out, what string) {
		var res struct {
			Bytes   int64 `json:"bytes"`
			Packets int64 `json:"packets"`
		}
		_ = json.Unmarshal([]byte(out), &res)
		info.Bytes += res.Bytes
		info.Packets += res.Packets
		logf("live %s %s: %d bytes in %d pages", c.suite, what, res.Bytes, res.Packets)
	}

	// visits: one carrier after the other, so the sessions of one carrier are not mixed with another;
	// a pause before each keeps the connection count of D8 (a 10-second window per client and
	// address) from adding two blocks, or a block and a control, into one storm
	for _, c := range activeCarriers() {
		time.Sleep(pauseBetween)
		out, err := client(c, max(1, *v2Visits), 0)
		if err != nil {
			v.check(false, "live visit of %s: %v %s", c.suite, err, out)
			continue
		}
		info.Modes = append(info.Modes, "visit:"+c.suite)
		count(c, out, "visit")
	}

	// the long sessions run together: the four carriers and the v1 product are on the path at once,
	// as a person who uses several things at the same time
	logf("long sessions of %s: the four carriers and the v1 product at once", *v2LongFor)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, c := range activeCarriers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := client(c, 0, *v2LongFor)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				v.check(false, "live long session of %s: %v %s", c.suite, err, out)
				return
			}
			info.Modes = append(info.Modes, "long:"+c.suite)
			count(c, out, "long")
		}()
	}
	if *v2Only == "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			productLong(nodes, *v2LongFor)
		}()
	}
	wg.Wait()
	return nil
}

// productLong is the v1 product through x1 for d: a page of varied size every one to three seconds.
func productLong(nodes map[string]*node, d time.Duration) {
	if err := startVPN("b2", joinAddr(nodes, "x1")); err != nil {
		logf("v1 product long session skipped: %v", err)
		return
	}
	defer func() { _ = call("b2", "POST", "/vpn/stop", nil, nil) }()
	if _, ok, _ := untilWorks("b2", 45*time.Second); !ok {
		logf("v1 product long session: the VPN did not come up")
		return
	}
	rng := rand.New(rand.NewSource(7))
	end := time.Now().Add(d)
	for i := 0; time.Now().Before(end); i++ {
		if r := fetch("b2", blobURL(2000+rng.Intn(150000), fmt.Sprintf("v1-long-%d", i)), 30*time.Second); !r.OK {
			logf("v1 product long session: download %d failed: %s", i, r.Error)
		}
		time.Sleep(time.Duration(1000+rng.Intn(2000)) * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------- the table

// longFlow is how the report tells a long session from a visit: the sessions of a visit last
// seconds, the long ones minutes.
const longFlow = 60 * time.Second

type groupRow struct {
	Flows   int                `json:"flows"`
	Flagged map[string]int     `json:"flagged"`
	N       map[string]int     `json:"n"`
	Share   map[string]float64 `json:"share"`
	AUCD6   *float64           `json:"d6_auc,omitempty"`
	H1a     *censor.H1aResult  `json:"h1a,omitempty"`
}

// byGroup gives, for every carrier and for the v1 product, the share of its flows that each detector
// flags, split into visits and long sessions, with D6's AUC and H1a. The gate's limits are applied
// by the reader; nothing here is a threshold.
func byGroup(labeled []censor.LabeledFlow, cfg censor.DetectorConfig) map[string]groupRow {
	out := map[string]groupRow{}
	groups := []struct {
		name  string
		label censor.Label
		suite string
	}{}
	for _, c := range activeCarriers() {
		groups = append(groups, struct {
			name  string
			label censor.Label
			suite string
		}{c.suite, censor.LabelCarrier, c.suite})
	}
	if *v2Only == "" {
		groups = append(groups, struct {
			name  string
			label censor.Label
			suite string
		}{"v1-product", censor.LabelProduct, "v1-product"})
	}

	for _, g := range groups {
		for _, kind := range []string{"visit", "long"} {
			// sel holds copies without flags: the gate below evaluates them again, and a flag that
			// depends on the neighbours (D8 counts the connections of the whole run) must not be
			// recomputed on a subset, nor overwrite the flags of the run
			var sel, mine []censor.LabeledFlow
			for _, f := range labeled {
				switch {
				case f.Label == censor.LabelBrowser:
					c := f
					c.Flags = nil
					sel = append(sel, c)
				case f.Label == g.label && f.Suite == g.suite && !f.Synthetic && (f.Duration >= longFlow) == (kind == "long"):
					mine = append(mine, f)
					c := f
					c.Flags = nil
					sel = append(sel, c)
				}
			}
			if len(mine) == 0 {
				continue
			}
			gate := censor.GateFor(sel, cfg, g.label)
			row := groupRow{Flows: len(mine), Flagged: map[string]int{}, N: map[string]int{}, Share: map[string]float64{}}
			for _, id := range []string{"D1", "D2", "D3", "D4", "D5", "D7", "D8", "D9", "D10"} {
				fl, n := 0, 0
				for _, f := range mine {
					if v, ok := f.Flags[id]; ok {
						n++
						if v {
							fl++
						}
					}
				}
				if n == 0 {
					continue
				}
				row.Flagged[id], row.N[id], row.Share[id] = fl, n, float64(fl)/float64(n)
			}
			for _, r := range gate.Rows {
				if r.Detector == "D6" && r.Measured && r.AUC != nil {
					a := *r.AUC
					row.AUCD6 = &a
				}
			}
			var exit, browser []censor.Flow
			for _, f := range sel {
				if f.Proto != "tcp" || len(f.Records) == 0 {
					continue
				}
				if f.Label == censor.LabelBrowser {
					browser = append(browser, f.Flow)
				} else {
					exit = append(exit, f.Flow)
				}
			}
			if len(exit) > 0 && len(browser) > 0 {
				r := censor.H1aMeasure(exit, browser)
				row.H1a = &r
			}
			out[g.name+"/"+kind] = row
		}
	}
	return out
}
