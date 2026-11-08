//go:build windows

package winwfp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/tailscale/wf"
	"golang.org/x/sys/windows"
)

// The tests here run on any Windows machine without rights: they never open the real engine.

const testLUID = uint64(0x0006_0000_0000_0000)

func guidSeq() func() windows.GUID {
	var n uint32
	return func() windows.GUID { n++; return windows.GUID{Data1: n} }
}

func plan() tun.Plan {
	return tun.Plan{
		Endpoints: []tun.Endpoint{
			{Addr: netip.MustParseAddrPort("203.0.113.10:443"), Proto: tun.ProtoUDP},
			{Addr: netip.MustParseAddrPort("[2001:db8:1::5]:8443"), Proto: tun.ProtoTCP},
		},
		TunnelPool: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")},
		AllowDHCP:  true,
	}
}

func build(t testing.TB, p tun.Plan) []tun.Rule {
	t.Helper()
	r, err := tun.BuildRules(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// noTunnel is how many filters a rule set gives while there is no adapter.
func noTunnel(t testing.TB, rules []tun.Rule) int {
	t.Helper()
	out, err := Compile(rules, 0, guidSeq())
	if err != nil {
		t.Fatal(err)
	}
	return len(out)
}

func find(rs []*wf.Rule, name string) *wf.Rule {
	for _, r := range rs {
		if r.Name == "EchoFog: "+name {
			return r
		}
	}
	return nil
}

func cond(r *wf.Rule, f wf.FieldID, op wf.MatchType) *wf.Match {
	for _, m := range r.Conditions {
		if m.Field == f && m.Op == op {
			return m
		}
	}
	return nil
}

func TestIdentifiersAreFixed(t *testing.T) {
	if got := windows.GUID(ProviderKey).String(); !strings.EqualFold(got, ProviderKeyString) {
		t.Errorf("provider %s, documented %s", got, ProviderKeyString)
	}
	if got := windows.GUID(SublayerKey).String(); !strings.EqualFold(got, SublayerKeyString) {
		t.Errorf("sublayer %s, documented %s", got, SublayerKeyString)
	}
	if ProviderKeyString != "{d2c88793-6bad-4fdf-881b-f515c796bdd7}" || SublayerKeyString != "{af629ec5-2cd7-4717-af0e-b4e3e4672d24}" {
		t.Error("the identifiers changed: filters of an older version would be orphaned")
	}
}

func TestCompileEveryRule(t *testing.T) {
	rules := build(t, plan())
	out, err := Compile(rules, testLUID, guidSeq())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no filters")
	}
	ids := map[wf.RuleID]bool{}
	for _, r := range out {
		if !r.Persistent {
			t.Errorf("%s is not persistent: it would die with the process", r.Name)
		}
		if r.Provider != ProviderKey || r.Sublayer != SublayerKey {
			t.Errorf("%s is not under the provider and sublayer of the kill switch", r.Name)
		}
		if r.Action != wf.ActionBlock && r.Action != wf.ActionPermit {
			t.Errorf("%s has action %v", r.Name, r.Action)
		}
		if r.ID.IsZero() || ids[r.ID] {
			t.Errorf("%s has a zero or repeated key", r.Name)
		}
		ids[r.ID] = true
		if r.Weight == 0 {
			t.Errorf("%s has no explicit weight", r.Name)
		}
	}
	if len(out) != len(rules) {
		t.Errorf("%d filters for %d rules", len(out), len(rules))
	}
}

func TestCompileLayersAndConditions(t *testing.T) {
	out, err := Compile(build(t, plan()), testLUID, guidSeq())
	if err != nil {
		t.Fatal(err)
	}
	if r := find(out, "block-all-v4-out"); r == nil || r.Layer != wf.LayerALEAuthConnectV4 || r.Action != wf.ActionBlock || len(r.Conditions) != 0 {
		t.Errorf("block-all-v4-out: %+v", r)
	}
	if r := find(out, "block-all-v6-in"); r == nil || r.Layer != wf.LayerALEAuthRecvAcceptV6 || r.Action != wf.ActionBlock {
		t.Errorf("block-all-v6-in: %+v", r)
	}
	if r := find(out, "permit-loopback-v4-out"); r == nil || cond(r, wf.FieldFlags, wf.MatchTypeFlagsAllSet) == nil {
		t.Errorf("permit-loopback-v4-out: %+v", r)
	}
	if r := find(out, "permit-tunnel-v6-out"); r == nil || r.Layer != wf.LayerALEAuthConnectV6 {
		t.Errorf("permit-tunnel-v6-out: %+v", r)
	} else if m := cond(r, wf.FieldIPLocalInterface, wf.MatchTypeEqual); m == nil || m.Value != testLUID {
		t.Errorf("the tunnel rule does not name the adapter: %+v", r.Conditions)
	}
	ep := find(out, "permit-endpoint-1-out")
	if ep == nil {
		t.Fatal("no endpoint rule")
	}
	if ep.Layer != wf.LayerALEAuthConnectV4 || ep.Action != wf.ActionPermit {
		t.Errorf("endpoint rule: %+v", ep)
	}
	if m := cond(ep, wf.FieldIPProtocol, wf.MatchTypeEqual); m == nil || m.Value != wf.IPProtoUDP {
		t.Errorf("endpoint protocol condition: %+v", m)
	}
	if m := cond(ep, wf.FieldIPRemotePort, wf.MatchTypeEqual); m == nil || m.Value != uint16(443) {
		t.Errorf("endpoint port condition: %+v", m)
	}
	if m := cond(ep, wf.FieldIPRemoteAddress, wf.MatchTypeEqual); m == nil || m.Value != netip.MustParsePrefix("203.0.113.10/32") {
		t.Errorf("endpoint address condition: %+v", m)
	}
	// "physical" is every interface except the tunnel, and not loopback
	if m := cond(ep, wf.FieldIPLocalInterface, wf.MatchTypeNotEqual); m == nil || m.Value != testLUID {
		t.Errorf("endpoint rule is not limited to the interfaces other than the tunnel: %+v", ep.Conditions)
	}
	if cond(ep, wf.FieldFlags, wf.MatchTypeFlagsNoneSet) == nil {
		t.Errorf("endpoint rule may match loopback: %+v", ep.Conditions)
	}
	if r := find(out, "permit-endpoint-1-in"); r == nil || r.Layer != wf.LayerALEAuthRecvAcceptV4 {
		t.Errorf("inbound endpoint rule: %+v", r)
	}
	if r := find(out, "permit-endpoint-2-out"); r == nil || r.Layer != wf.LayerALEAuthConnectV6 {
		t.Errorf("v6 endpoint rule: %+v", r)
	}
	// DNS ports are blocked above every permit
	dns := find(out, "block-dns-53-v4-out")
	if dns == nil || dns.Action != wf.ActionBlock || dns.Weight <= ep.Weight {
		t.Errorf("dns block %+v must outweigh the endpoint permit %+v", dns, ep)
	}
	// ICMPv6 neighbour discovery: protocol ICMPv6 and the type range (ICMP type is the local port)
	nd := find(out, "permit-nd6-0-out")
	if nd == nil {
		t.Fatal("no ND rule")
	}
	if m := cond(nd, wf.FieldIPProtocol, wf.MatchTypeEqual); m == nil || m.Value != wf.IPProtoICMPV6 {
		t.Errorf("ND protocol: %+v", m)
	}
	if m := cond(nd, wf.FieldIPLocalPort, wf.MatchTypeRange); m == nil || m.Value != (wf.Range{From: uint16(133), To: uint16(136)}) {
		t.Errorf("ND type range: %+v", m)
	}
	if r := find(out, "permit-dhcp4-0-out"); r == nil || cond(r, wf.FieldIPLocalPort, wf.MatchTypeEqual) == nil {
		t.Errorf("dhcp rule: %+v", r)
	}
}

func TestCompileWithoutTheTunnel(t *testing.T) {
	out, err := Compile(build(t, plan()), 0, guidSeq())
	if err != nil {
		t.Fatal(err)
	}
	if find(out, "permit-tunnel-v4-out") != nil || find(out, "permit-tunnel-v6-in") != nil {
		t.Error("a tunnel rule without a tunnel adapter would permit everything that claims to be the tunnel")
	}
	ep := find(out, "permit-endpoint-1-out")
	if ep == nil || cond(ep, wf.FieldIPLocalInterface, wf.MatchTypeNotEqual) != nil {
		t.Errorf("endpoint rule without an adapter: %+v", ep)
	}
	if find(out, "block-all-v4-out") == nil {
		t.Error("the block went missing")
	}
}

// ---- the installation, over a fake engine ------------------------------------------------------

type fakeEngine struct {
	mu        sync.Mutex
	providers []*wf.Provider
	sublayers []*wf.Sublayer
	rules     []*wf.Rule
	log       []string
	failAdd   int // fail the Nth AddRule (1-based), 0: never
	adds      int
	failDel   error
	denied    bool
}

func (e *fakeEngine) Providers() ([]*wf.Provider, error) { return e.providers, nil }
func (e *fakeEngine) AddProvider(p *wf.Provider) error {
	if e.denied {
		return windows.ERROR_ACCESS_DENIED
	}
	e.log = append(e.log, "add-provider")
	e.providers = append(e.providers, p)
	return nil
}
func (e *fakeEngine) DeleteProvider(id wf.ProviderID) error {
	e.log = append(e.log, "del-provider")
	for i, p := range e.providers {
		if p.ID == id {
			e.providers = append(e.providers[:i], e.providers[i+1:]...)
		}
	}
	return nil
}
func (e *fakeEngine) Sublayers(...wf.ProviderID) ([]*wf.Sublayer, error) { return e.sublayers, nil }
func (e *fakeEngine) AddSublayer(s *wf.Sublayer) error {
	e.log = append(e.log, "add-sublayer")
	e.sublayers = append(e.sublayers, s)
	return nil
}
func (e *fakeEngine) DeleteSublayer(id wf.SublayerID) error {
	e.log = append(e.log, "del-sublayer")
	for i, s := range e.sublayers {
		if s.ID == id {
			e.sublayers = append(e.sublayers[:i], e.sublayers[i+1:]...)
		}
	}
	return nil
}
func (e *fakeEngine) Rules() ([]*wf.Rule, error) { return append([]*wf.Rule(nil), e.rules...), nil }
func (e *fakeEngine) AddRule(r *wf.Rule) error {
	e.adds++
	if e.failAdd != 0 && e.adds == e.failAdd {
		return errors.New("filter add failed")
	}
	e.log = append(e.log, "add-rule")
	e.rules = append(e.rules, r)
	return nil
}
func (e *fakeEngine) DeleteRule(id wf.RuleID) error {
	if e.failDel != nil {
		return e.failDel
	}
	e.log = append(e.log, "del-rule")
	for i, r := range e.rules {
		if r.ID == id {
			e.rules = append(e.rules[:i], e.rules[i+1:]...)
			return nil
		}
	}
	return errors.New("no such filter")
}
func (e *fakeEngine) Close() error { return nil }

func newFW(e *fakeEngine) *Firewall {
	return &Firewall{open: func() (engine, error) { return e, nil }, newID: guidSeq()}
}

func ours(e *fakeEngine) int {
	n := 0
	for _, r := range e.rules {
		if r.Provider == ProviderKey {
			n++
		}
	}
	return n
}

func TestInstallCreatesProviderSublayerAndFilters(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	rules := build(t, plan())
	if err := f.Install(context.Background(), rules); err != nil {
		t.Fatal(err)
	}
	if len(e.providers) != 1 || e.providers[0].ID != ProviderKey || !e.providers[0].Persistent {
		t.Fatalf("providers %+v", e.providers)
	}
	if len(e.sublayers) != 1 || e.sublayers[0].ID != SublayerKey || !e.sublayers[0].Persistent || e.sublayers[0].Provider != ProviderKey ||
		e.sublayers[0].Weight != SublayerWeight {
		t.Fatalf("sublayers %+v", e.sublayers)
	}
	if ours(e) != noTunnel(t, rules) {
		t.Fatalf("%d filters installed, want %d", ours(e), noTunnel(t, rules))
	}
	if ok, err := f.Installed(context.Background()); err != nil || !ok {
		t.Fatalf("Installed = %v %v", ok, err)
	}
}

// A second Install is a replacement: the new filters go in first, the old ones leave afterwards, so
// there is no moment with fewer blocks than before.
func TestInstallReplacesWithoutAGap(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	first := len(e.rules)
	e.log = nil
	np := plan()
	np.Endpoints = np.Endpoints[:1]
	if err := f.Install(context.Background(), build(t, np)); err != nil {
		t.Fatal(err)
	}
	if want := noTunnel(t, build(t, np)); ours(e) != want {
		t.Fatalf("%d filters after the replacement, want %d (first install had %d)", ours(e), want, first)
	}
	lastAdd, firstDel := -1, -1
	for i, l := range e.log {
		if l == "add-rule" {
			lastAdd = i
		}
		if l == "del-rule" && firstDel < 0 {
			firstDel = i
		}
	}
	if lastAdd < 0 || firstDel < 0 || firstDel < lastAdd {
		t.Fatalf("old filters were deleted before the new ones were in: %v", e.log)
	}
	for _, l := range e.log {
		if l == "del-provider" || l == "del-sublayer" {
			t.Fatalf("the provider or sublayer was removed during a replacement: %v", e.log)
		}
	}
	if len(e.providers) != 1 || len(e.sublayers) != 1 {
		t.Error("provider or sublayer duplicated")
	}
}

func TestInstallFailureKeepsTheOldFilters(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	before := ours(e)
	e.adds, e.failAdd = 0, 5
	np := plan()
	np.Endpoints = np.Endpoints[:1]
	if err := f.Install(context.Background(), build(t, np)); err == nil {
		t.Fatal("Install succeeded with AddRule failing")
	}
	if ours(e) != before {
		t.Fatalf("%d filters after a failed replacement, want the old %d (new ones added before the failure must be taken back)", ours(e), before)
	}
}

func TestInstallRefusesAnOpenSet(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	open := []tun.Rule{{Name: "permit-all", Action: tun.Permit, Dir: tun.Out, Weight: 10, Persistent: true}}
	if err := f.Install(context.Background(), open); !errors.Is(err, tun.ErrBadRules) {
		t.Fatalf("Install(open set) = %v", err)
	}
	if len(e.rules) != 0 || len(e.log) != 0 {
		t.Fatalf("the engine was touched: %v", e.log)
	}
}

func TestInstallWithoutRights(t *testing.T) {
	e := &fakeEngine{denied: true}
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); !errors.Is(err, tun.ErrNoPrivilege) {
		t.Fatalf("Install = %v, want ErrNoPrivilege", err)
	}
}

