package tun_test

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/tuntest"
)

var resolver = netip.MustParseAddr("198.18.0.2")

type rig struct {
	t      *testing.T
	sw     *tun.Switch
	kernel *tuntest.Kernel
	fw     *tuntest.Firewall
	tn     *tuntest.Tunnel
	dns    *tuntest.DNS
	j      *tuntest.Journal
	states chan tun.State
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, kernel: &tuntest.Kernel{}, j: &tuntest.Journal{}, states: make(chan tun.State, 32)}
	r.fw = &tuntest.Firewall{K: r.kernel, J: r.j}
	r.tn = tuntest.NewTunnel()
	r.tn.J = r.j
	r.dns = &tuntest.DNS{J: r.j}
	r.sw = r.build(r.fw, r.tn, r.dns)
	return r
}

func (r *rig) build(fw tun.Firewall, tn tun.Tunnel, dns tun.DNSGuard) *tun.Switch {
	sw := tun.New(tun.Config{
		Firewall: fw, Tunnel: tn, DNS: dns, Plan: testPlan(),
		Tun:     tun.TunConfig{Name: "echofog0", Addrs: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/15")}, Resolver: resolver},
		OnState: func(s tun.State, _ error) { r.states <- s },
	})
	r.t.Cleanup(func() { _ = sw.Close() })
	return sw
}

func (r *rig) waitState(want tun.State) {
	r.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if r.sw.State() == want {
			return
		}
		select {
		case <-r.states:
		case <-deadline:
			r.t.Fatalf("state %s, want %s", r.sw.State(), want)
		}
	}
}

func (r *rig) start() {
	r.t.Helper()
	if err := r.sw.Start(context.Background()); err != nil {
		r.t.Fatalf("Start: %v", err)
	}
}

func (r *rig) leaks() []string {
	var out []string
	for _, p := range []tun.Packet{
		pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "93.184.216.34:443"),
		pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "8.8.8.8:53"),
		pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[2606:4700::1111]:443"),
	} {
		if tun.Evaluate(r.kernel.Rules(), p) == tun.Permit {
			out = append(out, p.Remote.String())
		}
	}
	return out
}

func TestStartOrderAndConfig(t *testing.T) {
	r := newRig(t)
	r.start()
	if r.sw.State() != tun.StateActive {
		t.Fatalf("state %s", r.sw.State())
	}
	// Filters before the tunnel (never a moment on the open network), DNS after it is up.
	if got, want := r.j.Entries(), []string{"fw.install", "tun.up", "dns.protect"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if want := mustRules(t, testPlan()); !reflect.DeepEqual(r.kernel.Rules(), want) {
		t.Fatal("installed rules are not BuildRules(plan)")
	}
	if r.tn.LastConf.Name != "echofog0" || r.tn.LastConf.Resolver != resolver {
		t.Fatalf("tunnel config %+v", r.tn.LastConf)
	}
	if d := r.dns.Last; d.Resolver != resolver || d.Tunnel != "echofog0" || !d.DisableSmartNameResolution {
		t.Fatalf("DNS config %+v: smart name resolution must be off and the resolver the tunnel's", d)
	}
	if l := r.leaks(); len(l) != 0 {
		t.Fatalf("active state leaks %v", l)
	}
	if err := r.sw.Start(context.Background()); !errors.Is(err, tun.ErrBadState) {
		t.Fatalf("second Start = %v", err)
	}
}

// No rights: fail closed, and the tunnel is never brought up without the kill switch.
func TestStartWithoutPrivilege(t *testing.T) {
	r := newRig(t)
	r.fw.Unprivileged = true
	err := r.sw.Start(context.Background())
	if !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Start = %v, want ErrNoPrivilege", err)
	}
	if r.sw.State() != tun.StateOff || r.tn.UpCalls != 0 || r.dns.Protects != 0 {
		t.Fatalf("state %s, tunnel ups %d, dns %d", r.sw.State(), r.tn.UpCalls, r.dns.Protects)
	}
}

