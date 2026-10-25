package main

// The VPN scenarios: people who browse through an exit, while the network breaks under them.
//
//	vpn   connecting, a whole download, reconnecting over and over, a router cut off, an exit killed
//	      or frozen, a lossy line, a neighbour behind the same router who fails handshakes, a call
//	load  many downloads at once through one exit
//	soak  hours of steady use with occasional failures, graphing goroutines, memory and open files

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	cycles     = flag.Int("cycles", 10, "vpn: how many times each client disconnects and connects again")
	soakFor    = flag.Duration("soak-for", 30*time.Minute, "soak: how long it runs")
	soakChaos  = flag.Bool("soak-chaos", true, "soak: break something every few minutes")
	recoverMax = flag.Duration("recover", 60*time.Second, "vpn: how long a client may take to work again after a failure")
)

var (
	exitNodes  = []string{"x1", "x2"}
	vpnClients = []string{"a1", "a2", "b1", "b2", "c1", "c2"}
	webBase    = "http://203.0.113.30"
	gwOf       = map[string]string{"a1": "gw_a", "a2": "gw_a", "b1": "gw_b", "b2": "gw_b", "c1": "gw_c", "c2": "gw_c"}
)

type fetchRes struct {
	OK     bool   `json:"ok"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	MS     int64  `json:"ms"`
	Error  string `json:"error"`
}

type vpnState struct {
	Running           bool   `json:"running"`
	RelayPeerID       string `json:"relayPeerId"`
	TunnelReady       bool   `json:"tunnelReady"`
	TunnelNotice      string `json:"tunnelNotice"`
	ExitState         string `json:"exitState"`
	Searching         bool   `json:"searching"`
	Switches          int    `json:"switches"`
	Refused           uint64 `json:"refused"`
	LastRefusalReason string `json:"lastRefusalReason"`
	LastRefusalHost   string `json:"lastRefusalHost"`
}

func (s vpnState) String() string {
	exit := s.RelayPeerID
	if len(exit) > 8 {
		exit = exit[:8]
	}
	return fmt.Sprintf("running=%v exit=%s state=%q ready=%v searching=%v switches=%d refused=%d last=%q/%q notice=%q",
		s.Running, exit, s.ExitState, s.TunnelReady, s.Searching, s.Switches, s.Refused, s.LastRefusalReason, s.LastRefusalHost, s.TunnelNotice)
}

func blobURL(size int, seed string) string {
	return fmt.Sprintf("%s/blob?size=%d&seed=%s", webBase, size, url.QueryEscape(seed))
}

func fetch(client, u string, timeout time.Duration) fetchRes {
	var r fetchRes
	path := "/vpn/fetch?timeout=" + timeout.String() + "&url=" + url.QueryEscape(u)
	if err := callT(client, "GET", path, nil, &r, timeout+15*time.Second); err != nil {
		r.Error = err.Error()
	}
	return r
}

func vpnStatusOf(client string) vpnState {
	var s vpnState
	_ = call(client, "GET", "/vpn/status", nil, &s)
	return s
}

// joinAddr is how a client reaches a public node inside the lab.
func joinAddr(nodes map[string]*node, name string) string {
	ip := map[string]string{"s1": "203.0.113.11", "s2": "203.0.113.12", "p1": "203.0.113.13", "p2": "203.0.113.14",
		"x1": "203.0.113.15", "x2": "203.0.113.16", "h1": "203.0.113.17"}[name]
	return ip + ":4242/" + nodes[name].id
}

// untilWorks fetches a small blob through client's proxy until one comes back whole, and returns
// how long that took; ok is false when nothing worked within limit.
func untilWorks(client string, limit time.Duration) (time.Duration, bool, fetchRes) {
	start := time.Now()
	var last fetchRes
	for i := 0; time.Since(start) < limit; i++ {
		last = fetch(client, blobURL(4096, fmt.Sprintf("probe-%s-%d", client, i)), 8*time.Second)
		if last.OK {
			return time.Since(start), true, last
		}
		time.Sleep(time.Second)
	}
	return time.Since(start), false, last
}

// stats keeps durations by label for the report.
type stats struct {
	mu   sync.Mutex
	vals map[string][]time.Duration
	fail map[string]int
}

func newStats() *stats { return &stats{vals: map[string][]time.Duration{}, fail: map[string]int{}} }

func (s *stats) add(label string, d time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.vals[label] = append(s.vals[label], d)
	} else {
		s.fail[label]++
	}
}

func (s *stats) report() {
	s.mu.Lock()
	defer s.mu.Unlock()
	labels := map[string]bool{}
	for l := range s.vals {
		labels[l] = true
	}
	for l := range s.fail {
		labels[l] = true
	}
	var names []string
	for l := range labels {
		names = append(names, l)
	}
	sort.Strings(names)
	logf("%-34s %6s %6s %8s %8s %8s", "measurement", "ok", "failed", "median", "p90", "max")
	for _, l := range names {
		v := append([]time.Duration(nil), s.vals[l]...)
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		q := func(p float64) string {
			if len(v) == 0 {
				return "-"
			}
			return v[min(len(v)-1, int(float64(len(v))*p))].Round(100 * time.Millisecond).String()
		}
		logf("%-34s %6d %6d %8s %8s %8s", l, len(v), s.fail[l], q(0.5), q(0.9), q(1))
	}
}

// diagnose writes down what a client knew when it failed, and the last minute of its log.
func diagnose(out, client, what string) {
	st := vpnStatusOf(client)
	logf("      %s on %s: %s", what, client, st)
	if out == "" {
		return
	}
	logs, _ := run("docker", "logs", "--since", "90s", container(client))
	name := fmt.Sprintf("fail-%s-%s-%s.log", time.Now().Format("150405"), client, strings.ReplaceAll(what, " ", "_"))
	_ = os.WriteFile(filepath.Join(out, name), []byte(st.String()+"\n\n"+logs), 0o644)
}

func startVPN(client, target string) error {
	return callT(client, "POST", "/vpn/start", map[string]string{"target": target}, nil, 40*time.Second)
}

// connectAll points every client at exit and waits until each one gets a page through it.
func connectAll(nodes map[string]*node, v *verdict, st *stats, out, exit string) {
	var wg sync.WaitGroup
	for _, c := range vpnClients {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			start := time.Now()
			if err := startVPN(c, joinAddr(nodes, exit)); err != nil {
				st.add("connect", 0, false)
				v.check(false, "%s connects to %s: %v", c, exit, err)
				diagnose(out, c, "connect refused")
				return
			}
			d, ok, last := untilWorks(c, *recoverMax)
			st.add("connect", time.Since(start), ok)
			v.check(ok, "%s gets a page through %s in %s", c, exit, time.Since(start).Round(100*time.Millisecond))
			if !ok {
				logf("      last attempt: %s", last.Error)
				diagnose(out, c, "connect no traffic")
			}
			_ = d
		}(c)
	}
	wg.Wait()
}

func vpnScenario(nodes map[string]*node, v *verdict, out string) {
	meshUp(nodes, 2*time.Minute)
	st := newStats()

	// 1. everybody connects to x1
	logf("every client connects to the exit x1")
	connectAll(nodes, v, st, out, "x1")

	// 2. a whole download, checked byte for byte
	logf("every client downloads 8 MB and checks it")
	var wg sync.WaitGroup
	for _, c := range vpnClients {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			r := fetch(c, blobURL(8<<20, "big-"+c), 90*time.Second)
			mbps := float64(r.Bytes) * 8 / 1e6 / max(float64(r.MS)/1000, 0.001)
			v.check(r.OK, "%s: 8 MB whole through the exit (%.1f Mbit/s) %s", c, mbps, r.Error)
			st.add("download 8MB", time.Duration(r.MS)*time.Millisecond, r.OK)
		}(c)
	}
	wg.Wait()

	// 3. disconnect and connect again, over and over: "sometimes it just does not connect"
	logf("each client disconnects and connects again %d times", *cycles)
	fails := map[string]int{}
	var fmu sync.Mutex
	for _, c := range vpnClients {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			for i := 0; i < *cycles; i++ {
				_ = call(c, "POST", "/vpn/stop", nil, nil)
				time.Sleep(time.Duration(500+rand.Intn(2500)) * time.Millisecond)
				start := time.Now()
				err := startVPN(c, joinAddr(nodes, "x1"))
				ok := err == nil
				if ok {
					_, ok, _ = untilWorks(c, 30*time.Second)
				}
				st.add("reconnect cycle", time.Since(start), ok)
				if !ok {
					fmu.Lock()
					fails[c]++
					fmu.Unlock()
					diagnose(out, c, fmt.Sprintf("cycle %d", i+1))
				}
			}
		}(c)
	}
	wg.Wait()
	total := 0
	for _, n := range fails {
		total += n
	}
	v.check(total == 0, "reconnecting %d times per client always worked (%d failures: %v)", *cycles, total, fails)

	// 4. a router loses the Internet for a while
	for _, gw := range gateways {
		pause := 20 * time.Second
		logf("cutting %s off for %s", gw, pause)
		_, _ = run("docker", "exec", container(gw), "iptables", "-I", "FORWARD", "1", "-j", "DROP")
		time.Sleep(pause)
		_, _ = run("docker", "exec", container(gw), "iptables", "-D", "FORWARD", "-j", "DROP")
		healed := time.Now()
		for _, c := range lanOfGW[gw] {
			wg.Add(1)
			go func(c string) {
				defer wg.Done()
				d, ok, _ := untilWorks(c, *recoverMax)
				st.add("after a 20s cut", d, ok)
				v.check(ok, "%s works again %s after %s came back", c, d.Round(100*time.Millisecond), gw)
				if !ok {
					diagnose(out, c, "after cut")
				}
			}(c)
		}
		wg.Wait()
		_ = healed
	}

	// 5. the exit dies: the clients must move to the other one
	logf("kill -9 x1: the clients must move to x2")
	_, _ = run("docker", "kill", container("x1"))
	for _, c := range vpnClients {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			d, ok, _ := untilWorks(c, 2**recoverMax)
			st.add("exit killed, switch", d, ok)
			v.check(ok, "%s works again %s after its exit died (%s)", c, d.Round(100*time.Millisecond), vpnStatusOf(c))
			if !ok {
				diagnose(out, c, "exit killed")
			}
		}(c)
	}
	wg.Wait()
	_, _ = run("docker", "start", container("x1"))

	// 6. the exit in use freezes: it neither answers nor closes anything
	inUse := map[string]int{}
	for _, c := range vpnClients {
		if s := vpnStatusOf(c); s.RelayPeerID != "" {
			for _, x := range exitNodes {
				if strings.HasPrefix(nodes[x].id, s.RelayPeerID[:min(8, len(s.RelayPeerID))]) {
					inUse[x]++
				}
			}
		}
	}
	frozen := "x2"
	if inUse["x1"] > inUse["x2"] {
		frozen = "x1"
	}
	logf("freezing %s, the exit most clients use (%v), for 40s", frozen, inUse)
	_, _ = run("docker", "pause", container(frozen))
	time.Sleep(5 * time.Second)
	for _, c := range vpnClients {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			d, ok, _ := untilWorks(c, 2**recoverMax)
			st.add("exit frozen", d, ok)
			v.check(ok, "%s works again %s while %s is frozen", c, d.Round(100*time.Millisecond), frozen)
			if !ok {
				diagnose(out, c, "exit frozen")
			}
		}(c)
	}
	wg.Wait()
	time.Sleep(10 * time.Second)
	_, _ = run("docker", "unpause", container(frozen))

	// 7. a bad line: loss and delay on one router
	gw := "gw_a"
	for _, c := range lanOfGW[gw] {
		if _, ok, _ := untilWorks(c, *recoverMax); !ok {
			logf("      %s does not work before the lossy line test", c)
		}
	}
	dev := wanIf(gw)
	logf("%s: 15%% loss and 150 ms delay for a minute of downloads", gw)
	_, _ = run("docker", "exec", container(gw), "sh", "-c", "tc qdisc replace dev "+dev+" root netem delay 150ms 30ms loss 15%")
	okN, badN := 0, 0
	end := time.Now().Add(time.Minute)
	for time.Now().Before(end) {
		for _, c := range lanOfGW[gw] {
			r := fetch(c, blobURL(256<<10, fmt.Sprintf("lossy-%d", okN+badN)), 30*time.Second)
			if r.OK {
				okN++
				st.add("256KB over a lossy line", time.Duration(r.MS)*time.Millisecond, true)
			} else {
				badN++
				st.add("256KB over a lossy line", 0, false)
			}
		}
	}
	_, _ = run("docker", "exec", container(gw), "tc", "qdisc", "del", "dev", dev, "root")
	v.check(badN*10 <= okN+badN, "over a lossy line at least 90%% of downloads complete (%d of %d)", okN, okN+badN)

	// 8. a neighbour behind the same router keeps failing handshakes with the exit (an old app
	// with an outdated key): the other computer must still connect
	logf("a1 fails 12 handshakes with x1 (wrong key), then a2, behind the same router, connects to x1")
	fake := strings.Repeat("ab", 32)
	for i := 0; i < 12; i++ {
		_ = callT("a1", "POST", "/dial", map[string]string{"addr": strings.Split(joinAddr(nodes, "x1"), "/")[0] + "/" + fake}, nil, 30*time.Second)
	}
	_ = call("a2", "POST", "/vpn/stop", nil, nil)
	time.Sleep(2 * time.Second)
	start := time.Now()
	err := startVPN("a2", joinAddr(nodes, "x1"))
	ok := err == nil
	if ok {
		_, ok, _ = untilWorks("a2", *recoverMax)
	}
	st.add("neighbour failed handshakes", time.Since(start), ok)
	v.check(ok, "a2 connects to x1 although a1 behind the same router failed handshakes (%v)", err)
	if !ok {
		diagnose(out, "a2", "neighbour ban")
	}

	// 9. a call to somebody behind the symmetric NAT
	logf("a1 calls b2 (behind the symmetric NAT)")
	var inv struct{ CallID, MessageID, State string }
	if err := call("a1", "POST", "/call/invite", map[string]string{"to": nodes["b2"].pubKey}, &inv); err != nil {
		v.check(false, "a1 places a call to b2: %v", err)
	} else {
		start := time.Now()
		ok := waitFor("the call invitation to reach b2", 90*time.Second, func() bool {
			var p struct{ Pending int }
			return call("a1", "GET", "/pending?peer="+nodes["b2"].id, nil, &p) == nil && p.Pending == 0
		})
		st.add("call invitation delivered", time.Since(start), ok)
		v.check(ok, "the call invitation reached b2 and was confirmed in %s (it started as %q)", time.Since(start).Round(100*time.Millisecond), inv.State)
	}

	fmt.Println()
	st.report()
}

func loadScenario(nodes map[string]*node, v *verdict, out string) {
	meshUp(nodes, 2*time.Minute)
	st := newStats()
	connectAll(nodes, v, st, out, "x1")
	before := heldOf("x1")

	type burst struct {
		OK, Failed int
		Bytes      int64
		Seconds    float64
		Mbps       float64
		Errors     map[string]int
	}
	runBurst := func(label string, n, size int, timeout time.Duration) {
		logf("%s: every client runs %d downloads of %d KB at once", label, n, size>>10)
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok, bad, mbps := 0, 0, 0.0
		errs := map[string]int{}
		for _, c := range vpnClients {
			wg.Add(1)
			go func(c string) {
				defer wg.Done()
				u := fmt.Sprintf("%s/blob?size=%d&seed=%s-{i}", webBase, size, c)
				var b burst
				path := fmt.Sprintf("/vpn/burst?n=%d&timeout=%s&url=%s", n, timeout, url.QueryEscape(u))
				if err := callT(c, "GET", path, nil, &b, timeout+30*time.Second); err != nil {
					b.Failed = n
					b.Errors = map[string]int{err.Error(): n}
				}
				mu.Lock()
				ok += b.OK
				bad += b.Failed
				mbps += b.Mbps
				for k, n := range b.Errors {
					errs[k] += n
				}
				mu.Unlock()
			}(c)
		}
		wg.Wait()
		v.check(bad*100 <= (ok+bad), "%s: at least 99%% complete (%d of %d, %.0f Mbit/s together) %v", label, ok, ok+bad, mbps, errs)
	}
	runBurst("browser", 64, 64<<10, 90*time.Second)
	runBurst("downloads", 16, 4<<20, 3*time.Minute)
	runBurst("many small", 256, 2<<10, 90*time.Second)

	peak := metricsOf("x1")
	logf("x1 under load: %s", peak)
	logf("letting the exit rest for 60s")
	time.Sleep(60 * time.Second)
	after := heldOf("x1")
	logf("x1 before %s", before)
	logf("x1 after  %s", after)
	_, alive := statusOf("x1")
	v.check(alive == nil, "x1 is still alive after the load")
	v.check(after.goroutines() <= before.goroutines()+60, "x1 gave its goroutines back (%d before, %d after)", before.goroutines(), after.goroutines())
	v.check(after.fds() <= before.fds()+30, "x1 closed its sockets (%d files before, %d after)", before.fds(), after.fds())
	v.check(after.num("heapMB") <= before.num("heapMB")+64, "x1 gave its memory back (%.0f MB before, %.0f MB after a collection)", before.num("heapMB"), after.num("heapMB"))
	st.report()
}

type metricsMap map[string]any

func (m metricsMap) num(k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return -1
}
func (m metricsMap) goroutines() int { return int(m.num("goroutines")) }
func (m metricsMap) fds() int        { return int(m.num("fds")) }
func (m metricsMap) String() string {
	return fmt.Sprintf("goroutines=%d heap=%.0fMB files=%d peers=%d exitStreams=%.0f", m.goroutines(), m.num("heapMB"), m.fds(), int(m.num("peers")), m.num("exitStreams"))
}

func metricsOf(n string) metricsMap {
	m := metricsMap{}
	_ = call(n, "GET", "/metrics", nil, &m)
	return m
}

// heldOf is metricsOf after a garbage collection: what a leak would keep.
func heldOf(n string) metricsMap {
	m := metricsMap{}
	_ = call(n, "GET", "/metrics?gc=1", nil, &m)
	return m
}

func soakScenario(nodes map[string]*node, v *verdict, l *ledger, out string) {
	meshUp(nodes, 2*time.Minute)
	st := newStats()
	connectAll(nodes, v, st, out, "x1")

	names := sortedNames(nodes)
	csvPath := filepath.Join(out, "metrics.csv")
	f, err := os.Create(csvPath)
	if err != nil {
		fatalf("metrics file: %v", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	cols := []string{"goroutines", "heapMB", "sysMB", "fds", "peers", "exitStreams", "relayCircuits", "vpnSwitches"}
	_ = w.Write(append([]string{"time", "node"}, cols...))
	first := map[string]metricsMap{}
	sample := func(held bool) map[string]metricsMap {
		now := time.Now().Format("15:04:05")
		got := map[string]metricsMap{}
		for _, n := range names {
			m := metricsOf(n)
			if held {
				m = heldOf(n)
			}
			got[n] = m
			row := []string{now, n}
			for _, c := range cols {
				row = append(row, strconv.FormatFloat(m.num(c), 'f', 1, 64))
			}
			_ = w.Write(row)
		}
		w.Flush()
		return got
	}
	for n, m := range sample(true) {
		first[n] = m
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup
	// people browse
	bg.Add(1)
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewSource(7))
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Second):
			}
			c := vpnClients[r.Intn(len(vpnClients))]
			res := fetch(c, blobURL(128<<10, fmt.Sprintf("soak-%d", i)), 30*time.Second)
			st.add("soak page", time.Duration(res.MS)*time.Millisecond, res.OK)
		}
	}()
	// and write to each other
	bg.Add(1)
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewSource(8))
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(4 * time.Second):
			}
			from, to := lanNodes[r.Intn(len(lanNodes))], lanNodes[r.Intn(len(lanNodes))]
			if from != to {
				l.sendDM(nodes, from, to, fmt.Sprintf("soak %d %s>%s", i, from, to))
			}
		}
	}()

	end := time.Now().Add(*soakFor)
	nextSample := time.Now().Add(30 * time.Second)
	nextChaos := time.Now().Add(3 * time.Minute)
	r := rand.New(rand.NewSource(9))
	for time.Now().Before(end) {
		time.Sleep(5 * time.Second)
		if time.Now().After(nextSample) {
			sample(false)
			nextSample = time.Now().Add(30 * time.Second)
		}
		if *soakChaos && time.Now().After(nextChaos) {
			switch r.Intn(4) {
			case 0:
				gw := gateways[r.Intn(len(gateways))]
				logf("soak: cutting %s off for 15s", gw)
				_, _ = run("docker", "exec", container(gw), "iptables", "-I", "FORWARD", "1", "-j", "DROP")
				time.Sleep(15 * time.Second)
				_, _ = run("docker", "exec", container(gw), "iptables", "-D", "FORWARD", "-j", "DROP")
			case 1:
				x := exitNodes[r.Intn(len(exitNodes))]
				logf("soak: restarting %s", x)
				_, _ = run("docker", "restart", "-t", "1", container(x))
			case 2:
				n := lanNodes[r.Intn(len(lanNodes))]
				logf("soak: kill -9 %s and start it again", n)
				_, _ = run("docker", "kill", container(n))
				time.Sleep(5 * time.Second)
				_, _ = run("docker", "start", container(n))
				time.Sleep(8 * time.Second)
				if strings.Contains(strings.Join(vpnClients, ","), n) {
					_ = startVPN(n, joinAddr(nodes, "x1"))
				}
			case 3:
				gw := gateways[r.Intn(len(gateways))]
				dev := wanIf(gw)
				logf("soak: 20%% loss on %s for 30s", gw)
				_, _ = run("docker", "exec", container(gw), "sh", "-c", "tc qdisc replace dev "+dev+" root netem delay 100ms loss 20%")
				time.Sleep(30 * time.Second)
				_, _ = run("docker", "exec", container(gw), "tc", "qdisc", "del", "dev", dev, "root")
			}
			nextChaos = time.Now().Add(time.Duration(2+r.Intn(4)) * time.Minute)
		}
	}
	close(stop)
	bg.Wait()
	logf("soak over; 90s of quiet before the last sample")
	time.Sleep(90 * time.Second)
	last := sample(true)

	for _, n := range names {
		a, b := first[n], last[n]
		if a.goroutines() < 0 || b.goroutines() < 0 {
			continue
		}
		growth := b.goroutines() - a.goroutines()
		v.check(growth < 150 || float64(b.goroutines()) < 1.6*float64(a.goroutines()),
			"%s: goroutines %d -> %d over the soak", n, a.goroutines(), b.goroutines())
		v.check(b.fds() < a.fds()+80, "%s: open files %d -> %d", n, a.fds(), b.fds())
		v.check(b.num("heapMB") < max(3*a.num("heapMB"), a.num("heapMB")+150), "%s: heap %.0f -> %.0f MB", n, a.num("heapMB"), b.num("heapMB"))
	}
	missing, dups := waitDelivered(nodes, l, 3*time.Minute)
	v.check(len(missing) == 0, "every message written during the soak arrived (%d missing of %d)", len(missing), len(l.dms))
	v.check(dups == 0, "no duplicates (%d)", dups)
	ok, bad := 0, 0
	st.mu.Lock()
	ok, bad = len(st.vals["soak page"]), st.fail["soak page"]
	st.mu.Unlock()
	v.check(bad*20 <= ok+bad, "at least 95%% of pages loaded during the soak (%d of %d)", ok, ok+bad)
	logf("metrics every 30s: %s", csvPath)
	st.report()
}
