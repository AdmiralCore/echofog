package main

// hidden: an exit for friends that only people with its link can use
// call:   a three-person call across the three NATs, with voice, a screen share and failures

import (
	"sync"
	"time"
)

type accessInfo struct {
	Mode   string `json:"mode"`
	Link   string `json:"link"`
	Joined []struct {
		PeerID string `json:"peerId"`
	} `json:"joined"`
}

func exitsOf(n string) []string {
	var ids []string
	_ = callT(n, "GET", "/exits", nil, &ids, 60*time.Second)
	return ids
}

func contains(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func hiddenScenario(nodes map[string]*node, v *verdict, out string) {
	meshUp(nodes, 2*time.Minute)
	st := newStats()

	var acc accessInfo
	ok := waitFor("h1 to have an invitation link", time.Minute, func() bool {
		return call("h1", "GET", "/access", nil, &acc) == nil && acc.Link != ""
	})
	v.check(ok && acc.Mode == "hidden", "h1 runs hidden and has an invitation link (mode %q)", acc.Mode)
	if !ok {
		return
	}
	logf("h1 link: %.60s…", acc.Link)

	// 1. nobody finds it: the public exit x1 shows up in the list, h1 never does
	time.Sleep(20 * time.Second) // adverts reach the DHT
	for _, c := range []string{"a1", "b1", "c2"} {
		list := exitsOf(c)
		v.check(contains(list, nodes["x1"].id), "%s finds the public exit x1 (%d exits listed)", c, len(list))
		v.check(!contains(list, nodes["h1"].id), "%s does not find the hidden exit h1", c)
	}

	// 2. a stranger who knows its address and key but has no link gets nothing through it
	err := startVPN("b1", joinAddr(nodes, "h1"))
	works := false
	if err == nil {
		_, works, _ = untilWorks("b1", 30*time.Second)
	}
	v.check(!works, "b1, without the link, gets no traffic through h1 (start: %v)", err)
	_ = call("b1", "POST", "/vpn/stop", nil, nil)

	// 3. friends with the link get in, behind any NAT
	for _, c := range []string{"a1", "b2", "c1"} {
		start := time.Now()
		err := startVPN(c, acc.Link)
		ok := err == nil
		if ok {
			_, ok, _ = untilWorks(c, *recoverMax)
		}
		st.add("join a hidden exit by link", time.Since(start), ok)
		v.check(ok, "%s gets in with the link in %s (%v)", c, time.Since(start).Round(100*time.Millisecond), err)
		if !ok {
			diagnose(out, c, "hidden join")
		}
	}
	_ = call("h1", "GET", "/access", nil, &acc)
	joined := map[string]bool{}
	for _, j := range acc.Joined {
		joined[j.PeerID] = true
	}
	v.check(joined[nodes["a1"].id] && joined[nodes["c1"].id], "h1 lists who joined (%d people)", len(acc.Joined))

	// 4. the owner takes one person's access away: that person is out, the others stay
	logf("h1 takes a1's access away")
	if err := call("h1", "POST", "/access/kick", map[string]string{"peer": nodes["a1"].id}, nil); err != nil {
		v.check(false, "h1 removes a1: %v", err)
	}
	out1 := waitFor("a1 to lose h1", 60*time.Second, func() bool {
		return !fetch("a1", blobURL(4096, "kicked"), 8*time.Second).OK
	})
	v.check(out1, "a1 cannot use h1 any more after it was removed")
	_, stillOK, _ := untilWorks("c1", 20*time.Second)
	v.check(stillOK, "c1 still uses h1")

	// 5. the hidden exit restarts: its friends come back without asking for a new link
	logf("restarting h1")
	_, _ = run("docker", "restart", "-t", "2", container("h1"))
	for _, c := range []string{"b2", "c1"} {
		d, ok, _ := untilWorks(c, 2**recoverMax)
		st.add("back after the hidden exit restarted", d, ok)
		v.check(ok, "%s works through h1 again %s after it restarted", c, d.Round(100*time.Millisecond))
		if !ok {
			diagnose(out, c, "hidden restart")
		}
	}
	st.report()
}

// ---------------------------------------------------------------------------- calls

type callRxReport struct {
	Audio     map[string]int     `json:"audio"`
	Video     map[string]int     `json:"video"`
	SilentFor map[string]float64 `json:"silentFor"`
	Damaged   int                `json:"damaged"`
}

func rxOf(n string) callRxReport {
	var r callRxReport
	_ = call(n, "GET", "/call/rx", nil, &r)
	return r
}

// heard is how many audio units n got from speaker.
func heard(n string, speaker *node) int { return rxOf(n).Audio[speaker.id[:8]] }

// hearsAgain waits until n gets fresh audio from speaker and says how long that took.
func hearsAgain(n string, speaker *node, limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	ok := waitFor(n+" to hear "+speaker.name, limit, func() bool {
		r := rxOf(n)
		s, seen := r.SilentFor[speaker.id[:8]]
		return seen && s < 2
	})
	return time.Since(start), ok
}

func callScenario(nodes map[string]*node, v *verdict, out string) {
	meshUp(nodes, 2*time.Minute)
	st := newStats()
	people := []string{"a1", "b2", "c1"} // behind a cone, the symmetric and the other cone NAT

	// a1 calls b2, as the app does: an invitation message, then everybody joins the room
	var inv struct {
		CallID      string `json:"callId"`
		Coordinator string `json:"coordinator"`
		State       string `json:"state"`
	}
	if err := call("a1", "POST", "/call/invite", map[string]string{"to": nodes["b2"].pubKey}, &inv); err != nil {
		v.check(false, "a1 calls b2: %v", err)
		return
	}
	delivered := waitFor("the invitation to be confirmed", time.Minute, func() bool {
		var p struct{ Pending int }
		return call("a1", "GET", "/pending?peer="+nodes["b2"].id, nil, &p) == nil && p.Pending == 0
	})
	v.check(delivered, "the invitation reached b2 (it started as %q)", inv.State)

	start := time.Now()
	for _, p := range people {
		if err := call(p, "POST", "/call/join", map[string]string{"callId": inv.CallID, "coordinator": inv.Coordinator}, nil); err != nil {
			v.check(false, "%s joins the call: %v", p, err)
		}
	}
	type state struct {
		InCall          bool     `json:"inCall"`
		Members         []string `json:"members"`
		DecryptFailures uint64   `json:"decryptFailures"`
	}
	formed := waitFor("everybody in the room", time.Minute, func() bool {
		for _, p := range people {
			var s state
			if call(p, "GET", "/call/state", nil, &s) != nil || !s.InCall || len(s.Members) != len(people) {
				return false
			}
		}
		return true
	})
	st.add("everybody in the call", time.Since(start), formed)
	v.check(formed, "three people across three NATs are in one call in %s", time.Since(start).Round(100*time.Millisecond))
	if !formed {
		for _, p := range people {
			var s state
			_ = call(p, "GET", "/call/state", nil, &s)
			logf("      %s: in call %v, members %v", p, s.InCall, s.Members)
		}
		return
	}

	// voice: everybody speaks for 10 s, 50 units a second
	for _, p := range people {
		_ = call(p, "POST", "/call/talk", nil, nil)
	}
	time.Sleep(10 * time.Second)
	for _, p := range people {
		for _, q := range people {
			if p == q {
				continue
			}
			got := heard(p, nodes[q])
			v.check(got >= 350, "%s hears %s: %d of ~500 voice units in 10 s", p, q, got)
		}
	}

	// a screen share: a1 streams 720p30, the others watch
	_ = call("a1", "POST", "/call/stream?id=a1-screen", nil, nil)
	for _, p := range []string{"b2", "c1"} {
		_ = call(p, "POST", "/call/watch?id=a1-screen", nil, nil)
	}
	time.Sleep(10 * time.Second)
	for _, p := range []string{"b2", "c1"} {
		got := rxOf(p).Video[nodes["a1"].id[:8]+"/a1-screen"]
		v.check(got >= 180, "%s sees a1's screen: %d of ~300 units in 10 s", p, got)
	}

	// b2 loses the Internet for 15 s: voice must come back by itself
	logf("cutting gw_b (b2) off for 15s during the call")
	_, _ = run("docker", "exec", container("gw_b"), "iptables", "-I", "FORWARD", "1", "-j", "DROP")
	time.Sleep(15 * time.Second)
	_, _ = run("docker", "exec", container("gw_b"), "iptables", "-D", "FORWARD", "-j", "DROP")
	for _, q := range []string{"a1", "c1"} {
		d, ok := hearsAgain("b2", nodes[q], *recoverMax)
		st.add("voice back after a 15s cut", d, ok)
		v.check(ok, "b2 hears %s again %s after its line came back", q, d.Round(100*time.Millisecond))
	}
	d, ok := hearsAgain("a1", nodes["b2"], *recoverMax)
	v.check(ok, "a1 hears b2 again %s after b2's line came back", d.Round(100*time.Millisecond))

	// a bad line on c1: 10 % loss, 100 ms
	dev := wanIf("gw_c")
	before := heard("c1", nodes["a1"])
	logf("gw_c (c1): 10%% loss and 100 ms delay for 15s")
	_, _ = run("docker", "exec", container("gw_c"), "sh", "-c", "tc qdisc replace dev "+dev+" root netem delay 100ms 20ms loss 10%")
	time.Sleep(15 * time.Second)
	_, _ = run("docker", "exec", container("gw_c"), "tc", "qdisc", "del", "dev", dev, "root")
	got := heard("c1", nodes["a1"]) - before
	v.check(got >= 500, "over a lossy line c1 still hears a1: %d of ~750 units in 15 s", got)

	// the one who called (the coordinator) drops out without a word: the others go on
	logf("kill -9 a1, who started the call")
	_, _ = run("docker", "kill", container("a1"))
	var wg sync.WaitGroup
	for _, pair := range [][2]string{{"b2", "c1"}, {"c1", "b2"}} {
		wg.Add(1)
		go func(listener, speaker string) {
			defer wg.Done()
			time.Sleep(3 * time.Second)
			d, ok := hearsAgain(listener, nodes[speaker], 2**recoverMax)
			st.add("voice after the caller died", d, ok)
			v.check(ok, "%s still hears %s after the caller died (%s)", listener, speaker, d.Round(100*time.Millisecond))
		}(pair[0], pair[1])
	}
	wg.Wait()
	_, _ = run("docker", "start", container("a1"))

	for _, p := range []string{"b2", "c1"} {
		var s state
		_ = call(p, "GET", "/call/state", nil, &s)
		r := rxOf(p)
		v.check(r.Damaged == 0 && s.DecryptFailures == 0, "%s: no damaged units (%d) and nothing failed to decrypt (%d)", p, r.Damaged, s.DecryptFailures)
	}
	st.report()
}
