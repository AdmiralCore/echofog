//go:build linux && !android

// Package platform ties the drivers of the kill switch to the platform: on Linux the nftables table
// of the kill switch (a fixed table of the kernel), the tunnel device of v1 tunmode and the DNS
// guard over resolvectl, driven through an elevated helper (ks). Elsewhere the drivers are of
// another file.
//
// The firewall is nftables in the inet family: one table, two chains (output and input), policy
// drop. The script is applied with `nft -f` as one transaction, so a replacement never leaves the
// computer open for a moment and a killed process leaves the table in the kernel.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/tunmode"
	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/ks"
)

// TableName is the nftables table of the kill switch. It is fixed and written in the operator
// guide: filters of an older run are found and removed by exactly this name.
const TableName = "echofog_ks"

// tunnelName is the device of v1 tunmode. The firewall names it and the tunnel creates it, so the
// two must agree; a configuration with another name still gets this device, as on Windows.
const tunnelName = tunmode.DeviceName

const (
	runDir = "/run/echofog"
	// helperStartTimeout covers the prompt for root rights (polkit).
	helperStartTimeout = 2 * time.Minute
	dnsStateVersion    = 1
)

// HelperFlag as os.Args[1] makes the executable run RunHelper.
const HelperFlag = ks.HelperFlag

func markerPath() string   { return filepath.Join(runDir, "killswitch.on") }
func dnsStatePath() string { return filepath.Join(runDir, "killswitch-dns.json") }

// Supported reports whether the kill switch has a driver here.
func Supported() bool { return true }

// Drivers is the application side: one Client that is the firewall, the tunnel and the DNS guard.
// exe is the executable the helper is started from (normally os.Executable()). Nothing is started
// until the first operation needs rights.
func Drivers(exe string) *ks.Client {
	return ks.NewClient(ks.ClientConfig{
		Connect: ks.Connector(launchElevated, exe, nil, helperStartTimeout),
		// A restart of the application asks before there is any helper: the marker the elevated
		// helper keeps and, when this process may read the kernel, the table itself.
		Installed: newFirewall().Installed,
	})
}

// RunHelper is the main of the elevated helper; args follow HelperFlag. It returns the exit code.
func RunHelper(args []string) int {
	return ks.ServeHelper(args, func() (ks.Backend, func(), error) {
		if os.Geteuid() != 0 {
			return ks.Backend{}, nil, errNoPrivilege("the helper")
		}
		if err := requiredTools(); err != nil {
			return ks.Backend{}, nil, err
		}
		return ks.Backend{Firewall: newFirewall(), Tunnel: newTunnel(), DNS: newDNS()}, nil, nil
	})
}

// FiltersStand reports whether the filters of the kill switch stand: the marker the elevated helper
// keeps and, when this process may read the kernel, the table. It needs no rights and starts
// nothing, so it can be asked at start-up and by the interface.
func FiltersStand(ctx context.Context) bool {
	ok, _ := newFirewall().Installed(ctx)
	return ok
}

// Release removes the filters and restores the DNS settings through the elevated helper (one prompt
// for rights). It is the "Disable protection" of the interface when no mode is running.
func Release(ctx context.Context, exe string) error {
	c := Drivers(exe)
	defer c.Close()
	return errors.Join(c.Restore(ctx), c.Remove(ctx))
}

// Report is what RemoveAll did.
type Report struct {
	Filters int // rules of the kill switch removed
}

// RemoveAll is the manual removal (efv2 killswitch remove): the table of the kill switch and the
// DNS settings as they were. It needs root, and on a computer that has none of it, it removes
// nothing and succeeds.
func RemoveAll(ctx context.Context) (Report, error) {
	if os.Geteuid() != 0 {
		return Report{}, errNoPrivilege("efv2 killswitch remove")
	}
	n, err := newFirewall().removeCounted(ctx)
	derr := newDNS().Restore(ctx)
	return Report{Filters: n}, errors.Join(err, derr)
}

