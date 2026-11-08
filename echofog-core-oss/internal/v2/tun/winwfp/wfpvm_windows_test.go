//go:build windows && wfpvm

package winwfp

// The integration test of the driver. It installs REAL persistent filters that block the network of
// the machine it runs on. It is built only with the tag wfpvm and refuses to run anywhere but in a
// virtual machine, with administrator rights, and only when ECHOFOG_WFP_VM=VM is set. Never run it
// on a shared or a working computer. Instructions: docs/kill-switch-windows.md, script:
// internal/v2/tun/winwfp/vm/run-wfpvm.ps1.

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func looksLikeVM() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	var text string
	for _, name := range []string{"SystemManufacturer", "SystemProductName", "BIOSVendor"} {
		if v, _, err := k.GetStringValue(name); err == nil {
			text += " " + v
		}
	}
	text = strings.ToLower(text)
	for _, w := range []string{"virtual machine", "vmware", "virtualbox", "qemu", "kvm", "xen", "parallels", "bochs", "hyper-v"} {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func vmOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("ECHOFOG_WFP_VM") != "VM" {
		t.Skip("set ECHOFOG_WFP_VM=VM inside a virtual machine to run the WFP integration test")
	}
	if !looksLikeVM() {
		t.Skip("this computer does not look like a virtual machine: the test would cut its network")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("administrator rights are needed")
	}
}

func dumpFilters(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "filters.xml")
	if b, err := exec.Command("netsh", "wfp", "show", "filters", "file="+out).CombinedOutput(); err != nil {
		t.Fatalf("netsh wfp show filters: %v: %s", err, b)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(string(raw))
}

func countOurs(xml string) int {
	return strings.Count(xml, "<providerkey>"+strings.ToLower(ProviderKeyString)+"</providerkey>")
}

func blocked(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

func TestRealFiltersInTheVM(t *testing.T) {
	vmOnly(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = RemoveAll() })
	_, _ = RemoveAll() // a clean start

	plan := tun.Plan{
		Endpoints:  []tun.Endpoint{{Addr: netip.MustParseAddrPort("192.0.2.10:443"), Proto: tun.ProtoTCP}},
		TunnelPool: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")},
	}
	rules, err := tun.BuildRules(plan)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Compile(rules, 0, randomGUID)

	f := NewFirewall()
	if err := f.Install(ctx, rules); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// 1. The filters are in the BFE, under our provider and sublayer, by their names.
	xml := dumpFilters(t)
	if got := countOurs(xml); got != len(want) {
		t.Fatalf("netsh wfp show filters lists %d filters of provider %s, want %d", got, ProviderKeyString, len(want))
	}
	for _, r := range want {
		if !strings.Contains(xml, "<name>"+strings.ToLower(r.Name)+"</name>") {
			t.Errorf("filter %q is not in the dump", r.Name)
		}
	}
	if !strings.Contains(xml, strings.ToLower(SublayerKeyString)) {
		t.Error("the sublayer is not in the dump")
	}

	// 2. They close the network, except loopback.
	if !blocked("1.1.1.1:443") || !blocked("8.8.8.8:53") {
		t.Fatal("a connection to the Internet went through the filters")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if blocked(ln.Addr().String()) {
		t.Fatal("loopback is blocked")
	}

	// 3. They outlive the process that installed them: a new handle finds them.
	g := NewFirewall()
	if ok, err := g.Installed(ctx); err != nil || !ok {
		t.Fatalf("Installed from a new handle = %v %v", ok, err)
	}

	// 4. A new plan replaces the old one without removing the provider.
	plan.Endpoints = []tun.Endpoint{{Addr: netip.MustParseAddrPort("192.0.2.11:8443"), Proto: tun.ProtoUDP}}
	rules2, _ := tun.BuildRules(plan)
	if err := g.Install(ctx, rules2); err != nil {
		t.Fatalf("replace: %v", err)
	}
	xml = dumpFilters(t)
	want2, _ := Compile(rules2, 0, randomGUID)
	if got := countOurs(xml); got != len(want2) {
		t.Fatalf("after the replacement %d filters, want %d", got, len(want2))
	}

	// 5. Removal by the provider key gives the network back.
	n, err := RemoveAll()
	if err != nil || n != len(want2) {
		t.Fatalf("RemoveAll = %d, %v; want %d", n, err, len(want2))
	}
	if got := countOurs(dumpFilters(t)); got != 0 {
		t.Fatalf("%d filters left after RemoveAll", got)
	}
	if ok, _ := g.Installed(ctx); ok {
		t.Fatal("Installed after RemoveAll")
	}
}
