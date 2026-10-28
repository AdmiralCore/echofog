// Command runner drives the Docker test bed: it builds the images, starts a small Internet with
// public nodes and nodes behind NAT routers, runs a scenario, and checks what the product promises:
// a message that the API accepted arrives, exactly once, even if nodes crash and links fail.
//
//	go run ./testbed/runner -scenario smoke
//	go run ./testbed/runner -scenario chaos -chaos-for 5m
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	project     = "efbed"
	composeFile = "testbed/docker-compose.yml"
)

var (
	scenario = flag.String("scenario", "smoke", "smoke (mesh, messages, group), chaos (the same, with failures), messaging (message/group/offline/call/stream delivery audit with a trace), vpn, load, soak (an exit and its users), hidden (an exit for friends), call (voice and video) or censor (how the traffic looks to DPI)")
	chaosFor = flag.Duration("chaos-for", 3*time.Minute, "how long failures are injected")
	settle   = flag.Duration("settle", 3*time.Minute, "how long after the chaos messages may still arrive")
	seed     = flag.Int64("seed", 0, "random seed of the chaos (0: time)")
	keep     = flag.Bool("keep", false, "leave the containers running at the end")
	noBuild  = flag.Bool("no-build", false, "do not rebuild the image")
	only     = flag.String("nodes", "", "comma-separated subset of nodes (default: all ten)")
)

var (
	publicNodes = []string{"s1", "s2", "p1", "p2"}
	lanNodes    = []string{"a1", "a2", "b1", "b2", "c1", "c2"}
	gateways    = []string{"gw_a", "gw_b", "gw_c"}
	lanOfGW     = map[string][]string{"gw_a": {"a1", "a2"}, "gw_b": {"b1", "b2"}, "gw_c": {"c1", "c2"}}
)