func TestStartRollsBack(t *testing.T) {
	boom := errors.New("boom")
	t.Run("tunnel fails", func(t *testing.T) {
		r := newRig(t)
		r.tn.FailUp = boom
		if err := r.sw.Start(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("Start = %v", err)
		}
		if r.sw.State() != tun.StateOff || len(r.kernel.Rules()) != 0 || r.dns.IsProtected() {
			t.Fatalf("not rolled back: %s, %d rules", r.sw.State(), len(r.kernel.Rules()))
		}
	})
	t.Run("dns fails", func(t *testing.T) {
		r := newRig(t)
		r.dns.FailProtect = boom
		if err := r.sw.Start(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("Start = %v", err)
		}
		if r.sw.State() != tun.StateOff || len(r.kernel.Rules()) != 0 || r.tn.IsUp() {
			t.Fatalf("not rolled back: %s, %d rules, up=%v", r.sw.State(), len(r.kernel.Rules()), r.tn.IsUp())
		}
	})
	t.Run("rollback cannot remove the filters: held, not open", func(t *testing.T) {
		r := newRig(t)
		r.tn.FailUp, r.fw.FailRemove = boom, boom
		_ = r.sw.Start(context.Background())
		if r.sw.State() != tun.StateHeld {
			t.Fatalf("state %s, want held", r.sw.State())
		}
		if len(r.kernel.Rules()) == 0 {
			t.Fatal("filters gone although removing them failed")
		}
	})
	t.Run("install fails", func(t *testing.T) {
		r := newRig(t)
		r.fw.FailInstall = boom
		if err := r.sw.Start(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("Start = %v", err)
		}
		if r.sw.State() != tun.StateOff || r.tn.UpCalls != 0 {
			t.Fatalf("state %s, tunnel ups %d", r.sw.State(), r.tn.UpCalls)
		}
	})
}

// The hole of v1: the helper dies or the adapter goes. Nothing may leave, DNS stays pinned.
func TestHelperKilledHolds(t *testing.T) {
	r := newRig(t)
	r.start()
	r.tn.Kill(errors.New("helper killed"))
	r.waitState(tun.StateHeld)
	if len(r.kernel.Rules()) == 0 {
		t.Fatal("filters removed when the helper died")
	}
	if l := r.leaks(); len(l) != 0 {
		t.Fatalf("held state leaks %v", l)
	}
	if !r.dns.IsProtected() || r.dns.Restores != 0 {
		t.Fatal("DNS protection dropped when the helper died")
	}
	if r.fw.Removes != 0 {
		t.Fatal("the firewall was cleared on a lost tunnel")
	}
}

func TestReconnect(t *testing.T) {
	r := newRig(t)
	if err := r.sw.Reconnect(context.Background()); !errors.Is(err, tun.ErrBadState) {
		t.Fatalf("Reconnect from off = %v", err)
	}
	r.start()
	if err := r.sw.Reconnect(context.Background()); !errors.Is(err, tun.ErrBadState) {
		t.Fatalf("Reconnect while active = %v", err)
	}
	r.tn.Kill(nil)
	r.waitState(tun.StateHeld)

	r.tn.FailUp = errors.New("still down")
	if err := r.sw.Reconnect(context.Background()); err == nil {
		t.Fatal("Reconnect succeeded with the tunnel failing")
	}
	if r.sw.State() != tun.StateHeld || len(r.leaks()) != 0 {
		t.Fatalf("failed reconnect left %s", r.sw.State())
	}
	r.tn.FailUp = nil
	if err := r.sw.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if r.sw.State() != tun.StateActive || !r.tn.IsUp() {
		t.Fatalf("state %s, up=%v", r.sw.State(), r.tn.IsUp())
	}
	// A second loss is seen again.
	r.tn.Kill(nil)
	r.waitState(tun.StateHeld)
}