func requiredTools() error {
	var missing []string
	for _, name := range []string{"nft", "resolvectl", "ip"} {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the kill switch needs %s", strings.Join(missing, ", "))
	}
	return nil
}

func errNoPrivilege(what string) error {
	return fmt.Errorf("%w: %s needs root (run it with sudo or through the helper)", tun.ErrNoPrivilege, what)
}

// launchElevated starts the helper as root: directly when this process is root already, through
// polkit otherwise.
func launchElevated(exe string, args []string) error {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(exe, args...)
	} else {
		if _, err := exec.LookPath("pkexec"); err != nil {
			return fmt.Errorf("%w: pkexec (polkit) is needed to ask for root rights", tun.ErrNoPrivilege)
		}
		cmd = exec.Command("pkexec", append([]string{exe}, args...)...)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the helper: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reap; the lifetime is tracked by the control connection
	return nil
}

// ---- the command line ---------------------------------------------------------------------------

// runner runs one command; stdin is written to it (nft -f - takes the script). Tests replace it.
type runner func(ctx context.Context, stdin, name string, args ...string) ([]byte, error)

func runCommand(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func lower(out []byte) string { return strings.ToLower(string(out)) }

func refused(out []byte) bool {
	s := lower(out)
	return strings.Contains(s, "operation not permitted") || strings.Contains(s, "permission denied")
}

func tableMissing(out []byte) bool {
	s := lower(out)
	return strings.Contains(s, "no such file or directory") || strings.Contains(s, "does not exist")
}

func linkGone(out []byte) bool {
	s := lower(out)
	return strings.Contains(s, "no such device") || strings.Contains(s, "failed to resolve interface")
}

func commandError(what string, out []byte, err error) error {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	if refused(out) {
		return fmt.Errorf("%w: %s: %s", tun.ErrNoPrivilege, what, msg)
	}
	return fmt.Errorf("%s: %v: %s", what, err, msg)
}

// ---- the nftables table -------------------------------------------------------------------------

type firewall struct {
	run    runner
	euid   func() int
	marker string
}

func newFirewall() *firewall {
	return &firewall{run: runCommand, euid: os.Geteuid, marker: markerPath()}
}

// Install replaces the whole rule set in one transaction: the new table, both chains and every rule
// go in together, so the rules are never absent. It needs root and refuses a set that does not
// close the computer.
func (f *firewall) Install(ctx context.Context, rules []tun.Rule) error {
	if err := tun.CheckRules(rules); err != nil {
		return err
	}
	if f.euid() != 0 {
		return errNoPrivilege("the kill switch")
	}
	if out, err := f.run(ctx, renderRules(rules, f.exists(ctx)), "nft", "-f", "-"); err != nil {
		return commandError("install the kill switch table", out, err)
	}
	f.setMarker(true)
	return nil
}

// exists asks the kernel whether the table of the kill switch is there (this process is root).
func (f *firewall) exists(ctx context.Context) bool {
	_, err := f.run(ctx, "", "nft", "list", "table", "inet", TableName)
	return err == nil
}

// Remove deletes the table; a table that is not there is not an error.
func (f *firewall) Remove(ctx context.Context) error {
	_, err := f.removeCounted(ctx)
	return err
}

// removeCounted deletes the table and reports how many rules it had.
func (f *firewall) removeCounted(ctx context.Context) (int, error) {
	if f.euid() != 0 {
		return 0, errNoPrivilege("the kill switch")
	}
	n := 0
	if out, err := f.run(ctx, "", "nft", "-j", "list", "table", "inet", TableName); err == nil {
		n = countNftRules(out)
	}
	if out, err := f.run(ctx, "", "nft", "delete", "table", "inet", TableName); err != nil && !tableMissing(out) {
		return n, commandError("remove the kill switch table", out, err)
	}
	f.setMarker(false)
	return n, nil
}

// Installed answers from the system: the marker of an earlier elevated run, or the kernel itself
// when the caller may read it. It errs on the side of "yes".
func (f *firewall) Installed(ctx context.Context) (bool, error) {
	if f.marker != "" {
		if _, err := os.Stat(f.marker); err == nil {
			return true, nil
		}
	}
	if f.euid() != 0 {
		return false, nil // cannot look, and no marker: nothing is known to stand
	}
	return f.exists(ctx), nil
}

func (f *firewall) setMarker(on bool) {
	if f.marker == "" {
		return
	}
	if on {
		_ = os.MkdirAll(filepath.Dir(f.marker), 0o755)
		_ = os.WriteFile(f.marker, []byte("EchoFog kill switch filters are installed. Remove: sudo efv2 killswitch remove (or nft delete table inet "+TableName+")\n"), 0o644)
		return
	}
	_ = os.Remove(f.marker)
}

func countNftRules(raw []byte) int {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0
	}
	n := 0
	for _, obj := range doc.Nftables {
		if _, ok := obj["rule"]; ok {
			n++
		}
	}
	return n
}