func logf(format string, args ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func fatalf(format string, args ...any) {
	logf("FAILED: "+format, args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------- docker

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func compose(env []string, args ...string) (string, error) {
	full := append([]string{"compose", "-f", composeFile, "-p", project}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func container(name string) string { return project + "-" + name }

// ---------------------------------------------------------------------------- nodes

type node struct {
	name   string
	id     string
	pubKey string
}

type status struct {
	Name    string   `json:"name"`
	ID      string   `json:"id"`
	PubKey  string   `json:"pubKey"`
	Peers   int      `json:"peers"`
	PeerIDs []string `json:"peerIds"`
}

// call talks to the control interface of a node from inside its container.
func call(n, method, path string, in, out any) error {
	return callT(n, method, path, in, out, 20*time.Second)
}

// callT is call with its own time limit, for requests that download through the VPN.
func callT(n, method, path string, in, out any, limit time.Duration) error {
	args := []string{"exec", container(n), "curl", "-sS", "-m", fmt.Sprint(int(limit.Seconds())), "-X", method, "-w", "\n%{http_code}"}
	if in != nil {
		raw, _ := json.Marshal(in)
		args = append(args, "-H", "Content-Type: application/json", "-d", string(raw))
	}
	args = append(args, "http://127.0.0.1:8080"+path)
	res, err := run("docker", args...)
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", n, path, err, res)
	}
	i := strings.LastIndex(res, "\n")
	if i < 0 {
		return fmt.Errorf("%s %s: no reply", n, path)
	}
	body, code := res[:i], res[i+1:]
	if code != "200" {
		return fmt.Errorf("%s %s: %s %s", n, path, code, body)
	}
	if out != nil {
		return json.Unmarshal([]byte(body), out)
	}
	return nil
}

func statusOf(n string) (status, error) {
	var s status
	err := call(n, "GET", "/status", nil, &s)
	return s, err
}

func waitFor(what string, timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(1500 * time.Millisecond)
	}
	logf("timeout waiting for %s", what)
	return false
}

// ---------------------------------------------------------------------------- bring up

func allNodeNames() []string {
	if *only != "" {
		return strings.Split(*only, ",")
	}
	return append(append([]string{}, publicNodes...), lanNodes...)
}

// afterGateways, when set, runs once the routers are up and before any node behind them starts: the
// censor scenario starts its captures there, so they see the first packet of every connection a
// node makes, not the middle of the ones it opened while the test bed came up.
var afterGateways func()

func bringUp(extra ...string) map[string]*node {
	logf("tearing down what is left of an earlier run")
	_, _ = compose(nil, "down", "-v", "--remove-orphans")
	if !*noBuild {
		logf("building the image")
		if out, err := compose(nil, "build"); err != nil {
			fatalf("build: %v\n%s", err, out)
		}
	}
	logf("starting the first two public nodes and the routers")
	if out, err := compose(nil, "up", "-d", "s1", "s2", "gw_a", "gw_b", "gw_c"); err != nil {
		fatalf("up: %v\n%s", err, out)
	}
	nodes := map[string]*node{}
	for _, n := range []string{"s1", "s2"} {
		n := n
		if !waitFor(n+" to answer", 90*time.Second, func() bool { _, err := statusOf(n); return err == nil }) {
			fatalf("%s did not start", n)
		}
		s, _ := statusOf(n)
		nodes[n] = &node{name: n, id: s.ID, pubKey: s.PubKey}
	}
	// the seeds are listed with their cover site's name, as a release lists its own: a node that
	// joins through them is camouflaged from its first packet
	if afterGateways != nil {
		afterGateways()
	}
	boot := fmt.Sprintf("203.0.113.11:4242/%s?sni=cover.lab,203.0.113.12:4242/%s?sni=cover.lab", nodes["s1"].id, nodes["s2"].id)
	logf("starting the rest, bootstrapping through s1 and s2")
	rest := []string{"up", "-d"}
	for _, n := range append(allNodeNames(), extra...) {
		if n != "s1" && n != "s2" {
			rest = append(rest, n)
		}
	}
	if out, err := compose(append([]string{"BOOTSTRAP=" + boot}, extraEnv...), rest...); err != nil {
		fatalf("up: %v\n%s", err, out)
	}
	for _, name := range append(allNodeNames(), extra...) {
		name := name
		if nodes[name] != nil || name == "web" {
			continue
		}
		if !waitFor(name+" to answer", 90*time.Second, func() bool { _, err := statusOf(name); return err == nil }) {
			fatalf("%s did not start", name)
		}
		s, _ := statusOf(name)
		nodes[name] = &node{name: name, id: s.ID, pubKey: s.PubKey}
	}
	return nodes
}

// meshUp waits until every node knows at least one other.
func meshUp(nodes map[string]*node, timeout time.Duration) {
	ok := waitFor("every node to have a peer", timeout, func() bool {
		for n := range nodes {
			s, err := statusOf(n)
			if err != nil || s.Peers < 1 {
				return false
			}
		}
		return true
	})
	names := sortedNames(nodes)
	var rows []string
	for _, n := range names {
		s, _ := statusOf(n)
		rows = append(rows, fmt.Sprintf("%s=%d", n, s.Peers))
	}
	logf("peers: %s", strings.Join(rows, " "))
	if !ok {
		fatalf("the mesh did not form")
	}
}

func sortedNames(nodes map[string]*node) []string {
	out := make([]string, 0, len(nodes))
	for n := range nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------- messages

type message struct {
	Text   string `json:"text"`
	IsMine bool   `json:"is_mine"`
}

// ledger remembers what the API accepted, to be checked later.
type ledger struct {
	mu       sync.Mutex
	dms      []dm
	groupMsg []gm
	rejected int
}

type dm struct{ from, to, text string }
type gm struct{ from, text string }

func (l *ledger) sendDM(nodes map[string]*node, from, to, text string) {
	err := call(from, "POST", "/send", map[string]string{"to": nodes[to].pubKey, "text": text}, nil)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.rejected++
		return
	}
	l.dms = append(l.dms, dm{from, to, text})
}

func (l *ledger) sendGroup(from, group, text string) {
	err := call(from, "POST", "/group/send", map[string]string{"id": group, "text": text}, nil)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.rejected++
		return
	}
	l.groupMsg = append(l.groupMsg, gm{from, text})
}

// missingDMs lists the accepted direct messages that the recipient does not have (yet).
func (l *ledger) missingDMs(nodes map[string]*node) (missing []dm, duplicates int) {
	l.mu.Lock()
	sent := append([]dm(nil), l.dms...)
	l.mu.Unlock()
	byPair := map[[2]string][]dm{}
	for _, d := range sent {
		k := [2]string{d.from, d.to}
		byPair[k] = append(byPair[k], d)
	}
	for k, list := range byPair {
		var msgs []message
		if err := call(k[1], "GET", "/history?peer="+nodes[k[0]].id, nil, &msgs); err != nil {
			missing = append(missing, list...)
			continue
		}
		count := map[string]int{}
		for _, m := range msgs {
			if !m.IsMine {
				count[m.Text]++
			}
		}
		for _, d := range list {
			switch c := count[d.text]; {
			case c == 0:
				missing = append(missing, d)
			case c > 1:
				duplicates += c - 1
			}
		}
	}
	return
}

type groupMessage struct {
	Text string `json:"text"`
}

// missingGroup lists (member, text) pairs: group messages that a member does not have.
func (l *ledger) missingGroup(members []string, group string) (missing []string, duplicates int) {
	l.mu.Lock()
	sent := append([]gm(nil), l.groupMsg...)
	l.mu.Unlock()
	for _, m := range members {
		var msgs []groupMessage
		if err := call(m, "GET", "/group/messages?id="+group+"&limit=0", nil, &msgs); err != nil {
			missing = append(missing, m+": "+err.Error())
			continue
		}
		count := map[string]int{}
		for _, x := range msgs {
			count[x.Text]++
		}
		for _, g := range sent {
			switch c := count[g.text]; {
			case c == 0:
				missing = append(missing, m+" lacks "+g.text)
			case c > 1:
				duplicates += c - 1
			}
		}
	}
	return
}

// ---------------------------------------------------------------------------- scenarios

type groupInfo struct {
	ID      string
	Members []string
}

func buildGroup(nodes map[string]*node, creator string) groupInfo {
	var g struct {
		ID string `json:"id"`
	}
	if err := call(creator, "POST", "/group/create", map[string]string{"title": "bed", "kind": "group"}, &g); err != nil {
		fatalf("create group: %v", err)
	}
	var inv struct {
		Invite string `json:"invite"`
	}
	if err := call(creator, "GET", "/group/invite?id="+g.ID, nil, &inv); err != nil {
		fatalf("invite: %v", err)
	}
	members := []string{creator}
	for _, n := range sortedNames(nodes) {
		if n == creator {
			continue
		}
		if err := call(n, "POST", "/group/join", map[string]string{"invite": inv.Invite}, nil); err != nil {
			fatalf("%s joins: %v", n, err)
		}
		members = append(members, n)
	}
	ok := waitFor("every member to know all members", 2*time.Minute, func() bool {
		for _, m := range members {
			var list []any
			if err := call(m, "GET", "/group/members?id="+g.ID, nil, &list); err != nil || len(list) != len(members) {
				return false
			}
		}
		return true
	})
	if !ok {
		fatalf("the group did not converge")
	}
	logf("group of %d members is complete", len(members))
	return groupInfo{ID: g.ID, Members: members}
}

type verdict struct {
	failed []string
}

func (v *verdict) check(ok bool, format string, args ...any) {
	if ok {
		logf("ok    "+format, args...)
		return
	}
	line := fmt.Sprintf(format, args...)
	logf("FAIL  %s", line)
	v.failed = append(v.failed, line)
}

func allToAll(nodes map[string]*node, l *ledger, tag string) {
	names := sortedNames(nodes)
	var wg sync.WaitGroup
	for _, from := range names {
		for _, to := range names {
			if from == to {
				continue
			}
			wg.Add(1)
			go func(from, to string) {
				defer wg.Done()
				l.sendDM(nodes, from, to, fmt.Sprintf("%s %s>%s", tag, from, to))
			}(from, to)
		}
	}
	wg.Wait()
}

func waitDelivered(nodes map[string]*node, l *ledger, timeout time.Duration) (missing []dm, dups int) {
	deadline := time.Now().Add(timeout)
	for {
		missing, dups = l.missingDMs(nodes)
		if len(missing) == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(4 * time.Second)
	}
}

// chatters are the nodes that chat: the lab's public nodes run in the server role (as on a VPS),
// which has no chat.
func chatters(nodes map[string]*node) map[string]*node {
	out := map[string]*node{}
	for name, n := range nodes {
		switch name {
		case "s1", "s2", "p1", "p2", "x1", "x2", "x3", "h1":
		default:
			out[name] = n
		}
	}
	return out
}

func smoke(all map[string]*node, v *verdict, l *ledger) groupInfo {
	meshUp(all, 2*time.Minute)
	nodes := chatters(all)

	logf("every node writes to every other node (%d messages), most of them for the first time", len(nodes)*(len(nodes)-1))
	start := time.Now()
	allToAll(nodes, l, "dm")
	missing, dups := waitDelivered(nodes, l, 3*time.Minute)
	v.check(len(missing) == 0, "direct messages: %d of %d delivered in %s", len(l.dms)-len(missing), len(l.dms), time.Since(start).Round(time.Second))
	for _, m := range firstN(missing, 8) {
		logf("      missing: %s -> %s %q", m.from, m.to, m.text)
	}
	v.check(dups == 0, "direct messages arrived exactly once (%d duplicates)", dups)

	creator := "a1"
	if nodes[creator] == nil {
		creator = sortedNames(nodes)[0]
	}
	g := buildGroup(nodes, creator)
	for _, m := range g.Members {
		l.sendGroup(m, g.ID, "group hello from "+m)
	}
	var gmissing []string
	var gdups int
	ok := waitFor("group messages everywhere", 3*time.Minute, func() bool {
		gmissing, gdups = l.missingGroup(g.Members, g.ID)
		return len(gmissing) == 0
	})
	_ = ok
	v.check(len(gmissing) == 0, "group messages reached every member (%d missing)", len(gmissing))
	for _, m := range firstN(gmissing, 8) {
		logf("      %s", m)
	}
	v.check(gdups == 0, "group messages arrived exactly once (%d duplicates)", gdups)
	return g
}

func firstN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------- chaos

type chaos struct {
	rng      *rand.Rand
	mu       sync.Mutex
	down     map[string]bool // crashed or frozen
	cut      map[string]bool // routers that drop everything
	impaired map[string]bool
	events   []string
	wg       sync.WaitGroup
}

func (c *chaos) note(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	logf("chaos: %s", line)
	c.mu.Lock()
	c.events = append(c.events, line)
	c.mu.Unlock()
}

func (c *chaos) alive(nodes map[string]*node) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, n := range sortedNames(nodes) {
		if !c.down[n] {
			out = append(out, n)
		}
	}
	return out
}

func (c *chaos) crash(name string) {
	c.mu.Lock()
	if c.down[name] {
		c.mu.Unlock()
		return
	}
	c.down[name] = true
	c.mu.Unlock()
	pause := time.Duration(5+c.rng.Intn(11)) * time.Second
	c.note("kill -9 %s, back in %s", name, pause)
	_, _ = run("docker", "kill", container(name))
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		time.Sleep(pause)
		_, _ = run("docker", "start", container(name))
		c.mu.Lock()
		delete(c.down, name)
		c.mu.Unlock()
		c.note("%s restarted", name)
	}()
}

