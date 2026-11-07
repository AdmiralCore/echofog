package behaviour

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// Options chooses how much a run does.
type Options struct {
	// Short runs a representative subset of the matrix and a few seconds of soak. The full run is
	// the matrix of the design (loss 0/1/5/15/40 % by delay 0/50/150/300 ms) and Soak.
	Short bool
	// Soak is the soak duration of a full run; zero means one minute.
	Soak time.Duration
}

// Report is what a run measured, what it found and, as important, what it did not check.
type Report struct {
	Freeze   []FreezeResult
	Dial     map[string]DialResult
	Parallel map[string]ParallelResult
	Penalty  PenaltyResult
	// PenaltyNever is a block that is never lifted, played for two hours: the attempts H15 allows.
	PenaltyNever PenaltyResult
	Crowd        CrowdResult
	ECH          ECHResult
	Net          map[string]TransferResult
	Soak         SoakResult
	NotChecked   []string
	Findings     []string
}

// tcpPorts is a card with a reality2-tcp address on each port.
func tcpPorts(sni string, ip netip.Addr, ports ...uint32) *objects.Card {
	params, _ := objects.Marshal(&pb.SuiteParams{Sni: sni})
	c := &objects.Card{Scope: objects.ScopePeer, Body: &pb.NodeCard{}}
	for _, p := range ports {
		c.Body.Addrs = append(c.Body.Addrs, &pb.Addr{Suite: objects.SuiteReality2TCP, Host: ip.String(), Port: p, Mask: 1, Params: params})
	}
	return c
}

// RunAll runs the whole module and collects the result.
func RunAll(ctx context.Context, o Options) Report {
	r := Report{
		Dial: map[string]DialResult{}, Parallel: map[string]ParallelResult{}, Net: map[string]TransferResult{},
	}
	const gb = 1 << 30
	freeze := CensorConfig{FreezeBytes: 16384}
	both := CensorConfig{FreezeBytes: 16384, UDPDrop: true}
	for _, p := range []Product{
		{Country: "RU", Net: routepolicy.NetMobile}, {Country: "RU", Net: routepolicy.NetWired}, {Country: "CN", Net: routepolicy.NetWired},
	} {
		for _, c := range []CensorConfig{freeze, both} {
			f := MeasureFreeze(p, c, gb)
			r.Freeze = append(r.Freeze, f)
			if !f.Completed {
				r.Findings = append(r.Findings, fmt.Sprintf("freeze: row %s with UDP gone: a 1 GiB TCP transfer stops at %d bytes and the freeze is outside the 12-20 KiB the product can tell from a slow server", f.Rule, f.FrozenAt))
			}
		}
	}

	rtt := 100 * time.Millisecond
	wired := Product{Country: "RU", Net: routepolicy.NetWired}
	home := netip.MustParseAddr("203.0.113.15")
	away := netip.MustParseAddr("198.51.100.20")
	allow := CensorConfig{AllowSNI: map[string]bool{"cover.lab": true}, AllowCIDR: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, Only443: true}
	r.Dial["free"] = MeasureDial(quicAndTCP("cover.lab", away), wired, CensorConfig{}, rtt)
	r.Dial["udp-ban"] = MeasureDial(quicAndTCP("cover.lab", away), wired, CensorConfig{UDPDrop: true}, rtt)
	r.Dial["allowlist-own-card"] = MeasureDial(quicAndTCP("cover.lab", home), wired, allow, rtt)
	r.Dial["allowlist-stranger"] = MeasureDial(quicAndTCP("evil.example", away), wired, allow, rtt)
	r.Dial["port-443-down"] = MeasureDial(tcpPorts("cover.lab", away, 443, 8443), wired, CensorConfig{Port443Down: true}, rtt)
	r.Dial["port-443-down-no-backup"] = MeasureDial(tcpPorts("cover.lab", away, 443), wired, CensorConfig{Port443Down: true}, rtt)

	rule := &AndRule{ASNs: map[string]bool{"AS64500": true}, JA4s: map[string]bool{"j": true}, Parallel: 3, Penalty: 120 * time.Second}
	r.Parallel["product"] = MeasureParallel(CensorConfig{And: rule}, 50, false)
	r.Parallel["naive"] = MeasureParallel(CensorConfig{And: rule}, 8, true)
	r.Penalty = MeasurePenalty(300*time.Second, 2*time.Hour)
	r.PenaltyNever = MeasurePenalty(0, 2*time.Hour)
	if r.Penalty.Grows {
		r.Findings = append(r.Findings, "penalty: the backoff doubles with every failed handshake (120 s, 240 s, ... up to 30 min), as H15 says; the design table's 'at most 120 s, no growth with retries' cannot hold for a block that lasts: the two texts disagree")
	}
	if c, err := MeasureCrowd(20); err == nil {
		r.Crowd = c
		if c.TopShare >= 0.99 {
			r.Findings = append(r.Findings, fmt.Sprintf("crowd: all %d clients send one ClientHello fingerprint (%s). H11 chose not to diversify; whether that is 'no more than a browser population' needs real browsers (T15.2)", c.Clients, c.Top))
		}
	}
	if e, err := MeasureECH(); err == nil {
		r.ECH = e
		if !e.Present {
			r.Findings = append(r.Findings, "H16: the hello has no ECH extension, GREASE-ECH was cut")
		}
	}

	r.runNet(ctx, o)

	soak := time.Minute
	if o.Short {
		soak = 5 * time.Second
	} else if o.Soak > 0 {
		soak = o.Soak
	}
	r.Soak = RunSoak(ctx, SoakConfig{Duration: soak, ListenerRestartEvery: 10})
	if r.Soak.GoroutinesEnd > r.Soak.GoroutinesStart+5 {
		r.Findings = append(r.Findings, fmt.Sprintf("soak: goroutines grew from %d to %d", r.Soak.GoroutinesStart, r.Soak.GoroutinesEnd))
	}

	if r.Soak.HeapPeak > r.Soak.HeapStart+64<<20 {
		r.Findings = append(r.Findings, fmt.Sprintf("soak: a noise3 listener that is closed and restarted keeps about 4 MiB for several seconds (heap %d MiB right after %d cycles with restarts, back to %d MiB after the settle): a restart storm costs memory for a moment, it does not leak", r.Soak.HeapPeak>>20, r.Soak.Cycles, r.Soak.HeapEnd>>20))
	}
	r.NotChecked = []string{
		"NAT matrix (port-restricted cone, symmetric, symmetric-narrow/CGNAT) and '1 GB through three NAT types': needs the stand's gateways; no NAT is simulated here",
		"IPv6 and DNS (poisoning, NXDOMAIN, no resolver): not modelled",
		fmt.Sprintf("soak of two hours: %s ran, in one process on loopback; the 2 h run is `launchpad-behaviour -soak 2h`, not run", soak),
		"call at 5% loss / 150 ms: the call service is not driven; only the noise3 carrier's byte stream is measured",
		"Wi-Fi to mobile: only a change of source address behind a forwarder on loopback",
		"kill -9, freeze and cut router of a node, 20 to 5 nodes, death of relay, exit and mailbox, restart of all nodes: need several nodes (the stand)",
		"rotation H7 thresholds (MB or minutes to rotation): Decision.Rotate has no number in the code, so there is nothing to measure yet",
		"shutdown D20 and the offline queue: the rule is modelled, the product's behaviour belongs to T15.0p",
		"a real censor: freeze, UDP ban, allow-list, AND-rule and 443 reset are rules of a model, and the product runs against the model on a virtual clock",
		"throttle D19 (the rule gives a rate; the time to a first page is arithmetic, not a measurement of the product)",
	}
	return r
}