// orderRules is the order of nft: the first matching rule decides, so the highest weight goes
// first; on a tie Block wins, as in tun.Evaluate.
func orderRules(rules []tun.Rule) []tun.Rule {
	out := append([]tun.Rule(nil), rules...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Action == tun.Block && out[j].Action != tun.Block
	})
	return out
}

// renderRules turns the rules into one nft script. replace says the table is already there and its
// chains must be recreated with the current definitions.
func renderRules(rules []tun.Rule, replace bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", TableName)
	if replace {
		fmt.Fprintf(&b, "flush table inet %s\n", TableName)
		fmt.Fprintf(&b, "delete chain inet %s out\n", TableName)
		fmt.Fprintf(&b, "delete chain inet %s in\n", TableName)
	}
	fmt.Fprintf(&b, "add chain inet %s out { type filter hook output priority 0; policy drop; }\n", TableName)
	fmt.Fprintf(&b, "add chain inet %s in { type filter hook input priority 0; policy drop; }\n", TableName)
	ordered := orderRules(rules)
	for _, dir := range []tun.Dir{tun.Out, tun.In} {
		chain := "out"
		if dir == tun.In {
			chain = "in"
		}
		for _, r := range ordered {
			if r.Dir != dir {
				continue
			}
			action := "drop"
			if r.Action == tun.Permit {
				action = "accept"
			}
			for _, expr := range matchExprs(r) {
				parts := append(clone(expr), action)
				fmt.Fprintf(&b, "add rule inet %s %s %s\n", TableName, chain, strings.Join(parts, " "))
			}
		}
	}
	return b.String()
}

func ifaceKeyword(d tun.Dir) string {
	if d == tun.In {
		return "iifname"
	}
	return "oifname"
}

func clone(in []string) []string { return append([]string(nil), in...) }

// matchExprs is one rule as the packet fields of nft. "Physical" is every interface that is not
// the tunnel and not loopback; the remote side is the destination of what leaves and the source of
// what arrives; a rule that names a port without a protocol becomes one rule for TCP and one for
// UDP.
func matchExprs(r tun.Rule) [][]string {
	kw := ifaceKeyword(r.Dir)
	var base []string
	switch r.Iface {
	case tun.IfaceLoopback:
		base = append(base, kw+` "lo"`)
	case tun.IfaceTunnel:
		base = append(base, kw+` "`+tunnelName+`"`)
	case tun.IfacePhysical:
		base = append(base, kw+` != "`+tunnelName+`"`, kw+` != "lo"`)
	}
	if r.Remote.IsValid() {
		field := "daddr"
		if r.Dir == tun.In {
			field = "saddr"
		}
		family := "ip"
		if r.V6 {
			family = "ip6"
		}
		base = append(base, family+" "+field+" "+r.Remote.String())
	}
	switch r.Proto {
	case tun.ProtoAny:
		if r.RemotePort == 0 && r.LocalPort == 0 {
			return [][]string{base}
		}
		return [][]string{
			append(clone(base), portExprs(r, "tcp")...),
			append(clone(base), portExprs(r, "udp")...),
		}
	case tun.ProtoTCP:
		return [][]string{append(base, portExprs(r, "tcp")...)}
	case tun.ProtoUDP:
		return [][]string{append(base, portExprs(r, "udp")...)}
	case tun.ProtoICMP:
		proto := "icmp"
		if r.V6 {
			proto = "icmpv6"
		}
		if r.ICMPHi != 0 {
			return [][]string{append(base, fmt.Sprintf("%s type %d-%d", proto, r.ICMPLo, r.ICMPHi))}
		}
		return [][]string{append(base, proto)}
	}
	return nil
}