func (c *chaos) freeze(name string) {
	c.mu.Lock()
	if c.down[name] {
		c.mu.Unlock()
		return
	}
	c.down[name] = true
	c.mu.Unlock()
	pause := time.Duration(8+c.rng.Intn(10)) * time.Second
	c.note("freeze %s for %s (no packets processed)", name, pause)
	_, _ = run("docker", "pause", container(name))
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		time.Sleep(pause)
		_, _ = run("docker", "unpause", container(name))
		c.mu.Lock()
		delete(c.down, name)
		c.mu.Unlock()
		c.note("%s thawed", name)
	}()
}

func wanIf(gw string) string {
	out, _ := run("docker", "exec", container(gw), "cat", "/tmp/wan_if")
	if out == "" {
		return "eth1"
	}
	return out
}

func (c *chaos) partition(gw string) {
	c.mu.Lock()
	if c.cut[gw] {
		c.mu.Unlock()
		return
	}
	c.cut[gw] = true
	c.mu.Unlock()
	pause := time.Duration(10+c.rng.Intn(15)) * time.Second
	c.note("cut %s (%s lose the Internet) for %s", gw, strings.Join(lanOfGW[gw], ","), pause)
	_, _ = run("docker", "exec", container(gw), "iptables", "-I", "FORWARD", "1", "-j", "DROP")
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		time.Sleep(pause)
		_, _ = run("docker", "exec", container(gw), "iptables", "-D", "FORWARD", "-j", "DROP")
		c.mu.Lock()
		delete(c.cut, gw)
		c.mu.Unlock()
		c.note("%s reconnected", gw)
	}()
}

