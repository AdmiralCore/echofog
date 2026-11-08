//go:build windows

// Package windns is the DNS guard of the Windows kill switch (T10.6b): while the mode runs, names
// are resolved through the tunnel and nowhere else. Three things make that so:
//
//   - the DNS client policy DisableSmartNameResolution and DisableParallelAandAAAA (the resolver
//     otherwise asks the DNS server of every interface in parallel, the classic leak);
//   - a catch-all name resolution policy rule ("." to the resolver of the tunnel);
//   - the firewall rules of package winwfp, which block ports 53 and 853 outside the tunnel.
//
// The original values of the policy are kept until Restore, also across a crash of the process.
package windns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// PolicyKey is the machine DNS client policy.
	PolicyKey = `SOFTWARE\Policies\Microsoft\Windows NT\DNSClient`
	// StateKey keeps the values the policy had before the first Protect.
	StateKey = `SOFTWARE\EchoFog\KillSwitchDNS`
	// RuleLabel marks the namespace rule as ours (the Comment of the rule).
	RuleLabel = "EchoFog kill switch"
)

var policyValues = []string{"DisableSmartNameResolution", "DisableParallelAandAAAA"}

// Guard is the tun.DNSGuard of Windows.
type Guard struct {
	root      registry.Key
	policy    string // the DNS client policy key
	state     string // where the original values are kept until Restore
	runPS     func(ctx context.Context, script string, env []string) error
	flush     func()
	nrptLabel string
}

// New returns the guard over the real registry (HKLM), PowerShell and ipconfig. It needs
// administrator rights.
func New() *Guard {
	return &Guard{root: registry.LOCAL_MACHINE, policy: PolicyKey, state: StateKey, runPS: powershell, flush: flushDNS, nrptLabel: RuleLabel}
}

func system32(name string) string {
	if dir, err := windows.GetSystemDirectory(); err == nil {
		return filepath.Join(dir, name)
	}
	return name
}

func powershell(ctx context.Context, script string, env []string) error {
	exe := system32(`WindowsPowerShell\v1.0\powershell.exe`)
	cmd := exec.CommandContext(ctx, exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("windns: powershell: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func flushDNS() {
	cmd := exec.Command(system32("ipconfig.exe"), "/flushdns")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// snapshot keeps the original values once: a second Protect, or a Protect after a crash, finds the
// snapshot and leaves it alone, so Restore always gives back what the user had.
func (g *Guard) snapshot() error {
	st, _, err := registry.CreateKey(g.root, g.state, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("windns: open %s: %w", g.state, err)
	}
	defer st.Close()
	if _, _, err := st.GetIntegerValue("saved"); err == nil {
		return nil
	}
	pol, err := registry.OpenKey(g.root, g.policy, registry.QUERY_VALUE)
	for _, name := range policyValues {
		had, val := uint32(0), uint32(0)
		if err == nil {
			if v, _, e := pol.GetIntegerValue(name); e == nil {
				had, val = 1, uint32(v)
			}
		}
		if e := st.SetDWordValue(name+".had", had); e != nil {
			return e
		}
		if e := st.SetDWordValue(name+".val", val); e != nil {
			return e
		}
	}
	if err == nil {
		pol.Close()
	}
	return st.SetDWordValue("saved", 1)
}

func (g *Guard) Protect(ctx context.Context, cfg tun.DNSConfig) error {
	if err := tun.CheckDNS(cfg); err != nil {
		return err
	}
	if err := g.snapshot(); err != nil {
		return err
	}
	pol, _, err := registry.CreateKey(g.root, g.policy, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("windns: open %s: %w", g.policy, err)
	}
	for _, name := range policyValues {
		if err := pol.SetDWordValue(name, 1); err != nil {
			pol.Close()
			return fmt.Errorf("windns: set %s: %w", name, err)
		}
	}
	pol.Close()
	if err := g.runPS(ctx, namespaceRule(g.nrptLabel), []string{"EF_RESOLVER=" + cfg.Resolver.String()}); err != nil {
		return err
	}
	g.flush()
	return nil
}

// namespaceRule replaces our rule with a catch-all one to the resolver. The text is constant but for
// the fixed label; the address is read from the environment of the child (EF_RESOLVER), so nothing
// the caller sent is ever part of the script.
func namespaceRule(label string) string {
	return "$ErrorActionPreference='Stop'; " +
		"Get-DnsClientNrptRule | Where-Object { $_.Comment -eq '" + label + "' } | Remove-DnsClientNrptRule -Force; " +
		"Add-DnsClientNrptRule -Namespace '.' -NameServers $env:EF_RESOLVER -Comment '" + label + "'"
}

func removeRule(label string) string {
	return "$ErrorActionPreference='Stop'; " +
		"Get-DnsClientNrptRule | Where-Object { $_.Comment -eq '" + label + "' } | Remove-DnsClientNrptRule -Force"
}

func (g *Guard) Restore(ctx context.Context) error {
	var errs []error
	if err := g.runPS(ctx, removeRule(g.nrptLabel), nil); err != nil {
		errs = append(errs, err)
	}
	st, err := registry.OpenKey(g.root, g.state, registry.QUERY_VALUE)
	if err == nil {
		defer st.Close()
		if _, _, e := st.GetIntegerValue("saved"); e == nil {
			if pol, e := registry.OpenKey(g.root, g.policy, registry.SET_VALUE); e == nil {
				for _, name := range policyValues {
					had, _, _ := st.GetIntegerValue(name + ".had")
					val, _, _ := st.GetIntegerValue(name + ".val")
					if had == 1 {
						if e := pol.SetDWordValue(name, uint32(val)); e != nil {
							errs = append(errs, e)
						}
					} else if e := pol.DeleteValue(name); e != nil && !errors.Is(e, registry.ErrNotExist) {
						errs = append(errs, e)
					}
				}
				pol.Close()
			}
			if len(errs) == 0 {
				_ = registry.DeleteKey(g.root, g.state)
			}
		}
	}
	g.flush()
	return errors.Join(errs...)
}

var _ tun.DNSGuard = (*Guard)(nil)
