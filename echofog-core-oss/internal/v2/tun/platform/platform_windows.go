//go:build windows

// Package platform ties the drivers of the kill switch to the platform: on Windows the persistent
// WFP filters (winwfp), the DNS guard (windns) and the Wintun tunnel (wintunnel), driven through an
// elevated helper (ks). Elsewhere nothing is supported yet.
package platform

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/ks"
	"github.com/echofog/echofog-core/internal/v2/tun/windns"
	"github.com/echofog/echofog-core/internal/v2/tun/wintunnel"
	"github.com/echofog/echofog-core/internal/v2/tun/winwfp"
	"golang.org/x/sys/windows"
)

// HelperFlag as os.Args[1] makes the executable run RunHelper.
const HelperFlag = ks.HelperFlag

// helperStartTimeout covers the prompt for administrator rights.
const helperStartTimeout = 2 * time.Minute

// Supported reports whether the kill switch has a driver here.
func Supported() bool { return true }

// Drivers is the application side: one Client that is the firewall, the tunnel and the DNS guard.
// exe is the executable the helper is started from (normally os.Executable()). Nothing is started
// until the first operation needs rights.
func Drivers(exe string) *ks.Client {
	return ks.NewClient(ks.ClientConfig{
		Connect: ks.Connector(launchElevated, exe, nil, helperStartTimeout),
		// A restart of the application asks before there is any helper: the marker file and, when
		// the BFE may be read, the filters themselves.
		Installed: winwfp.NewFirewall().Installed,
	})
}

// RunHelper is the main of the elevated helper; args follow HelperFlag. It returns the exit code.
func RunHelper(args []string) int {
	hardenHelper()
	return ks.ServeHelper(args, func() (ks.Backend, func(), error) {
		if !isAdmin() {
			return ks.Backend{}, nil, tun.ErrNoPrivilege
		}
		fw := winwfp.NewFirewall()
		return ks.Backend{Firewall: fw, Tunnel: wintunnel.New(fw), DNS: windns.New()}, nil, nil
	})
}

// FiltersStand reports whether the filters of the kill switch stand: the marker the elevated helper
// keeps, and the BFE itself when this process may read it. It needs no rights and starts nothing,
// so it can be asked at start-up and by the interface.
func FiltersStand(ctx context.Context) bool {
	ok, _ := winwfp.NewFirewall().Installed(ctx)
	return ok
}

// Release removes the filters and restores the DNS policy through the elevated helper (one prompt
// for rights). It is the "Disable protection" of the interface when no mode is running.
func Release(ctx context.Context, exe string) error {
	c := Drivers(exe)
	defer c.Close()
	return errors.Join(c.Restore(ctx), c.Remove(ctx))
}

// Report is what RemoveAll did.
type Report struct {
	Filters int // filters of the kill switch removed
}

// RemoveAll is the manual removal (efv2 killswitch remove): every filter, the sublayer and the
// provider of the kill switch, the DNS policy rule and the policy values as they were. It needs
// administrator rights, and on a computer that has none of it, it removes nothing and succeeds.
func RemoveAll(ctx context.Context) (Report, error) {
	if !isAdmin() {
		return Report{}, tun.ErrNoPrivilege
	}
	n, err := winwfp.RemoveAll()
	rep := Report{Filters: n}
	if derr := windns.New().Restore(ctx); derr != nil {
		err = errors.Join(err, derr)
	}
	return rep, err
}

func isAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// hardenHelper runs first in the elevated helper: DLLs come from System32 only (the helper is the
// application's own executable, started from a folder other programs can write to).
func hardenHelper() {
	_ = windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
}

// launchElevated starts the helper through the prompt for rights.
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
			return fmt.Errorf("%w: the administrator prompt was declined", tun.ErrNoPrivilege)
		}
		return fmt.Errorf("start the helper: %w", err)
	}
	return nil
}
