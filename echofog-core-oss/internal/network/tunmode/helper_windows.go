package tunmode

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// DeviceName is the adapter name shown in Windows network settings.
const DeviceName = "EchoFog"

// A fixed GUID keeps Windows from creating "Network 2, 3, ..." profiles on every start.
const deviceGUID = "{7E4C6A1E-EF06-4F0D-9C2A-5E3C0F0E0F06}"

func deviceURL() string { return "tun://" + DeviceName + "?guid=" + deviceGUID }

func preflight() error { return ensureWintun() }

// ensureWintun makes sure wintun.dll can be found by the loader before the adapter is created.
// It writes the embedded driver next to the executable; if that directory is not writable (e.g.
// the app is installed under Program Files and not elevated yet), it writes to a per-user cache
// directory and adds it to the DLL search path instead. A wintun.dll already present next to the
// executable is left as is, so an operator can override the embedded one.
func ensureWintun() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	beside := filepath.Join(filepath.Dir(exe), "wintun.dll")
	if _, err := os.Stat(beside); err == nil {
		return nil // already there (shipped alongside, or written on an earlier run)
	}
	if len(wintunDLL) == 0 {
		return fmt.Errorf("wintun.dll is missing next to %s and none is embedded for this architecture", filepath.Base(exe))
	}
	if err := writeFileIfAbsent(beside, wintunDLL); err == nil {
		return nil
	}
	// fall back to a writable cache directory the loader is told to search
	cache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("cannot place wintun.dll: %w", err)
	}
	dir := filepath.Join(cache, "EchoFog")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFileIfAbsent(filepath.Join(dir, "wintun.dll"), wintunDLL); err != nil {
		return err
	}
	if err := windows.SetDllDirectory(dir); err != nil {
		return fmt.Errorf("wintun.dll written to %s but it could not be added to the DLL search path: %w", dir, err)
	}
	return nil
}

