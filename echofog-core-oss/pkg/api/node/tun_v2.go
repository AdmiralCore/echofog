package node

import (
	"context"
	"os"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/tun/platform"
)

// The kill switch of protocol 2 on the facade (T10.6b): the entry of its elevated helper, the
// configuration of the app's whole-computer mode, and the two questions the interface asks before a
// node exists (is the computer held, and let it go).

// KillSwitchHelperFlag as os.Args[1] means the process was started as the elevated helper of the
// protocol 2 kill switch; the executable calls RunKillSwitchHelper with the rest of the arguments and
// exits with its result. No window, no node.
const KillSwitchHelperFlag = platform.HelperFlag

// RunKillSwitchHelper runs the helper with the arguments that follow KillSwitchHelperFlag.
func RunKillSwitchHelper(args []string) int { return platform.RunHelper(args) }

// V2WholeComputer is the drivers of the whole-computer mode for app.Config.WholeComputer, or nil
// where the platform has none. bypassUDPPort returns the node's own UDP port (nil: none).
func V2WholeComputer(bypassUDPPort func() int) *app.WholeComputerConfig {
	if !platform.Supported() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	c := platform.Drivers(exe)
	return &app.WholeComputerConfig{Firewall: c, Tunnel: c, DNS: c, BypassUDPPort: bypassUDPPort}
}

// v2KillSwitchHeld: the filters of the protocol 2 kill switch stand. It asks the marker of the helper
// (and the BFE when allowed), needs no rights and starts nothing.
func v2KillSwitchHeld() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return platform.FiltersStand(ctx)
}

// v2ReleaseKillSwitch removes the filters of the protocol 2 kill switch and restores the DNS policy,
// through the elevated helper (one prompt for rights). With no filters it does nothing.
func v2ReleaseKillSwitch() error {
	if !v2KillSwitchHeld() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return platform.Release(ctx, exe)
}