func portExprs(r tun.Rule, proto string) []string {
	sport, dport := r.LocalPort, r.RemotePort
	if r.Dir == tun.In {
		sport, dport = r.RemotePort, r.LocalPort
	}
	var out []string
	if sport != 0 {
		out = append(out, fmt.Sprintf("%s sport %d", proto, sport))
	}
	if dport != 0 {
		out = append(out, fmt.Sprintf("%s dport %d", proto, dport))
	}
	return out
}

// ---- DNS over resolvectl -------------------------------------------------------------------------

// linkState is one link of systemd-resolved as it was before Protect.
type linkState struct {
	Name         string   `json:"name"`
	DNS          []string `json:"dns,omitempty"`
	Domains      []string `json:"domains,omitempty"`
	DefaultRoute bool     `json:"default_route"`
}

// dnsState is what Protect saves for Restore, also across a crash of the process. It lives in /run,
// so a reboot clears it together with the table and the settings of the resolver.
type dnsState struct {
	Version int         `json:"version"`
	Links   []linkState `json:"links"`
}

func readState(path string) ([]linkState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st dnsState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("read the DNS snapshot: %w", err)
	}
	if st.Version != dnsStateVersion {
		return nil, fmt.Errorf("read the DNS snapshot: version %d", st.Version)
	}
	return st.Links, nil
}

