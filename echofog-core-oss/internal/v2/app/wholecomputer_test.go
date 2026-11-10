package app_test

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/tuntest"
)

const exitURL2 = "echofog://join?v=2&card=EXIT2"

// remoteExit is a fake exit that knows the address of the first hop of its session.
type remoteExit struct {
	*fakeExit
	mu sync.Mutex
	ap netip.AddrPort
}

func (r *remoteExit) Remote(context.Context) (netip.AddrPort, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ap.IsValid() {
		return netip.AddrPort{}, errors.New("no session")
	}
	return r.ap, nil
}

func (r *remoteExit) set(s string) {
	r.mu.Lock()
	r.ap = netip.MustParseAddrPort(s)
	r.mu.Unlock()
}

type wholeRig struct {
	*vpnRig
	link   *remoteExit
	kernel *tuntest.Kernel
	fw     *tuntest.Firewall
	tn     *tuntest.Tunnel
	dns    *tuntest.DNS
	j      *tuntest.Journal
}

func newWholeRig(t *testing.T) *wholeRig {
	t.Helper()
	r := newRig(t, app.ModePublic)
	v := &vpnRig{rig: r, exit: &fakeExit{}, clk: &tclock{t: testNow}}
	w := &wholeRig{vpnRig: v, link: &remoteExit{fakeExit: v.exit}, kernel: &tuntest.Kernel{}, j: &tuntest.Journal{}}
	w.link.set("203.0.113.10:4433")
	w.fw = &tuntest.Firewall{K: w.kernel, J: w.j}
	w.tn = tuntest.NewTunnel()
	w.tn.J = w.j
	w.dns = &tuntest.DNS{J: w.j}
	v.dialer = &fakeDialer{link: v.exit}
	r.ln.preview[exitURL] = &links.Preview{Kind: links.KindContact, Node: nid(40), Device: nid(41), NotAfter: testNow.Add(time.Hour)}
	r.ln.peer = node.Peer{Node: node.NodeID(nid(40)), Device: node.DeviceID(nid(41)), Card: exitCard(t, true)}
	a, err := app.New(app.Config{Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: v.clk, Self: nid(1), Mode: app.ModePublic, Version: "0.3.0",
		Exit: remoteDialer{w.link}, VPNListen: "127.0.0.1:0",
		WholeComputer: &app.WholeComputerConfig{Firewall: w.fw, Tunnel: w.tn, DNS: w.dns, BypassUDPPort: func() int { return 4433 },
			Poll: time.Hour, PollInterval: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	r.a = a
	t.Cleanup(func() { _ = a.WholeComputerStop(context.Background()); _ = a.VPNStop() })
	return w
}

type remoteDialer struct{ l *remoteExit }

func (d remoteDialer) Dial(context.Context, *objects.Card) (app.ExitLink, error) { return d.l, nil }

func (w *wholeRig) startVPN(t *testing.T) {
	t.Helper()
	if _, err := w.a.VPNStart(context.Background(), exitURL); err != nil {
		t.Fatalf("VPNStart: %v", err)
	}
}

func permits(rules []tun.Rule, proto tun.Proto, addr string) bool {
	return tun.Evaluate(rules, tun.Packet{Dir: tun.Out, Iface: tun.IfacePhysical, Proto: proto, Remote: netip.MustParseAddrPort(addr), Local: 40000}) == tun.Permit
}

func journalHas(j *tuntest.Journal, s string) bool {
	for _, e := range j.Entries() {
		if e == s {
			return true
		}
	}
	return false
}

var bg = context.Background()

func TestWholeComputerNeedsDriversAndAVPN(t *testing.T) {
	v := newVPNRig(t) // built without WholeComputer
	if v.a.WholeComputerSupported() {
		t.Fatal("supported without drivers")
	}
	if err := v.a.WholeComputerStart(bg); !errors.Is(err, app.ErrWholeOff) {
		t.Fatalf("Start without drivers = %v", err)
	}
	w := newWholeRig(t)
	if !w.a.WholeComputerSupported() {
		t.Fatal("not supported with drivers")
	}
	if err := w.a.WholeComputerStart(bg); !errors.Is(err, app.ErrNeedVPN) {
		t.Fatalf("Start without a VPN = %v", err)
	}
	if len(w.j.Entries()) != 0 {
		t.Fatalf("drivers were called: %v", w.j.Entries())
	}
}

func TestWholeComputerStartPermitsOnlyTheLiveSession(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := w.j.Entries(), []string{"fw.install", "tun.up", "dns.protect"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateActive {
		t.Fatalf("status %+v", st)
	}
	rules := w.kernel.Rules()
	if !permits(rules, tun.ProtoUDP, "203.0.113.10:4433") || !permits(rules, tun.ProtoTCP, "203.0.113.10:4433") {
		t.Error("the address of the live session is not permitted (the carrier may be UDP or TCP)")
	}
	for _, other := range []string{"203.0.113.10:443", "203.0.113.11:4433", "198.51.100.1:4433"} {
		if permits(rules, tun.ProtoUDP, other) || permits(rules, tun.ProtoTCP, other) {
			t.Errorf("%s is permitted although no session talks to it", other)
		}
	}
	if permits(rules, tun.ProtoUDP, "8.8.8.8:53") {
		t.Error("DNS is permitted outside the tunnel")
	}
	// The tunnel feeds the local proxy of the VPN and keeps the node's own port off the tunnel.
	st := w.a.VPNStatus()
	if w.tn.LastConf.Socks != st.Listen || w.tn.LastConf.BypassUDPPort != 4433 {
		t.Fatalf("tunnel config %+v, proxy %s", w.tn.LastConf, st.Listen)
	}
	if !w.dns.IsProtected() || !w.dns.Last.DisableSmartNameResolution {
		t.Fatalf("DNS %+v", w.dns.Last)
	}
}

// The exit moves (a migration of the session, a new carrier): the exceptions follow through Install,
// never through Remove.
func TestMigrationUpdatesWithoutRemoving(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	w.link.set("203.0.113.99:5000")
	if err := w.a.WholeComputerSync(bg); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if journalHas(w.j, "fw.remove") {
		t.Fatalf("a migration removed the filters: %v", w.j.Entries())
	}
	rules := w.kernel.Rules()
	if !permits(rules, tun.ProtoUDP, "203.0.113.99:5000") {
		t.Error("the new address is not permitted")
	}
	if !permits(rules, tun.ProtoUDP, "203.0.113.10:4433") {
		t.Error("the previous address of the same exit went away: a redial after a drop would be blocked")
	}
	for i, snap := range w.kernel.History() {
		if len(snap) == 0 {
			t.Fatalf("rule set %d is empty: open for a moment", i)
		}
	}
	// no change, no Install
	n := w.fw.Installs
	if err := w.a.WholeComputerSync(bg); err != nil || w.fw.Installs != n {
		t.Fatalf("Sync without a change installed again: %d -> %d, %v", n, w.fw.Installs, err)
	}
}

// Another exit: the VPN is stopped for the switch and started again; the mode stays on, never
// removes the filters, and only the new exit's address stays permitted.
func TestSwitchOfExitKeepsTheModeOn(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	if err := w.a.VPNStopForSwitch(); err != nil {
		t.Fatal(err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateActive {
		t.Fatalf("the switch turned the mode %v", st.State)
	}
	// the other exit
	w.link.set("198.51.100.20:4433")
	w.ln.preview[exitURL2] = &links.Preview{Kind: links.KindContact, Node: nid(50), Device: nid(51), NotAfter: testNow.Add(time.Hour)}
	w.ln.peer = node.Peer{Node: node.NodeID(nid(50)), Device: node.DeviceID(nid(51)), Card: exitCard(t, true)}
	if _, err := w.a.VPNStart(bg, exitURL2); err != nil {
		t.Fatalf("VPNStart: %v", err)
	}
	if err := w.a.WholeComputerSync(bg); err != nil {
		t.Fatal(err)
	}
	if journalHas(w.j, "fw.remove") {
		t.Fatalf("the switch removed the filters: %v", w.j.Entries())
	}
	rules := w.kernel.Rules()
	if !permits(rules, tun.ProtoUDP, "198.51.100.20:4433") {
		t.Error("the new exit is not permitted")
	}
	if permits(rules, tun.ProtoUDP, "203.0.113.10:4433") {
		t.Error("the old exit is still permitted after the switch")
	}
}

func TestStoppingTheVPNEndsTheMode(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	if err := w.a.VPNStop(); err != nil {
		t.Fatal(err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateOff {
		t.Fatalf("state %v after the user stopped the VPN", st.State)
	}
	if len(w.kernel.Rules()) != 0 || w.tn.IsUp() || w.dns.IsProtected() {
		t.Fatal("the user's stop left filters, tunnel or DNS behind")
	}
}

func TestHeldAfterTheTunnelIsLostAndStop(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	w.tn.Kill(errors.New("helper killed"))
	eventually(t, func() bool { return w.a.WholeComputerStatus().State == tun.StateHeld })
	// The switch reports the loss the moment the helper dies; the reason comes from the app's own
	// path, so the two are read a beat apart and the test waits for both.
	eventually(t, func() bool { return w.a.WholeComputerStatus().Err != "" }, "held without a reason")
	if len(w.kernel.Rules()) == 0 {
		t.Fatal("the filters left with the helper")
	}
	// the user presses "disable protection"
	if err := w.a.WholeComputerStop(bg); err != nil {
		t.Fatal(err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateOff || len(w.kernel.Rules()) != 0 {
		t.Fatalf("after Stop: %v, %d rules", st.State, len(w.kernel.Rules()))
	}
}

func TestStartFromHeldReconnects(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	w.tn.Kill(nil)
	eventually(t, func() bool { return w.a.WholeComputerStatus().State == tun.StateHeld })
	installs := w.fw.Installs
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatalf("Start from held: %v", err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateActive || !w.tn.IsUp() {
		t.Fatalf("state %v up=%v", st.State, w.tn.IsUp())
	}
	if w.fw.Installs <= installs {
		t.Error("the exceptions were not refreshed for the new tunnel")
	}
	if journalHas(w.j, "fw.remove") {
		t.Fatal("reconnect removed the filters")
	}
}

// A new run finds the filters of the earlier one and holds the computer closed.
func TestRecoverHoldsAfterACrash(t *testing.T) {
	w := newWholeRig(t)
	rules, _ := tun.BuildRules(tun.Plan{})
	if err := w.fw.Install(bg, rules); err != nil {
		t.Fatal(err)
	}
	if err := w.a.WholeComputerRecover(bg); err != nil {
		t.Fatal(err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateHeld {
		t.Fatalf("state %v, want held", st.State)
	}
	if err := w.a.WholeComputerStop(bg); err != nil {
		t.Fatal(err)
	}
	if len(w.kernel.Rules()) != 0 {
		t.Fatal("Stop did not remove the filters of the earlier run")
	}
}

func TestWithoutPrivilegeTheModeStaysOff(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	w.fw.Unprivileged = true
	if err := w.a.WholeComputerStart(bg); !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Start = %v", err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateOff || w.tn.UpCalls != 0 {
		t.Fatalf("state %v, tunnel ups %d: the tunnel must not come up without the filters", st.State, w.tn.UpCalls)
	}
}

func eventually(t *testing.T, f func() bool, msgAndArgs ...any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal(append([]any{"condition not reached"}, msgAndArgs...)...)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// In the held state the helper may be gone; a timer that called the firewall driver would start it
// again and put a prompt for rights on the screen every few seconds. Only the user brings it back.
func TestSyncInTheHeldStateCallsNoDriver(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	w.tn.Kill(errors.New("helper killed"))
	eventually(t, func() bool { return w.a.WholeComputerStatus().State == tun.StateHeld })
	w.link.set("203.0.113.77:9000")
	installs := w.fw.Installs
	if err := w.a.WholeComputerSync(bg); err != nil {
		t.Fatal(err)
	}
	if w.fw.Installs != installs {
		t.Fatal("Sync called the firewall driver while the mode is held")
	}
}

// V: the stop that cannot restore DNS says so with its own error, keeps the mode held and can be repeated.
func TestStopThatCannotRestoreDNSStaysHeldAndCanBeRepeated(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	w.dns.FailRestore = errors.New("powershell failed")
	err := w.a.WholeComputerStop(bg)
	if !errors.Is(err, app.ErrWholeStop) {
		t.Fatalf("Stop = %v, want ErrWholeStop", err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateHeld {
		t.Fatalf("state %v", st.State)
	}
	if len(w.kernel.Rules()) == 0 {
		t.Fatal("the filters left")
	}
	w.dns.FailRestore = nil
	if err := w.a.WholeComputerStop(bg); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if st := w.a.WholeComputerStatus(); st.State != tun.StateOff || len(w.kernel.Rules()) != 0 {
		t.Fatalf("after the repeat: %v, %d rules", st.State, len(w.kernel.Rules()))
	}
}

// D (decision of the architect): DHCP is permitted so the lease of the link can renew, the LAN is not.
func TestPlanPermitsDHCPAndNotTheLAN(t *testing.T) {
	w := newWholeRig(t)
	w.startVPN(t)
	if err := w.a.WholeComputerStart(bg); err != nil {
		t.Fatal(err)
	}
	rules := w.kernel.Rules()
	dhcp := tun.Packet{Dir: tun.Out, Iface: tun.IfacePhysical, Proto: tun.ProtoUDP, Remote: netip.MustParseAddrPort("255.255.255.255:67"), Local: 68}
	if tun.Evaluate(rules, dhcp) != tun.Permit {
		t.Error("DHCP is blocked: the lease cannot renew")
	}
	for _, lan := range []string{"192.168.1.5:445", "10.0.0.7:80", "169.254.1.1:80"} {
		for _, d := range []tun.Dir{tun.Out, tun.In} {
			p := tun.Packet{Dir: d, Iface: tun.IfacePhysical, Proto: tun.ProtoTCP, Remote: netip.MustParseAddrPort(lan), Local: 40000}
			if tun.Evaluate(rules, p) != tun.Block {
				t.Errorf("%v %s is permitted: the LAN is off", d, lan)
			}
		}
	}
}
