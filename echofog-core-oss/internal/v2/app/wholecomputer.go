package app

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

// The whole-computer mode with its kill switch (T10.6b): every application of the computer goes
// through the VPN, and nothing leaves the computer outside the tunnel. The rules and the state
// machine are package tun; the drivers are given in Config.WholeComputer.

// WholeComputerConfig turns the mode on. Nil in Config: the mode is not available.
type WholeComputerConfig struct {
	Firewall tun.Firewall
	Tunnel   tun.Tunnel
	DNS      tun.DNSGuard
	// BypassUDPPort returns the UDP port of the node's own transport, which must stay off the
	// tunnel; nil or 0: none.
	BypassUDPPort func() int
	// Poll is how often the exceptions are compared with the live sessions; zero means 3 s.
	Poll time.Duration
	// PollInterval is passed to the state machine (tun.Config.PollInterval).
	PollInterval time.Duration
}

// WholeStatus is the state of the mode.
type WholeStatus struct {
	// State is tun.StateOff, StateActive or StateHeld.
	State tun.State
	// Err says why the mode is held, or why it could not start.
	Err string
}

// Whole-computer errors.
var (
	// ErrWholeOff reports a call on an app built without drivers for the mode.
	ErrWholeOff = errors.New("app: the whole-computer mode is not available")
	// ErrNeedVPN reports a start of the mode while the VPN is not running: there is no tunnel to feed.
	ErrNeedVPN = errors.New("app: the VPN is not running")
)

// RemoteLink is an ExitLink that knows the address of the node it talks to (the first hop of the
// live session: the exit itself, or the relay in the path). The mode permits exactly these.
type RemoteLink interface {
	Remote() (netip.AddrPort, bool)
}

// WholeComputerSupported reports whether the mode is available.
func (a *App) WholeComputerSupported() bool { return a.cfg.WholeComputer != nil }

// WholeComputerStart brings the mode up: the filters, the tunnel and DNS, in that order. From the
// held state it brings the tunnel back instead.
func (a *App) WholeComputerStart(ctx context.Context) error { return errors.New("not implemented") }

// WholeComputerStop ends the mode on the user's word: the tunnel goes down, DNS is restored and the
// filters leave.
func (a *App) WholeComputerStop(ctx context.Context) error { return errors.New("not implemented") }

// WholeComputerStatus reports the state of the mode.
func (a *App) WholeComputerStatus() WholeStatus { return WholeStatus{} }

// WholeComputerRecover finds filters an earlier run left and holds the computer closed; call it
// once at start-up.
func (a *App) WholeComputerRecover(ctx context.Context) error { return errors.New("not implemented") }

// WholeComputerSync compares the exceptions of the firewall with the live session of the exit and
// replaces them when they differ. It runs on a timer; it is public for the tests and for a caller
// that learns of a migration earlier.
func (a *App) WholeComputerSync(ctx context.Context) error { return errors.New("not implemented") }

// VPNStopForSwitch stops the VPN to start another exit: the mode, if it is on, stays on.
func (a *App) VPNStopForSwitch() error { return a.vpnStop("switched") }