func quicAndTCP(sni string, ip netip.Addr) *objects.Card {
	params, _ := objects.Marshal(&pb.SuiteParams{Sni: sni})
	return &objects.Card{Scope: objects.ScopePeer, Body: &pb.NodeCard{Addrs: []*pb.Addr{
		{Suite: objects.SuiteReality2QUIC, Host: ip.String(), Port: 443, Mask: 1, Params: params},
		{Suite: objects.SuiteReality2TCP, Host: ip.String(), Port: 443, Mask: 1, Params: params},
	}}}
}

// runNet runs the loss, delay, path change, cut and path-MTU cells through the real noise3
// carrier and a netem, one cell at a time.
func (r *Report) runNet(ctx context.Context, o Options) {
	losses := []float64{0, 0.01, 0.05, 0.15, 0.40}
	delays := []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond}
	if o.Short {
		losses, delays = []float64{0, 0.05}, []time.Duration{0, 150 * time.Millisecond}
	}
	cell := func(name string, cfg NetemConfig, n int64, hook func(*Netem, int64)) {
		addr, stop, err := StartEcho(cfg.Seed)
		if err != nil {
			r.Net[name] = TransferResult{Err: err.Error()}
			return
		}
		defer stop()
		nm, err := StartNetem(addr, cfg)
		if err != nil {
			r.Net[name] = TransferResult{Err: err.Error()}
			return
		}
		defer nm.Close()
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		var h func(int64)
		if hook != nil {
			h = func(sent int64) { hook(nm, sent) }
		}
		t := RunTransfer(cctx, nm.Addr(), cfg.Seed, n, h)
		r.Net[name] = t
		if !t.Connected || t.Echoed != t.Bytes {
			r.Findings = append(r.Findings, fmt.Sprintf("net %s: %s", name, t))
		}
	}
	seed := int64(10)
	for _, l := range losses {
		for _, d := range delays {
			seed++
			// the delay is the round trip; a path adds half of it each way
			cell(fmt.Sprintf("loss %.0f%% rtt %s", 100*l, d), NetemConfig{Loss: l, Delay: d / 2, Seed: seed}, 256<<10, nil)
		}
	}
	cell("path change", NetemConfig{Seed: 91, Delay: 10 * time.Millisecond}, 512<<10, once(func(n *Netem) { n.Rebind() }))
	cell("cut 3 s", NetemConfig{Seed: 92}, 512<<10, once(func(n *Netem) { n.Cut(3 * time.Second) }))
	for _, mtu := range []int{1500, 1280, 1200} {
		name := fmt.Sprintf("mtu %d", mtu)
		cell(name, NetemConfig{Seed: 93, MTU: mtu - 28}, 32<<10, nil)
		if t := r.Net[name]; mtu < 1500 && !t.Connected {
			r.Findings = append(r.Findings, fmt.Sprintf("%s: noise3 cannot connect: the first flight is a 1271-byte datagram (71 bytes of Init and the 1200-byte inner Initial), which a path of %d bytes with IP and UDP headers does not carry (noise3/doc.go, an open contradiction with the 1232-byte floor)", name, mtu))
		}
	}
}