func (c *chaos) impair(gw string) {
	c.mu.Lock()
	if c.impaired[gw] {
		c.mu.Unlock()
		return
	}
	c.impaired[gw] = true
	c.mu.Unlock()
	pause := time.Duration(15+c.rng.Intn(15)) * time.Second
	loss := 10 + c.rng.Intn(30)
	delay := 50 + c.rng.Intn(250)
	c.note("%s: %d%% loss, %d ms delay for %s", gw, loss, delay, pause)
	dev := wanIf(gw)
	spec := fmt.Sprintf("delay %dms %dms loss %d%%", delay, delay/4, loss)
	_, _ = run("docker", "exec", container(gw), "sh", "-c", "tc qdisc replace dev "+dev+" root netem "+spec)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		time.Sleep(pause)
		_, _ = run("docker", "exec", container(gw), "tc", "qdisc", "del", "dev", dev, "root")
		c.mu.Lock()
		delete(c.impaired, gw)
		c.mu.Unlock()
		c.note("%s: link restored", gw)
	}()
}

// healAll undoes everything that may still be broken.
func (c *chaos) healAll(nodes map[string]*node) {
	c.wg.Wait()
	for _, gw := range gateways {
		_, _ = run("docker", "exec", container(gw), "sh", "-c", "iptables -D FORWARD -j DROP 2>/dev/null; tc qdisc del dev "+wanIf(gw)+" root 2>/dev/null; true")
	}
	for n := range nodes {
		_, _ = run("docker", "unpause", container(n))
		_, _ = run("docker", "start", container(n))
	}
}