// writeFileIfAbsent writes data to a new file at path, via a temp file renamed into place so a
// torn write never leaves a half-driver behind. If the file appeared meanwhile, it is kept.
func writeFileIfAbsent(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wintun-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func netsh(args ...string) error {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// configure gives the adapter its address and DNS and routes everything through it with two
// /1 routes, which win over the default route without replacing it. The node's own socket is
// pinned to the physical interface (IP_UNICAST_IF), so the bypass port is not used here.
// The routes go away with the adapter; the returned teardown only has to undo the DNS-leak policy.
func configure(_ int) (func(), error) {
	if _, err := waitInterface(DeviceName); err != nil {
		return nil, err
	}
	name := "name=" + DeviceName
	steps := [][]string{
		{"interface", "ipv4", "set", "address", name, "source=static", "address=" + tunAddr4, "mask=" + tunMask4},
		{"interface", "ipv4", "set", "interface", DeviceName, "metric=1"},
		{"interface", "ipv4", "set", "dnsservers", name, "source=static", "address=" + tunDNS4, "register=none", "validate=no"},
		{"interface", "ipv4", "add", "route", "0.0.0.0/1", DeviceName, "0.0.0.0", "metric=1", "store=active"},
		{"interface", "ipv4", "add", "route", "128.0.0.0/1", DeviceName, "0.0.0.0", "metric=1", "store=active"},
	}
	for _, s := range steps {
		if err := netsh(s...); err != nil {
			return nil, err
		}
	}
	// IPv6 is captured too so it cannot leak around the tunnel; failures (IPv6 disabled) are fine.
	_ = netsh("interface", "ipv6", "add", "address", DeviceName, tunCIDR6, "store=active")
	_ = netsh("interface", "ipv6", "add", "route", "::/1", DeviceName, "::", "metric=1", "store=active")
	_ = netsh("interface", "ipv6", "add", "route", "8000::/1", DeviceName, "::", "metric=1", "store=active")

	// Stop Windows from also querying the physical adapter's DNS servers in parallel (see
	// enableDNSLeakProtection). Without this the EchoFog adapter is preferred but the ISP still
	// sees the queries. If it cannot be applied, the tunnel still routes DNS to the fake resolver;
	// only the parallel leak remains, so this is best-effort, not fatal.
	revertDNS, err := enableDNSLeakProtection()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tun-helper: DNS leak protection not applied:", err)
		revertDNS = func() {}
	}
	return revertDNS, nil
}

// dnsPolicyKey holds the machine DNS client policy. The two values below disable Windows "smart
// multi-homed name resolution", which otherwise sends every lookup to the DNS servers of all
// interfaces at once — so even with the EchoFog adapter preferred, the physical adapter's resolver
// (the ISP) still receives the query. OpenVPN's block-outside-dns and WireGuard's firewall rules
// address the same leak; this policy is the lighter, fully reversible equivalent and fails open
// (once the adapter is gone Windows simply uses the remaining interface's DNS).
const dnsPolicyKey = `SOFTWARE\Policies\Microsoft\Windows NT\DNSClient`

var dnsLeakValues = []string{"DisableSmartNameResolution", "DisableParallelAandAAAA"}

// enableDNSLeakProtection sets the policy values and returns a function that restores the previous
// state exactly (a prior value is put back; a value that did not exist is removed).
func enableDNSLeakProtection() (func(), error) {
	return setDWordFlags(registry.LOCAL_MACHINE, dnsPolicyKey, dnsLeakValues)
}

// setDWordFlags sets each named value to 1 under root\path, creating the key if needed, and returns
// a function that restores exactly what was there before (prior value, or removal when it was
// absent). Factored out of enableDNSLeakProtection so the snapshot/restore logic can be tested
// against a scratch key without administrator rights.
func setDWordFlags(root registry.Key, path string, names []string) (func(), error) {
	key, _, err := registry.CreateKey(root, path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return func() {}, fmt.Errorf("open %s: %w", path, err)
	}
	type prior struct {
		had bool
		val uint64
	}
	saved := make(map[string]prior, len(names))
	for _, vname := range names {
		if v, _, err := key.GetIntegerValue(vname); err == nil {
			saved[vname] = prior{had: true, val: v}
		} else {
			saved[vname] = prior{had: false}
		}
		if err := key.SetDWordValue(vname, 1); err != nil {
			key.Close()
			return func() {}, fmt.Errorf("set %s: %w", vname, err)
		}
	}
	key.Close()
	return func() {
		k, err := registry.OpenKey(root, path, registry.SET_VALUE)
		if err != nil {
			return
		}
		defer k.Close()
		for vname, p := range saved {
			if p.had {
				_ = k.SetDWordValue(vname, uint32(p.val))
			} else {
				_ = k.DeleteValue(vname)
			}
		}
	}, nil
}

func flushDNS() {
	cmd := exec.Command("ipconfig", "/flushdns")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// launchElevated starts the helper through the UAC prompt.
func launchElevated(exe string, args []string) error {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	if err := windows.ShellExecute(0, verb, file, params, dir, windows.SW_HIDE); err != nil {
		if err == windows.ERROR_CANCELLED {
			return fmt.Errorf("administrator permission was declined")
		}
		return fmt.Errorf("start the helper: %w", err)
	}
	return nil
}

// DefaultInterfaceIndex is the interface Internet traffic uses right now. It must be read before
// the TUN goes up; the node pins its own socket to it.
func DefaultInterfaceIndex() (int, error) {
	var idx uint32
	if err := windows.GetBestInterfaceEx(&windows.SockaddrInet4{Addr: [4]byte{1, 1, 1, 1}}, &idx); err != nil {
		return 0, fmt.Errorf("find the default network interface: %w", err)
	}
	if ifc, err := net.InterfaceByIndex(int(idx)); err == nil && ifc.Name == DeviceName {
		return 0, fmt.Errorf("the EchoFog adapter is still active from a previous run; turn it off first")
	}
	return int(idx), nil
}
