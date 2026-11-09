//go:build linux && !android

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/tuntest"
)

// ---- a fake command runner: the tests never touch nft or resolvectl ------------------------------

type cmdCall struct {
	stdin string
	name  string
	args  []string
}

type fakeExec struct {
	mu    sync.Mutex
	calls []cmdCall
	reply func(cmdCall) (string, error)
}

func newFakeExec(reply func(cmdCall) (string, error)) *fakeExec {
	return &fakeExec{reply: reply}
}

func (e *fakeExec) run(_ context.Context, stdin, name string, args ...string) ([]byte, error) {
	e.mu.Lock()
	c := cmdCall{stdin: stdin, name: name, args: append([]string(nil), args...)}
	e.calls = append(e.calls, c)
	reply := e.reply
	e.mu.Unlock()
	if reply == nil {
		return nil, nil
	}
	out, err := reply(c)
	return []byte(out), err
}

func (e *fakeExec) all() []cmdCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]cmdCall(nil), e.calls...)
}

func (e *fakeExec) nftApply(t *testing.T) cmdCall {
	t.Helper()
	var found []cmdCall
	for _, c := range e.all() {
		if c.name == "nft" && len(c.args) == 2 && c.args[0] == "-f" && c.args[1] == "-" {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d nft -f calls, want exactly one", len(found))
	}
	return found[0]
}

func root() int      { return 0 }
func plainUser() int { return 1000 }

func testPlan() tun.Plan {
	return tun.Plan{
		Endpoints: []tun.Endpoint{
			{Addr: netip.MustParseAddrPort("203.0.113.10:443"), Proto: tun.ProtoUDP},
			{Addr: netip.MustParseAddrPort("[2001:db8:1::5]:8443"), Proto: tun.ProtoTCP},
		},
		TunnelPool: tun.DefaultTunnelPool,
		AllowDHCP:  true,
	}
}

func testRules(t *testing.T) []tun.Rule {
	t.Helper()
	r, err := tun.BuildRules(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newTestFirewall(t *testing.T, e *fakeExec, euid func() int) *firewall {
	t.Helper()
	return &firewall{run: e.run, euid: euid, marker: filepath.Join(t.TempDir(), "killswitch.on")}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// ---- the table and the script -------------------------------------------------------------------

func TestNTFTableNameIsFixed(t *testing.T) {
	if TableName != "echofog_ks" {
		t.Fatalf("the table name is %q: filters of an older version would be orphaned", TableName)
	}
}

func TestInstallIsOneAtomicReplace(t *testing.T) {
	e := newFakeExec(nil) // every command succeeds: the table exists
	f := newTestFirewall(t, e, root)
	if err := f.Install(context.Background(), testRules(t)); err != nil {
		t.Fatal(err)
	}
	c := e.nftApply(t)
	s := c.stdin
	for _, want := range []string{
		"add table inet echofog_ks",
		"flush table inet echofog_ks",
		"delete chain inet echofog_ks out",
		"delete chain inet echofog_ks in",
		"add chain inet echofog_ks out { type filter hook output priority 0; policy drop; }",
		"add chain inet echofog_ks in { type filter hook input priority 0; policy drop; }",
		"oifname != \"echofog0\" oifname != \"lo\"",
		"iifname != \"echofog0\" iifname != \"lo\"",
		"ip daddr 203.0.113.10/32 udp dport 443 accept",
		"iifname != \"echofog0\" iifname != \"lo\" ip saddr 203.0.113.10/32 udp sport 443 accept",
		"ip6 daddr 2001:db8:1::5/128 tcp dport 8443 accept",
		"ip daddr 255.255.255.255/32 udp sport 68 udp dport 67 accept",
		"ip6 saddr ff02::/16 udp sport 547 udp dport 546 accept",
		"ip6 daddr fe80::/10 icmpv6 type 133-136 accept",
	} {
		if !contains(s, want) {
			t.Errorf("the script has no %q\n%s", want, s)
		}
	}
	// DNS blocks outweigh every permit and come before them: nft is first match.
	dns := strings.Index(s, `tcp dport 53 drop`)
	ep := strings.Index(s, "udp dport 443 accept")
	if dns < 0 || ep < 0 || dns > ep {
		t.Errorf("the DNS block is not before the endpoint permit:\n%s", s)
	}
	// The lowest weight, block-all, closes each chain.
	if !strings.HasSuffix(s, "add rule inet echofog_ks in drop\n") {
		t.Errorf("the input chain does not end with the block-all rule:\n%s", s)
	}
	if n := strings.Count(s, "add chain inet echofog_ks"); n != 2 {
		t.Errorf("%d chains in the script, want 2", n)
	}
}

func TestFirstInstallCreatesTheTable(t *testing.T) {
	e := newFakeExec(func(c cmdCall) (string, error) {
		if c.name == "nft" && len(c.args) > 0 && c.args[0] == "list" {
			return "Error: No such file or directory", errors.New("exit status 1")
		}
		return "", nil
	})
	f := newTestFirewall(t, e, root)
	if err := f.Install(context.Background(), testRules(t)); err != nil {
		t.Fatal(err)
	}
	s := e.nftApply(t).stdin
	if contains(s, "flush table") || contains(s, "delete chain") {
		t.Errorf("a table that is not there is not flushed or deleted:\n%s", s)
	}
	if !contains(s, "add chain inet echofog_ks out") {
		t.Errorf("no output chain:\n%s", s)
	}
}

func TestInstallRefusesAnOpenSet(t *testing.T) {
	e := newFakeExec(nil)
	f := newTestFirewall(t, e, root)
	open := []tun.Rule{{Name: "permit-all", Action: tun.Permit, Dir: tun.Out, Weight: 10, Persistent: true}}
	if err := f.Install(context.Background(), open); !errors.Is(err, tun.ErrBadRules) {
		t.Fatalf("Install(open set) = %v, want ErrBadRules", err)
	}
	if len(e.all()) != 0 {
		t.Fatalf("nft was touched: %v", e.all())
	}
}

func TestInstallWithoutRoot(t *testing.T) {
	e := newFakeExec(nil)
	f := newTestFirewall(t, e, plainUser)
	if err := f.Install(context.Background(), testRules(t)); !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Install = %v, want ErrNoPrivilege", err)
	}
	if len(e.all()) != 0 {
		t.Fatalf("nft was touched without rights: %v", e.all())
	}
}

func TestInstallMapsTheRefusalOfNft(t *testing.T) {
	e := newFakeExec(func(c cmdCall) (string, error) {
		if c.name == "nft" && len(c.args) > 0 && c.args[0] == "-f" {
			return "Error: Could not process rule: Operation not permitted", errors.New("exit status 1")
		}
		return "", errors.New("exit status 1")
	})
	f := newTestFirewall(t, e, root)
	if err := f.Install(context.Background(), testRules(t)); !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Install = %v, want ErrNoPrivilege", err)
	}
}

func TestRemoveDeletesTheTableAndTheMarker(t *testing.T) {
	e := newFakeExec(nil)
	f := newTestFirewall(t, e, root)
	if err := f.Install(context.Background(), testRules(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatalf("the marker of the installed filters: %v", err)
	}
	if err := f.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker is still there: %v", err)
	}
	var deleted bool
	for _, c := range e.all() {
		if c.name == "nft" && strings.Join(c.args, " ") == "delete table inet echofog_ks" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("the table was not deleted: %v", e.all())
	}
	// A second Remove finds no table and is not an error.
	e2 := newFakeExec(func(c cmdCall) (string, error) {
		return "Error: Could not process rule: No such file or directory", errors.New("exit status 1")
	})
	f2 := newTestFirewall(t, e2, root)
	if err := f2.Remove(context.Background()); err != nil {
		t.Fatalf("Remove of a missing table = %v", err)
	}
}

func TestRemoveWithoutRoot(t *testing.T) {
	e := newFakeExec(nil)
	f := newTestFirewall(t, e, plainUser)
	if err := f.Remove(context.Background()); !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Remove = %v, want ErrNoPrivilege", err)
	}
	if len(e.all()) != 0 {
		t.Fatalf("nft was touched without rights: %v", e.all())
	}
}

func TestInstalledAnswersWithoutRights(t *testing.T) {
	e := newFakeExec(nil)
	f := newTestFirewall(t, e, plainUser)
	if err := os.WriteFile(f.marker, []byte("on"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := f.Installed(context.Background())
	if err != nil || !ok {
		t.Fatalf("Installed = %v, %v", ok, err)
	}
	if len(e.all()) != 0 {
		t.Fatalf("nft was asked by an unelevated process: %v", e.all())
	}
	if err := os.Remove(f.marker); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.Installed(context.Background()); ok {
		t.Fatal("Installed says yes without a marker and without rights")
	}
}

func TestInstalledAsksNftWithRoot(t *testing.T) {
	e := newFakeExec(nil)
	f := newTestFirewall(t, e, root)
	if ok, err := f.Installed(context.Background()); err != nil || !ok {
		t.Fatalf("Installed = %v, %v", ok, err)
	}
	e2 := newFakeExec(func(cmdCall) (string, error) { return "No such file or directory", errors.New("exit status 1") })
	f2 := newTestFirewall(t, e2, root)
	if ok, err := f2.Installed(context.Background()); err != nil || ok {
		t.Fatalf("Installed = %v, %v", ok, err)
	}
}

func TestRemoveCountedCountsTheRules(t *testing.T) {
	e := newFakeExec(func(c cmdCall) (string, error) {
		if c.name == "nft" && len(c.args) > 1 && c.args[0] == "-j" {
			return `{"nftables":[{"metainfo":{}},{"table":{}},{"chain":{}},{"rule":{"expr":[]}},{"rule":{"expr":[]}}]}`, nil
		}
		return "", nil
	})
	f := newTestFirewall(t, e, root)
	n, err := f.removeCounted(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("removeCounted = %d, %v", n, err)
	}
}

// ---- DNS over resolvectl -------------------------------------------------------------------------

const statusText = `Global
         Protocols: -LLMNR -mDNS -DNSOverTLS DNSSEC=no/unsupported
  resolv.conf mode: stub

Link 2 (eth0)
    Current Scopes: DNS
         Protocols: +DefaultRoute -LLMNR -mDNS -DNSOverTLS
     Default Route: yes
Current DNS Server: 192.168.1.1
       DNS Servers: 192.168.1.1

Link 3 (docker0)
    Current Scopes: none
         Protocols: -DefaultRoute -LLMNR -mDNS -DNSOverTLS
     Default Route: no

Link 7 (echofog0)
    Current Scopes: DNS
         Protocols: +DefaultRoute -LLMNR -mDNS -DNSOverTLS
     Default Route: yes
       DNS Servers: 198.18.0.2
`

const dnsText = `Global: 10.255.255.254
Link 2 (eth0): 192.168.1.1
Link 3 (docker0):
Link 7 (echofog0): 198.18.0.2
`

const domainText = `Global: ~.
Link 2 (eth0): ~lan
Link 3 (docker0):
Link 7 (echofog0): ~.
`

func resolveReply(c cmdCall) (string, error) {
	if c.name != "resolvectl" {
		return "", nil
	}
	switch {
	case len(c.args) == 1 && c.args[0] == "status":
		return statusText, nil
	case len(c.args) == 1 && c.args[0] == "dns":
		return dnsText, nil
	case len(c.args) == 1 && c.args[0] == "domain":
		return domainText, nil
	}
	return "", nil
}

func newTestDNS(t *testing.T, e *fakeExec) *dnsGuard {
	t.Helper()
	return &dnsGuard{run: e.run, euid: root, state: filepath.Join(t.TempDir(), "dns.json"), tunnel: "echofog0"}
}

func dnsConfig() tun.DNSConfig {
	return tun.DNSConfig{Tunnel: "echofog0", Resolver: netip.MustParseAddr("198.18.0.2"), DisableSmartNameResolution: true}
}

func hasCall(e *fakeExec, name string, args ...string) bool {
	want := name + " " + strings.Join(args, " ")
	for _, c := range e.all() {
		if c.name+" "+strings.Join(c.args, " ") == want {
			return true
		}
	}
	return false
}

func TestProtectPinsDNSThroughTheTunnel(t *testing.T) {
	e := newFakeExec(resolveReply)
	g := newTestDNS(t, e)
	if err := g.Protect(context.Background(), dnsConfig()); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{"dns", "echofog0", "198.18.0.2"},
		{"domain", "echofog0", "~."},
		{"default-route", "echofog0", "yes"},
		{"default-route", "eth0", "no"},
		{"flush-caches"},
	} {
		if !hasCall(e, "resolvectl", want...) {
			t.Errorf("resolvectl %v was not called: %v", want, e.all())
		}
	}
	if hasCall(e, "resolvectl", "default-route", "docker0", "no") {
		t.Error("a link that was not a default route was touched")
	}
	snap, err := readState(g.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("the snapshot has %d links, want eth0 and docker0: %+v", len(snap), snap)
	}
	for _, l := range snap {
		if l.Name == "echofog0" {
			t.Fatal("the tunnel itself is in the snapshot: Restore would point DNS at a dead tunnel")
		}
	}
	if snap[0].Name != "eth0" || !snap[0].DefaultRoute || len(snap[0].DNS) != 1 || snap[0].DNS[0] != "192.168.1.1" ||
		len(snap[0].Domains) != 1 || snap[0].Domains[0] != "~lan" {
		t.Fatalf("eth0 snapshot %+v", snap[0])
	}
}

func TestProtectKeepsTheFirstSnapshot(t *testing.T) {
	e := newFakeExec(resolveReply)
	g := newTestDNS(t, e)
	if err := g.Protect(context.Background(), dnsConfig()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(g.state)
	if err != nil {
		t.Fatal(err)
	}
	// The resolver of the user changes while the mode runs: the snapshot stays the one of the first
	// Protect, or Restore would keep the protection's own values.
	e.reply = func(c cmdCall) (string, error) {
		if len(c.args) == 1 && c.args[0] == "dns" {
			return "Global: 1.1.1.1\nLink 2 (eth0): 1.1.1.1\n", nil
		}
		return resolveReply(c)
	}
	if err := g.Protect(context.Background(), dnsConfig()); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(g.state)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("the snapshot was overwritten:\n%s\n%s", first, second)
	}
}

func TestRestorePutsTheValuesBack(t *testing.T) {
	e := newFakeExec(resolveReply)
	g := newTestDNS(t, e)
	if err := g.Protect(context.Background(), dnsConfig()); err != nil {
		t.Fatal(err)
	}
	if err := g.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{"dns", "eth0", "192.168.1.1"},
		{"domain", "eth0", "~lan"},
		{"default-route", "eth0", "yes"},
		{"dns", "docker0"},
		{"domain", "docker0"},
		{"default-route", "docker0", "no"},
		{"revert", "echofog0"},
		{"flush-caches"},
	} {
		if !hasCall(e, "resolvectl", want...) {
			t.Errorf("resolvectl %v was not called: %v", want, e.all())
		}
	}
	if _, err := os.Stat(g.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the snapshot is still there: %v", err)
	}
}

func TestRestoreWithoutASnapshotIsFine(t *testing.T) {
	e := newFakeExec(func(c cmdCall) (string, error) {
		if c.name == "resolvectl" && len(c.args) == 2 && c.args[0] == "revert" {
			return `Failed to resolve interface "echofog0": No such device`, errors.New("exit status 1")
		}
		return "", nil
	})
	g := newTestDNS(t, e)
	if err := g.Restore(context.Background()); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if !hasCall(e, "resolvectl", "revert", "echofog0") || !hasCall(e, "resolvectl", "flush-caches") {
		t.Fatalf("the tunnel was not reverted: %v", e.all())
	}
}

func TestProtectRefusesAForeignResolver(t *testing.T) {
	e := newFakeExec(resolveReply)
	g := newTestDNS(t, e)
	cfg := dnsConfig()
	cfg.Resolver = netip.MustParseAddr("8.8.8.8")
	if err := g.Protect(context.Background(), cfg); !errors.Is(err, tun.ErrBadDNS) {
		t.Fatalf("Protect = %v, want ErrBadDNS", err)
	}
	if len(e.all()) != 0 {
		t.Fatalf("resolvectl was touched: %v", e.all())
	}
}

func TestParseStatus(t *testing.T) {
	got := parseStatus(statusText)
	if len(got) != 3 {
		t.Fatalf("%d links: %+v", len(got), got)
	}
	want := map[string]bool{"eth0": true, "docker0": false, "echofog0": true}
	for _, l := range got {
		if l.DefaultRoute != want[l.Name] {
			t.Errorf("%s default route %v, want %v", l.Name, l.DefaultRoute, want[l.Name])
		}
	}
	// The older format names the flag in Protocols only.
	old := "Link 4 (wlan0)\n         Protocols: -DefaultRoute +LLMNR\n"
	if got := parseStatus(old); len(got) != 1 || got[0].DefaultRoute {
		t.Fatalf("old format: %+v", got)
	}
}

func TestParseLinkValues(t *testing.T) {
	got := parseLinkValues(dnsText)
	if len(got["eth0"]) != 1 || got["eth0"][0] != "192.168.1.1" {
		t.Fatalf("eth0: %+v", got)
	}
	if servers, ok := got["docker0"]; !ok || len(servers) != 0 {
		t.Fatalf("docker0: %+v", got["docker0"])
	}
	if got["echofog0"][0] != "198.18.0.2" {
		t.Fatalf("echofog0: %+v", got)
	}
}

// ---- the tunnel device ---------------------------------------------------------------------------

type fakeController struct {
	mu       sync.Mutex
	socks    string
	bypass   int
	hold     string
	startErr error
	running  bool
	onExit   func(error)
	starts   int
	stops    int
}

func (c *fakeController) Start(_ context.Context, socks string, bypass int, hold string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.socks, c.bypass, c.hold = socks, bypass, hold
	if c.startErr != nil {
		return c.startErr
	}
	c.running = true
	return nil
}
func (c *fakeController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops++
	c.running = false
}
func (c *fakeController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}
func (c *fakeController) SetOnExit(f func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onExit = f
}
func (c *fakeController) exit(err error) {
	c.mu.Lock()
	c.running = false
	f := c.onExit
	c.mu.Unlock()
	if f != nil {
		f(err)
	}
}

func newTestTunnel(ctl *fakeController, iface bool) *linuxTunnel {
	return &linuxTunnel{ctl: ctl, ifaceExists: func(string) bool { return iface }, events: make(chan tun.TunEvent, 4)}
}

func tunConfig() tun.TunConfig {
	return tun.TunConfig{Name: "EchoFog", Socks: "127.0.0.1:1080", BypassUDPPort: 4433, Resolver: netip.MustParseAddr("198.18.0.2")}
}

func TestTunnelUpStartsTheController(t *testing.T) {
	ctl := &fakeController{}
	tr := newTestTunnel(ctl, true)
	if err := tr.Up(context.Background(), tunConfig()); err != nil {
		t.Fatal(err)
	}
	if ctl.starts != 1 || ctl.socks != "127.0.0.1:1080" || ctl.bypass != 4433 || ctl.hold != "" {
		t.Fatalf("start: %+v", ctl)
	}
	if !tr.Alive(context.Background()) {
		t.Fatal("Alive says no after Up")
	}
}

func TestTunnelRefusesAProxyOffTheLoopback(t *testing.T) {
	ctl := &fakeController{}
	tr := newTestTunnel(ctl, true)
	cfg := tunConfig()
	cfg.Socks = "192.168.1.5:1080"
	if err := tr.Up(context.Background(), cfg); err == nil {
		t.Fatal("Up accepted a proxy outside the loopback")
	}
	if ctl.starts != 0 {
		t.Fatalf("the controller was started: %+v", ctl)
	}
}

func TestTunnelWithoutTheInterfaceComesDown(t *testing.T) {
	ctl := &fakeController{}
	tr := newTestTunnel(ctl, false)
	if err := tr.Up(context.Background(), tunConfig()); err == nil {
		t.Fatal("Up succeeded without the device")
	}
	if ctl.stops != 1 {
		t.Fatalf("the controller was not stopped: %+v", ctl)
	}
	if tr.Alive(context.Background()) {
		t.Fatal("Alive says yes without the device")
	}
}

func TestTunnelLostIsAnEventOnlyWhenItRuns(t *testing.T) {
	ctl := &fakeController{}
	tr := newTestTunnel(ctl, true)
	if err := tr.Up(context.Background(), tunConfig()); err != nil {
		t.Fatal(err)
	}
	ctl.exit(errors.New("the helper exited"))
	select {
	case ev := <-tr.Events():
		if ev.Err == nil || !strings.Contains(ev.Err.Error(), "helper") {
			t.Fatalf("event %+v", ev)
		}
	default:
		t.Fatal("no event after the helper went away")
	}
	if tr.Alive(context.Background()) {
		t.Fatal("Alive says yes after the loss")
	}
	if err := tr.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctl.exit(errors.New("late"))
	select {
	case ev := <-tr.Events():
		t.Fatalf("an event after Down: %+v", ev)
	default:
	}
}

// ---- the driver under the state machine ---------------------------------------------------------

func TestSwitchOverTheLinuxFirewall(t *testing.T) {
	e := newFakeExec(nil)
	fw := newTestFirewall(t, e, root)
	tn := tuntest.NewTunnel()
	d := &tuntest.DNS{}
	j := &tuntest.Journal{}
	tn.J, d.J = j, j
	sw := tun.New(tun.Config{Firewall: fw, Tunnel: tn, DNS: d, Plan: testPlan(),
		Tun: tun.TunConfig{Name: "EchoFog", Socks: "127.0.0.1:1080", Resolver: netip.MustParseAddr("198.18.0.2")}})
	defer sw.Close()
	if err := sw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(j.Entries(), ","); got != "tun.up,dns.protect" {
		t.Fatalf("order %q", got)
	}
	if len(e.all()) == 0 {
		t.Fatal("nft was never called")
	}
	if err := sw.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(j.Entries(), ","); got != "tun.up,dns.protect,tun.down,dns.restore" {
		t.Fatalf("order %q", got)
	}
}

// The snapshot of the state file is JSON that a later process reads.
func TestStateFileIsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns.json")
	if err := writeState(path, []linkState{{Name: "eth0", DNS: []string{"192.168.1.1"}, DefaultRoute: true}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st dnsState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if st.Links[0].Name != "eth0" || !st.Links[0].DefaultRoute {
		t.Fatalf("%+v", st)
	}
}

// The identifiers in the guide a person uses to take the protection off are the ones in the code.
func TestRemovalGuideNamesTheSameIdentifiers(t *testing.T) {
	for _, rel := range []string{
		`../../../../../docs/kill-switch-linux.md`,
		`../../../../../docs/kill-switch-linux.ru.md`,
	} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		text := string(raw)
		for _, want := range []string{TableName, tunnelName, "efv2 killswitch remove", dnsStatePath()} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not contain %q", rel, want)
			}
		}
	}
}