func chaosScenario(nodes map[string]*node, v *verdict, l *ledger, g groupInfo) {
	s := *seed
	if s == 0 {
		s = time.Now().UnixNano()
	}
	logf("chaos for %s, seed %d (-seed %d repeats it)", *chaosFor, s, s)
	c := &chaos{rng: rand.New(rand.NewSource(s)), down: map[string]bool{}, cut: map[string]bool{}, impaired: map[string]bool{}}

	end := time.Now().Add(*chaosFor)
	var traffic sync.WaitGroup
	stop := make(chan struct{})
	seq := 0
	var seqMu sync.Mutex
	next := func() int { seqMu.Lock(); defer seqMu.Unlock(); seq++; return seq }

	// steady traffic: every second somebody writes to somebody, every few seconds to the group
	traffic.Add(1)
	go func() {
		defer traffic.Done()
		r := rand.New(rand.NewSource(s + 1))
		t := time.NewTicker(1200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				alive := c.alive(nodes)
				if len(alive) < 2 {
					continue
				}
				from := alive[r.Intn(len(alive))]
				to := sortedNames(nodes)[r.Intn(len(nodes))]
				if from == to {
					continue
				}
				n := next()
				if n%4 == 0 {
					go l.sendGroup(from, g.ID, fmt.Sprintf("chaos group %d from %s", n, from))
				} else {
					go l.sendDM(nodes, from, to, fmt.Sprintf("chaos dm %d %s>%s", n, from, to))
				}
			}
		}
	}()

	// the failures
	for time.Now().Before(end) {
		time.Sleep(time.Duration(4+c.rng.Intn(6)) * time.Second)
		names := sortedNames(nodes)
		switch c.rng.Intn(5) {
		case 0, 1:
			c.crash(names[c.rng.Intn(len(names))])
		case 2:
			c.freeze(names[c.rng.Intn(len(names))])
		case 3:
			c.partition(gateways[c.rng.Intn(len(gateways))])
		case 4:
			c.impair(gateways[c.rng.Intn(len(gateways))])
		}
	}
	close(stop)
	traffic.Wait()
	logf("chaos over: %d direct and %d group messages were accepted, %d refused while nodes were down; healing", len(l.dms), len(l.groupMsg), l.rejected)
	c.healAll(nodes)

	logf("waiting up to %s for everything to arrive", *settle)
	meshUp(nodes, 2*time.Minute)
	missing, dups := waitDelivered(nodes, l, *settle)
	v.check(len(missing) == 0, "after the chaos every accepted direct message arrived (%d missing of %d)", len(missing), len(l.dms))
	for _, m := range firstN(missing, 12) {
		logf("      missing: %s -> %s %q", m.from, m.to, m.text)
	}
	v.check(dups == 0, "no duplicates among direct messages (%d)", dups)

	var gmissing []string
	var gdups int
	waitFor("group messages everywhere", *settle, func() bool {
		gmissing, gdups = l.missingGroup(g.Members, g.ID)
		return len(gmissing) == 0
	})
	v.check(len(gmissing) == 0, "after the chaos every accepted group message reached every member (%d missing)", len(gmissing))
	for _, m := range firstN(gmissing, 12) {
		logf("      %s", m)
	}
	v.check(gdups == 0, "no duplicates among group messages (%d)", gdups)
}