func writeState(path string, links []linkState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(dnsState{Version: dnsStateVersion, Links: links})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dnsGuard pins name resolution to the tunnel: the tunnel link becomes the default route and every
// other link that was one stops being it. The settings of the other links are saved once, so
// Restore gives back what the user had even after a crash and a new process.
type dnsGuard struct {
	run    runner
	euid   func() int
	state  string
	tunnel string
}

func newDNS() *dnsGuard {
	return &dnsGuard{run: runCommand, euid: os.Geteuid, state: dnsStatePath(), tunnel: tunnelName}
}

func (g *dnsGuard) Protect(ctx context.Context, cfg tun.DNSConfig) error {
	if err := tun.CheckDNS(cfg); err != nil {
		return err
	}
	if g.euid() != 0 {
		return errNoPrivilege("pin DNS")
	}
	if _, err := os.Stat(g.state); errors.Is(err, os.ErrNotExist) {
		links, serr := g.snapshot(ctx)
		if serr != nil {
			return serr
		}
		if werr := writeState(g.state, links); werr != nil {
			return fmt.Errorf("save the DNS settings: %w", werr)
		}
	} else if err != nil {
		return fmt.Errorf("read the DNS snapshot: %w", err)
	}
	links, err := readState(g.state)
	if err != nil {
		return err
	}
	if err := g.must(ctx, "dns", g.tunnel, cfg.Resolver.String()); err != nil {
		return err
	}
	if err := g.must(ctx, "domain", g.tunnel, "~."); err != nil {
		return err
	}
	if err := g.must(ctx, "default-route", g.tunnel, "yes"); err != nil {
		return err
	}
	for _, l := range links {
		if l.DefaultRoute {
			if err := g.set(ctx, "default-route", l.Name, "no"); err != nil {
				return err
			}
		}
	}
	g.flush(ctx)
	return nil
}

func (g *dnsGuard) Restore(ctx context.Context) error {
	if g.euid() != 0 {
		return errNoPrivilege("restore DNS")
	}
	var errs []error
	links, serr := readState(g.state)
	if serr == nil {
		for _, l := range links {
			if err := g.set(ctx, "dns", l.Name, l.DNS...); err != nil {
				errs = append(errs, err)
			}
			if err := g.set(ctx, "domain", l.Name, l.Domains...); err != nil {
				errs = append(errs, err)
			}
			value := "no"
			if l.DefaultRoute {
				value = "yes"
			}
			if err := g.set(ctx, "default-route", l.Name, value); err != nil {
				errs = append(errs, err)
			}
		}
	} else if !errors.Is(serr, os.ErrNotExist) {
		errs = append(errs, serr)
	}
	// The link of the tunnel goes back to the settings of the system (DHCP).
	if out, err := g.run(ctx, "", "resolvectl", "revert", g.tunnel); err != nil && !linkGone(out) {
		errs = append(errs, commandError("resolvectl revert "+g.tunnel, out, err))
	}
	g.flush(ctx)
	if len(errs) == 0 && serr == nil {
		_ = os.Remove(g.state)
	}
	return errors.Join(errs...)
}

// must sets a value on a link that has to be there (the tunnel).
func (g *dnsGuard) must(ctx context.Context, what, link string, values ...string) error {
	args := append([]string{what, link}, values...)
	out, err := g.run(ctx, "", "resolvectl", args...)
	if err != nil {
		return commandError("resolvectl "+what+" "+link, out, err)
	}
	return nil
}

// set restores a value of a link that may have gone (a Wi-Fi adapter unplugged) without failing the
// whole restore.
func (g *dnsGuard) set(ctx context.Context, what, link string, values ...string) error {
	args := append([]string{what, link}, values...)
	out, err := g.run(ctx, "", "resolvectl", args...)
	if err != nil && linkGone(out) {
		return nil
	}
	if err != nil {
		return commandError("resolvectl "+what+" "+link, out, err)
	}
	return nil
}

func (g *dnsGuard) flush(ctx context.Context) {
	_, _ = g.run(ctx, "", "resolvectl", "flush-caches")
}

func (g *dnsGuard) snapshot(ctx context.Context) ([]linkState, error) {
	statusOut, err := g.run(ctx, "", "resolvectl", "status")
	if err != nil {
		return nil, commandError("read the resolver status (systemd-resolved is needed)", statusOut, err)
	}
	links := parseStatus(string(statusOut))
	dnsOut, err := g.run(ctx, "", "resolvectl", "dns")
	if err != nil {
		return nil, commandError("read the resolver servers", dnsOut, err)
	}
	domainOut, err := g.run(ctx, "", "resolvectl", "domain")
	if err != nil {
		return nil, commandError("read the resolver domains", domainOut, err)
	}
	servers := parseLinkValues(string(dnsOut))
	domains := parseLinkValues(string(domainOut))
	out := make([]linkState, 0, len(links))
	for _, l := range links {
		if l.Name == g.tunnel {
			continue // its settings belong to the tunnel, not to the user
		}
		l.DNS = servers[l.Name]
		l.Domains = domains[l.Name]
		out = append(out, l)
	}
	return out, nil
}

// parseStatus reads the links and their DefaultRoute flag from `resolvectl status`. Both the older
// "Default Route: yes" line and the newer "+DefaultRoute" in Protocols are understood.
func parseStatus(text string) []linkState {
	var links []linkState
	current := -1
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if name, ok := linkName(line); ok {
			links = append(links, linkState{Name: name})
			current = len(links) - 1
			continue
		}
		if current < 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "Default Route:"):
			links[current].DefaultRoute = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "Default Route:")), "yes")
		case strings.HasPrefix(line, "Protocols:"):
			switch {
			case strings.Contains(line, "+DefaultRoute"):
				links[current].DefaultRoute = true
			case strings.Contains(line, "-DefaultRoute"):
				links[current].DefaultRoute = false
			}
		}
	}
	return links
}

