//go:build windows

package windns

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"golang.org/x/sys/windows/registry"
)

// The tests use a scratch key under HKCU and a fake PowerShell: no rights are needed and no system
// setting is touched.

type rig struct {
	g       *Guard
	policy  string
	state   string
	mu      sync.Mutex
	scripts []string
	envs    [][]string
	flushes int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	base := fmt.Sprintf(`Software\EchoFogTest\windns-%s`, strings.ReplaceAll(t.Name(), "/", "_"))
	r := &rig{policy: base + `\Policy`, state: base + `\State`}
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, r.policy)
		_ = registry.DeleteKey(registry.CURRENT_USER, r.state)
		_ = registry.DeleteKey(registry.CURRENT_USER, base)
	})
	r.g = &Guard{
		root: registry.CURRENT_USER, policy: r.policy, state: r.state, nrptLabel: "EchoFog kill switch",
		runPS: func(_ context.Context, s string, env []string) error {
			r.mu.Lock()
			r.scripts = append(r.scripts, s)
			r.envs = append(r.envs, env)
			r.mu.Unlock()
			return nil
		},
		flush: func() { r.mu.Lock(); r.flushes++; r.mu.Unlock() },
	}
	return r
}

func (r *rig) value(t *testing.T, name string) (uint64, bool) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, r.policy, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	return v, err == nil
}

func (r *rig) set(t *testing.T, name string, v uint32) {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, r.policy, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetDWordValue(name, v); err != nil {
		t.Fatal(err)
	}
}

var cfg = tun.DNSConfig{Tunnel: "EchoFog", Resolver: netip.MustParseAddr("198.18.0.2"), DisableSmartNameResolution: true}

func TestProtectSetsThePolicyAndTheNamespaceRule(t *testing.T) {
	r := newRig(t)
	if err := r.g.Protect(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"DisableSmartNameResolution", "DisableParallelAandAAAA"} {
		if v, ok := r.value(t, name); !ok || v != 1 {
			t.Errorf("%s = %d (set %v), want 1", name, v, ok)
		}
	}
	if len(r.scripts) != 1 {
		t.Fatalf("%d scripts: %v", len(r.scripts), r.scripts)
	}
	s := r.scripts[0]
	for _, want := range []string{"Add-DnsClientNrptRule", "EchoFog kill switch", "-Namespace", "$env:EF_RESOLVER"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	// B: the address travels in the environment of the child, never inside the text of the script
	if strings.Contains(s, "198.18.0.2") {
		t.Errorf("the resolver is pasted into the script: %s", s)
	}
	if got := r.envs[0]; len(got) != 1 || got[0] != "EF_RESOLVER=198.18.0.2" {
		t.Errorf("environment of the script: %v", got)
	}
	if r.flushes == 0 {
		t.Error("the resolver cache was not flushed: cached real addresses would bypass the tunnel resolver")
	}
}

func TestProtectIsRepeatable(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		if err := r.g.Protect(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.g.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.value(t, "DisableSmartNameResolution"); ok {
		t.Error("a repeated Protect lost the original state: the value stayed after Restore")
	}
}

func TestRestoreGivesBackTheOriginalValues(t *testing.T) {
	r := newRig(t)
	r.set(t, "DisableSmartNameResolution", 0) // the user had set it to 0 himself
	if err := r.g.Protect(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.value(t, "DisableSmartNameResolution"); v != 1 {
		t.Fatalf("not set: %d", v)
	}
	if err := r.g.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, ok := r.value(t, "DisableSmartNameResolution"); !ok || v != 0 {
		t.Errorf("DisableSmartNameResolution = %d (set %v), want the original 0", v, ok)
	}
	if _, ok := r.value(t, "DisableParallelAandAAAA"); ok {
		t.Error("DisableParallelAandAAAA did not exist before and exists now")
	}
	last := r.scripts[len(r.scripts)-1]
	if !strings.Contains(last, "Remove-DnsClientNrptRule") || !strings.Contains(last, "EchoFog kill switch") {
		t.Errorf("Restore did not remove the namespace rule:\n%s", last)
	}
}

// The helper of v1 sets and reverts the same policy values itself. Whatever it did in between, a
// Restore after a crash and a new run still gives back the first original state.
func TestRestoreAfterAnotherProcessChangedTheValues(t *testing.T) {
	r := newRig(t)
	if err := r.g.Protect(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// another process (the old helper) removes the values on its way out
	k, _ := registry.OpenKey(registry.CURRENT_USER, r.policy, registry.SET_VALUE)
	_ = k.DeleteValue("DisableSmartNameResolution")
	k.Close()
	// a new guard object (a new run) restores
	g2 := *r.g
	if err := g2.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.value(t, "DisableSmartNameResolution"); ok {
		t.Error("value left behind")
	}
}

func TestProtectRefusesWhatIsNotAResolver(t *testing.T) {
	r := newRig(t)
	for _, bad := range []tun.DNSConfig{
		{Tunnel: "EchoFog"}, // no resolver
		{Tunnel: "EchoFog", Resolver: netip.MustParseAddr("0.0.0.0")},
		{Tunnel: "EchoFog", Resolver: netip.MustParseAddr("8.8.8.8"), DisableSmartNameResolution: false},
	} {
		if err := r.g.Protect(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if len(r.scripts) != 0 {
		t.Errorf("a script ran for a bad config: %v", r.scripts)
	}
}

func TestRestoreWhenNothingWasProtected(t *testing.T) {
	r := newRig(t)
	if err := r.g.Restore(context.Background()); err != nil {
		t.Fatalf("Restore on a clean computer = %v", err)
	}
	if len(r.scripts) > 1 {
		t.Errorf("scripts: %v", r.scripts)
	}
}

// B: this code runs with administrator rights. A zone, a quote or an address outside the tunnel never
// reaches PowerShell or the registry.
func TestProtectRefusesAHostileResolver(t *testing.T) {
	for name, a := range map[string]string{
		"zone with a command": "fdfe:dcba:9876::2%a';calc.exe;'",
		"zone":                "fe80::1%eth0",
		"public":              "8.8.8.8",
		"LAN":                 "192.168.1.1",
	} {
		r := newRig(t)
		c := cfg
		c.Resolver = netip.MustParseAddr(a)
		if err := r.g.Protect(context.Background(), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(r.scripts) != 0 {
			t.Errorf("%s: a script ran: %v", name, r.scripts)
		}
		if _, ok := r.value(t, "DisableSmartNameResolution"); ok {
			t.Errorf("%s: the registry was touched", name)
		}
	}
}

// D: Restore ends the state: the key that holds the originals is gone, so a later Protect takes new
// ones and a later Restore cannot put back stale values.
func TestRestoreDeletesTheStateKey(t *testing.T) {
	r := newRig(t)
	if err := r.g.Protect(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, r.state, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal("no state key while protected")
	}
	k.Close()
	if err := r.g.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, r.state, registry.QUERY_VALUE); err == nil {
		k.Close()
		t.Fatal("the state key is still there after Restore")
	}
}