// ---------------------------------------------------------------------------- main

func main() {
	flag.Parse()
	if out, err := run("docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		fatalf("Docker is not running: %v %s", err, out)
	}
	var extra []string
	switch *scenario {
	case "vpn", "load", "soak":
		extra = []string{"x1", "x2", "web"}
	case "censor":
		extra = []string{"x1", "x2", "web"}
		extraEnv = []string{"TRANSPORT_C1=tcp"} // c1 shows the TCP fallback
		afterGateways = startCensorCapture
	case "hidden":
		extra = []string{"x1", "h1", "web"}
	}
	out := prepareOut()
	nodes := bringUp(extra...)
	l := &ledger{}
	v := &verdict{}

	defer func() {
		if !*keep {
			logf("tearing down")
			_, _ = compose(nil, "down", "-v", "--remove-orphans")
		}
	}()

	switch *scenario {
	case "vpn":
		vpnScenario(nodes, v, out)
	case "load":
		loadScenario(nodes, v, out)
	case "soak":
		soakScenario(nodes, v, l, out)
	case "hidden":
		hiddenScenario(nodes, v, out)
	case "censor":
		censorScenario(nodes, v, out)
	case "call":
		callScenario(nodes, v, out)
	case "messaging":
		messagingScenario(nodes, v, out)
	case "smoke":
		smoke(nodes, v, l)
	case "chaos":
		g := smoke(nodes, v, l)
		if len(v.failed) == 0 {
			chaosScenario(nodes, v, l, g)
		} else {
			logf("the calm part already failed; no point in breaking things")
		}
	default:
		fatalf("unknown scenario %q", *scenario)
	}

	fmt.Println()
	collectLogs(out)
	fmt.Println()
	printDeliverySummary() // the delivery trace + error summary, last, when a scenario recorded flows
	if len(v.failed) > 0 {
		logf("%d check(s) failed:", len(v.failed))
		for _, f := range v.failed {
			logf("  - %s", f)
		}
		*keep = true // leave the containers for a look
		logf("containers left running: docker compose -f %s -p %s logs <service>; stop them with: docker compose -f %s -p %s down -v", composeFile, project, composeFile, project)
		os.Exit(1)
	}
	logf("all checks passed")
}