// parseLinkValues reads `resolvectl dns` and `resolvectl domain` output into link name -> values.
func parseLinkValues(text string) map[string][]string {
	out := map[string][]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "Link ") {
			continue
		}
		open, end := strings.IndexByte(line, '('), strings.IndexByte(line, ')')
		if open < 0 || end <= open {
			continue
		}
		name := line[open+1 : end]
		rest := strings.TrimSpace(strings.TrimPrefix(line[end+1:], ":"))
		out[name] = strings.Fields(rest)
	}
	return out
}

func linkName(line string) (string, bool) {
	if !strings.HasPrefix(line, "Link ") {
		return "", false
	}
	open, close := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
	if open < 0 || close <= open+1 {
		return "", false
	}
	return line[open+1 : close], true
}

// ---- the tunnel device --------------------------------------------------------------------------

// tunnelController is the part of *tunmode.Controller the driver uses; tests replace it.
type tunnelController interface {
	Start(ctx context.Context, socksAddr string, bypassUDPPort int, holdFile string) error
	Stop()
	Running() bool
	SetOnExit(func(error))
}

// linuxTunnel is the tun.Tunnel of Linux: the TUN device and the tun2socks helper of v1, started
// from the elevated process of the kill switch so that no second prompt for rights is needed.
type linuxTunnel struct {
	ctl         tunnelController
	ifaceExists func(string) bool
	events      chan tun.TunEvent

	mu       sync.Mutex
	up       bool
	stopping bool
}

func newTunnel() *linuxTunnel {
	return &linuxTunnel{ctl: &tunmode.Controller{}, ifaceExists: interfaceUp, events: make(chan tun.TunEvent, 4)}
}

func interfaceUp(name string) bool {
	ifc, err := net.InterfaceByName(name)
	return err == nil && ifc.Flags&net.FlagUp != 0
}

func loopbackProxy(addr string) bool {
	ap, err := netip.ParseAddrPort(addr)
	return err == nil && ap.Addr().IsLoopback() && ap.Port() != 0
}

func (t *linuxTunnel) Up(ctx context.Context, cfg tun.TunConfig) error {
	if !loopbackProxy(cfg.Socks) {
		return errors.New("the proxy of the node must be on the loopback")
	}
	t.mu.Lock()
	t.stopping = false
	t.mu.Unlock()
	t.ctl.SetOnExit(t.exited)
	// No hold file: the firewall is the kill switch. Two of them would fight over the same traffic.
	if err := t.ctl.Start(ctx, cfg.Socks, cfg.BypassUDPPort, ""); err != nil {
		return err
	}
	if !t.ifaceExists(tunnelName) {
		t.ctl.Stop()
		return fmt.Errorf("the tunnel device %s did not appear", tunnelName)
	}
	t.mu.Lock()
	t.up = true
	t.mu.Unlock()
	return nil
}

// exited is the TUN helper of v1 going away without being told to.
func (t *linuxTunnel) exited(err error) {
	t.mu.Lock()
	t.up = false
	stopping := t.stopping
	t.mu.Unlock()
	if stopping {
		return
	}
	if err == nil {
		err = errors.New("the tunnel helper exited")
	}
	select {
	case t.events <- tun.TunEvent{Err: err}:
	default:
	}
}

func (t *linuxTunnel) Down(context.Context) error {
	t.mu.Lock()
	t.stopping = true
	t.up = false
	t.mu.Unlock()
	t.ctl.Stop()
	return nil
}

func (t *linuxTunnel) Events() <-chan tun.TunEvent { return t.events }

// Alive is true while the helper runs and the device is there and up.
func (t *linuxTunnel) Alive(context.Context) bool {
	t.mu.Lock()
	up := t.up
	t.mu.Unlock()
	return up && t.ctl.Running() && t.ifaceExists(tunnelName)
}

var (
	_ tun.Firewall = (*firewall)(nil)
	_ tun.Tunnel   = (*linuxTunnel)(nil)
	_ tun.DNSGuard = (*dnsGuard)(nil)
)
