package tun_test

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/tuntest"
)

// Tests of the findings of the acceptance of T10.6 (reports/T10.6-accept.md, findings 1-7).

func icmp6(dir tun.Dir, remote string, typ uint8) tun.Packet {
	return tun.Packet{Dir: dir, Iface: tun.IfacePhysical, Proto: tun.ProtoICMP, Remote: netip.MustParseAddrPort(remote), ICMPType: typ}
}

func hasBlockAll(rules []tun.Rule) bool {
	for _, v6 := range []bool{false, true} {
		for _, d := range []tun.Dir{tun.Out, tun.In} {
			found := false
			for _, r := range rules {
				if r.Action == tun.Block && r.Dir == d && r.V6 == v6 && r.Iface == tun.IfaceAny && r.Proto == tun.ProtoAny && r.RemotePort == 0 {
					found = true
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

// Finding 1: the plan changes while the mode runs, through Install, never through Remove.
func TestUpdateReplacesRulesWithoutOpeningAMoment(t *testing.T) {
	for _, from := range []string{"active", "held"} {
		t.Run(from, func(t *testing.T) {
			r := newRig(t)
			r.start()
			if from == "held" {
				r.tn.Kill(nil)
				r.waitState(tun.StateHeld)
			}
			np := testPlan()
			np.Endpoints = []tun.Endpoint{{Addr: netip.MustParseAddrPort("192.0.2.77:4433"), Proto: tun.ProtoUDP}}
			if err := r.sw.Update(context.Background(), np); err != nil {
				t.Fatalf("Update: %v", err)
			}
			for _, e := range r.j.Entries() {
				if e == "fw.remove" {
					t.Fatalf("Update removed the filters: %v", r.j.Entries())
				}
			}
			if want := mustRules(t, np); len(r.kernel.Rules()) != len(want) {
				t.Fatalf("kernel has %d rules, want the new plan's %d", len(r.kernel.Rules()), len(want))
			}
			for i, snap := range r.kernel.History() {
				if !hasBlockAll(snap) {
					t.Fatalf("rule set %d of the history has no block-all: the network was open for a moment", i)
				}
			}
			if tun.Evaluate(r.kernel.Rules(), pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "192.0.2.77:4433")) != tun.Permit {
				t.Error("the new endpoint is not permitted")
			}
			if tun.Evaluate(r.kernel.Rules(), pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "203.0.113.10:443")) != tun.Block {
				t.Error("the old endpoint is still permitted")
			}
			if l := r.leaks(); len(l) != 0 {
				t.Fatalf("leaks %v", l)
			}
			wantState := tun.StateActive
			if from == "held" {
				wantState = tun.StateHeld
			}
			if r.sw.State() != wantState {
				t.Fatalf("state %s after Update, want %s", r.sw.State(), wantState)
			}
		})
	}
}

func TestUpdateFailureKeepsTheOldRules(t *testing.T) {
	r := newRig(t)
	if err := r.sw.Update(context.Background(), testPlan()); !errors.Is(err, tun.ErrBadState) {
		t.Fatalf("Update when off = %v", err)
	}
	r.start()
	before := r.kernel.Rules()
	bad := tun.Plan{Endpoints: []tun.Endpoint{{Addr: netip.MustParseAddrPort("127.0.0.1:443"), Proto: tun.ProtoTCP}}}
	if err := r.sw.Update(context.Background(), bad); !errors.Is(err, tun.ErrBadPlan) {
		t.Fatalf("Update with a bad plan = %v", err)
	}
	r.fw.FailInstall = errors.New("bfe busy")
	if err := r.sw.Update(context.Background(), testPlan()); err == nil {
		t.Fatal("Update succeeded with Install failing")
	}
	if len(r.kernel.Rules()) != len(before) || r.sw.State() != tun.StateActive {
		t.Fatalf("a failed Update changed the rules or the state: %d rules, %s", len(r.kernel.Rules()), r.sw.State())
	}
	r.fw.FailInstall = nil
	if err := r.sw.Update(context.Background(), testPlan()); err != nil {
		t.Fatal(err)
	}
}

// Finding 2: a driver that closes Events() must not turn a live tunnel into Held or spin the CPU.
func TestClosedEventsChannelIsNotAnEvent(t *testing.T) {
	r := newRig(t)
	r.start()
	r.tn.CloseEvents()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if st := r.sw.State(); st != tun.StateActive {
			t.Fatalf("state %s after the channel was closed with the tunnel up", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { _ = r.sw.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hangs after the channel was closed")
	}
}

// Finding 3: OnState calls never overlap and come in the order of the changes.
func TestOnStateIsOrdered(t *testing.T) {
	var (
		mu       sync.Mutex
		seq      []tun.State
		inflight int32
		overlap  atomic.Bool
		gate     = make(chan struct{})
		first    sync.Once
	)
	j := &tuntest.Journal{}
	kernel := &tuntest.Kernel{}
	tn := tuntest.NewTunnel()
	sw := tun.New(tun.Config{
		Firewall: &tuntest.Firewall{K: kernel, J: j}, Tunnel: tn, DNS: &tuntest.DNS{J: j}, Plan: testPlan(),
		Tun: tun.TunConfig{Name: "echofog0", Resolver: resolver},
		OnState: func(s tun.State, _ error) {
			if atomic.AddInt32(&inflight, 1) > 1 {
				overlap.Store(true)
			}
			defer atomic.AddInt32(&inflight, -1)
			if s == tun.StateHeld {
				first.Do(func() { <-gate })
			}
			mu.Lock()
			seq = append(seq, s)
			mu.Unlock()
		},
	})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(func() { openGate(); _ = sw.Close() })
	if err := sw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tn.Kill(nil) // the watcher reports Held and sticks in OnState on the gate
	for i := 0; sw.State() != tun.StateHeld && i < 500; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- sw.Reconnect(context.Background()) }()
	time.Sleep(150 * time.Millisecond) // the second notification would be delivered now if it could overlap
	if overlap.Load() {
		t.Fatal("OnState was called from two goroutines at once")
	}
	openGate()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		mu.Lock()
		n := len(seq)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []tun.State{tun.StateActive, tun.StateHeld, tun.StateActive}
	if len(seq) != 3 || seq[0] != want[0] || seq[1] != want[1] || seq[2] != want[2] || overlap.Load() {
		t.Fatalf("notifications %v (overlap %v), want %v", seq, overlap.Load(), want)
	}
	if sw.State() != tun.StateActive {
		t.Fatalf("final state %s", sw.State())
	}
}

// Finding 4: State() does not wait for a driver call, and a loss that arrives meanwhile is handled
// after the call.
func TestStateDoesNotBlockDuringStart(t *testing.T) {
	r := newRig(t)
	r.tn.UpGate = make(chan struct{})
	errc := make(chan error, 1)
	go func() { errc <- r.sw.Start(context.Background()) }()
	for i := 0; i < 500; i++ {
		if len(r.kernel.Rules()) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	got := make(chan tun.State, 1)
	go func() { got <- r.sw.State() }()
	select {
	case st := <-got:
		if st != tun.StateOff {
			t.Fatalf("state %s while Start is running", st)
		}
	case <-time.After(time.Second):
		t.Fatal("State() hangs while Start waits for the driver")
	}
	// The tunnel is lost while Start is still going; the loss is not forgotten.
	r.tn.Kill(errors.New("lost during start"))
	close(r.tn.UpGate)
	if err := <-errc; err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.waitState(tun.StateHeld)
}

// Finding 5: IPv6 neighbour discovery on the physical interface, narrow, only with an IPv6 endpoint.
func TestNeighbourDiscoveryRule(t *testing.T) {
	rules := mustRules(t, testPlan()) // has a v6 endpoint, no AllowLAN
	tunnelEcho := tun.Packet{Dir: tun.Out, Iface: tun.IfaceTunnel, Proto: tun.ProtoICMP, Remote: netip.MustParseAddrPort("[2606:4700::1111]:0"), ICMPType: 128}
	for _, c := range []struct {
		name string
		p    tun.Packet
		want tun.Action
	}{
		{"neighbour solicitation to the solicited-node group", icmp6(tun.Out, "[ff02::1:ff00:1]:0", 135), tun.Permit},
		{"router solicitation to all routers", icmp6(tun.Out, "[ff02::2]:0", 133), tun.Permit},
		{"neighbour advertisement from a link-local address", icmp6(tun.In, "[fe80::1]:0", 136), tun.Permit},
		{"router advertisement from the router", icmp6(tun.In, "[fe80::1]:0", 134), tun.Permit},
		{"echo request to a link-local address is not ND", icmp6(tun.Out, "[fe80::1]:0", 128), tun.Block},
		{"ND type to a public address", icmp6(tun.Out, "[2606:4700::1111]:0", 135), tun.Block},
		{"ND type from a public address", icmp6(tun.In, "[2606:4700::1111]:0", 136), tun.Block},
		{"ND type to a ULA address is LAN, not ND", icmp6(tun.Out, "[fd00::1]:0", 135), tun.Block},
		{"redirect (137) is not allowed", icmp6(tun.In, "[fe80::1]:0", 137), tun.Block},
		{"UDP to the same group", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[ff02::1:ff00:1]:5000"), tun.Block},
		{"inside the tunnel everything stays permitted", tunnelEcho, tun.Permit},
	} {
		if got := tun.Evaluate(rules, c.p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// No IPv6 endpoint: no ND rule at all.
	v4only := tun.Plan{Endpoints: []tun.Endpoint{{Addr: udpEndpoint, Proto: tun.ProtoUDP}}}
	if tun.Evaluate(mustRules(t, v4only), icmp6(tun.Out, "[ff02::1:ff00:1]:0", 135)) != tun.Block {
		t.Error("ND is permitted although the plan has no IPv6 endpoint")
	}
	// The ND rules are persistent and carry the ICMP type range.
	found := false
	for _, r := range rules {
		if r.Proto == tun.ProtoICMP && r.Action == tun.Permit && r.Iface == tun.IfacePhysical {
			found = true
			if !r.Persistent || r.ICMPLo != 133 || r.ICMPHi != 136 {
				t.Errorf("ND rule %q: persistent=%v types %d-%d", r.Name, r.Persistent, r.ICMPLo, r.ICMPHi)
			}
		}
	}
	if !found {
		t.Error("no ND rule in the plan with an IPv6 endpoint")
	}
}

// Finding 6: a failed Start cleans up DNS too, with its own context.
func TestRollbackRestoresDNSAndUsesItsOwnContext(t *testing.T) {
	t.Run("dns protect fails: dns is restored", func(t *testing.T) {
		r := newRig(t)
		r.dns.FailProtect = errors.New("nrpt denied")
		_ = r.sw.Start(context.Background())
		var restores int
		for _, e := range r.j.Entries() {
			if e == "dns.restore" {
				restores++
			}
		}
		if restores == 0 {
			t.Fatalf("a half-applied DNS policy is left: %v", r.j.Entries())
		}
	})
	t.Run("start cancelled: the filters are still removed", func(t *testing.T) {
		r := newRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		r.tn.OnUp = cancel
		if err := r.sw.Start(ctx); err == nil {
			t.Fatal("Start succeeded with the context cancelled")
		}
		if r.fw.Removes != 1 || r.fw.RemoveCtxErr != nil || !r.fw.RemoveDeadline {
			t.Fatalf("rollback called Remove %d times with ctx error %v, deadline %v: it needs a live context with a timeout", r.fw.Removes, r.fw.RemoveCtxErr, r.fw.RemoveDeadline)
		}
		if r.sw.State() != tun.StateOff || len(r.kernel.Rules()) != 0 {
			t.Fatalf("state %s, %d rules: a cancelled start must end clean, not blocked", r.sw.State(), len(r.kernel.Rules()))
		}
	})
}

// Finding 7: a lost event must not leave Active over a dead tunnel.
func TestLostEventIsFoundByPolling(t *testing.T) {
	r := newRig(t)
	sw := tun.New(tun.Config{Firewall: r.fw, Tunnel: r.tn, DNS: r.dns, Plan: testPlan(),
		Tun: tun.TunConfig{Name: "echofog0", Resolver: resolver}, PollInterval: 10 * time.Millisecond})
	t.Cleanup(func() { _ = sw.Close() })
	if err := sw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tn.DropEvents = true
	r.tn.Kill(errors.New("lost, and nobody was told"))
	deadline := time.Now().Add(2 * time.Second)
	for sw.State() != tun.StateHeld && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sw.State() != tun.StateHeld {
		t.Fatalf("state %s: the dead tunnel was not noticed", sw.State())
	}
	if l := r.leaks(); len(l) != 0 {
		t.Fatalf("leaks %v", l)
	}
}

func TestPollingStopsWithClose(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		j := &tuntest.Journal{}
		sw := tun.New(tun.Config{Firewall: &tuntest.Firewall{K: &tuntest.Kernel{}, J: j}, Tunnel: tuntest.NewTunnel(),
			DNS: &tuntest.DNS{J: j}, Plan: testPlan(), PollInterval: time.Millisecond})
		_ = sw.Close()
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d -> %d: the poller outlives Close", before, after)
	}
}