func TestSetTunnelLUIDReplacesWithoutRemoving(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	has := func(luid uint64) bool {
		for _, r := range e.rules {
			if r.Name == "EchoFog: permit-tunnel-v4-out" {
				for _, m := range r.Conditions {
					if m.Field == wf.FieldIPLocalInterface && m.Value == luid {
						return true
					}
				}
			}
		}
		return false
	}
	if has(testLUID) {
		t.Fatal("tunnel rule before there is an adapter")
	}
	e.log = nil
	if err := f.SetTunnelLUID(testLUID); err != nil {
		t.Fatal(err)
	}
	if !has(testLUID) {
		t.Fatal("tunnel rule does not name the adapter")
	}
	for _, l := range e.log {
		if l == "del-provider" || l == "del-sublayer" {
			t.Fatalf("SetTunnelLUID removed the provider: %v", e.log)
		}
	}
	// A later Install (Update of the plan) keeps the adapter.
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	if !has(testLUID) {
		t.Fatal("Install forgot the adapter")
	}
	// Before any Install there is nothing to replace.
	if err := newFW(&fakeEngine{}).SetTunnelLUID(testLUID); err != nil {
		t.Fatalf("SetTunnelLUID with nothing installed = %v", err)
	}
}

func TestRemoveDeletesOnlyOurObjects(t *testing.T) {
	e := &fakeEngine{}
	foreign := &wf.Rule{ID: wf.RuleID{Data1: 99}, Name: "someone else", Provider: wf.ProviderID{Data1: 7}}
	e.rules = append(e.rules, foreign)
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ours(e) != 0 || len(e.sublayers) != 0 || len(e.providers) != 0 {
		t.Fatalf("left behind: %d filters, %d sublayers, %d providers", ours(e), len(e.sublayers), len(e.providers))
	}
	if len(e.rules) != 1 || e.rules[0] != foreign {
		t.Fatal("a foreign filter was touched")
	}
	if ok, _ := f.Installed(context.Background()); ok {
		t.Fatal("still installed")
	}
	// Removing nothing is fine.
	if err := f.Remove(context.Background()); err != nil {
		t.Fatalf("second Remove = %v", err)
	}
}

func TestRemoveThatFailsReports(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	e.failDel = fmt.Errorf("busy")
	if err := f.Remove(context.Background()); err == nil {
		t.Fatal("Remove reported success although filters stay")
	}
	if ok, _ := f.Installed(context.Background()); !ok {
		t.Fatal("Installed says no although filters stay")
	}
}

func TestRemoveAllOver(t *testing.T) {
	e := &fakeEngine{}
	f := newFW(e)
	if err := f.Install(context.Background(), build(t, plan())); err != nil {
		t.Fatal(err)
	}
	n, err := removeAll(e)
	if err != nil || n != noTunnel(t, build(t, plan())) {
		t.Fatalf("removeAll = %d, %v", n, err)
	}
	if ours(e) != 0 || len(e.providers) != 0 || len(e.sublayers) != 0 {
		t.Fatal("not everything was removed")
	}
	if n, err := removeAll(e); err != nil || n != 0 {
		t.Fatalf("removeAll on a clean engine = %d, %v", n, err)
	}
}