// The node (or the whole app) crashed: the persistent filters are still there. A new run finds
// them and holds; it never starts quietly on the open network.
func TestRecoverAfterCrash(t *testing.T) {
	r := newRig(t)
	r.start()
	_ = r.sw.Close() // the process dies; the kernel keeps the filters

	fw2 := &tuntest.Firewall{K: r.kernel, J: r.j}
	tn2, dns2 := tuntest.NewTunnel(), &tuntest.DNS{J: r.j}
	sw2 := r.build(fw2, tn2, dns2)
	if err := sw2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if sw2.State() != tun.StateHeld {
		t.Fatalf("state %s, want held", sw2.State())
	}
	if fw2.Installs != 0 || fw2.Removes != 0 {
		t.Fatalf("Recover touched the filters: %d installs, %d removes", fw2.Installs, fw2.Removes)
	}
	if len(r.leaks()) != 0 {
		t.Fatal("leak after recovery")
	}
	if err := sw2.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect after recovery: %v", err)
	}
	if sw2.State() != tun.StateActive || !dns2.IsProtected() {
		t.Fatalf("state %s, dns=%v", sw2.State(), dns2.IsProtected())
	}
	if err := sw2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.kernel.Rules()) != 0 || sw2.State() != tun.StateOff {
		t.Fatal("Stop did not end it")
	}
}

func TestRecoverOnCleanComputer(t *testing.T) {
	r := newRig(t)
	if err := r.sw.Recover(context.Background()); err != nil || r.sw.State() != tun.StateOff {
		t.Fatalf("Recover = %v, state %s", err, r.sw.State())
	}
}

func TestStop(t *testing.T) {
	r := newRig(t)
	r.start()
	if err := r.sw.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"fw.install", "tun.up", "dns.protect", "tun.down", "dns.restore", "fw.remove"}
	if got := r.j.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if r.sw.State() != tun.StateOff || len(r.kernel.Rules()) != 0 {
		t.Fatalf("state %s, %d rules", r.sw.State(), len(r.kernel.Rules()))
	}
	// Stopping from held (the user chose to go back to the open network).
	r.start()
	r.tn.Kill(nil)
	r.waitState(tun.StateHeld)
	if err := r.sw.Stop(context.Background()); err != nil || r.sw.State() != tun.StateOff || len(r.kernel.Rules()) != 0 {
		t.Fatalf("Stop from held: %v, %s", err, r.sw.State())
	}
	if err := r.sw.Stop(context.Background()); err != nil {
		t.Fatalf("Stop when off = %v", err)
	}
}

func TestStopThatCannotRemoveFiltersStaysHeld(t *testing.T) {
	r := newRig(t)
	r.start()
	r.fw.FailRemove = errors.New("denied")
	if err := r.sw.Stop(context.Background()); err == nil {
		t.Fatal("Stop reported success")
	}
	if r.sw.State() != tun.StateHeld || len(r.leaks()) != 0 {
		t.Fatalf("state %s", r.sw.State())
	}
}

// A late event of a tunnel that was stopped on purpose must not turn Off into Held.
func TestLateEventAfterStop(t *testing.T) {
	r := newRig(t)
	r.start()
	if err := r.sw.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tn.Kill(errors.New("late"))
	time.Sleep(50 * time.Millisecond)
	if r.sw.State() != tun.StateOff {
		t.Fatalf("state %s after a late event", r.sw.State())
	}
}

func TestConcurrentOperations(t *testing.T) {
	r := newRig(t)
	r.start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = r.sw.State()
				_ = r.sw.Reconnect(context.Background())
				if j%7 == 0 {
					r.tn.Kill(nil)
				}
			}
		}()
	}
	wg.Wait()
	// Whatever happened, the end state never leaks.
	if st := r.sw.State(); st != tun.StateOff && len(r.leaks()) != 0 {
		t.Fatalf("state %s leaks %v", st, r.leaks())
	}
}
