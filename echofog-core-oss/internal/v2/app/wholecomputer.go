package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

// The whole-computer mode with its kill switch (T10.6b): every application of the computer goes
// through the VPN, and nothing leaves the computer outside the tunnel. The rules and the state
// machine are package tun; the drivers are given in Config.WholeComputer.
//
// What the firewall permits outside the tunnel is the first hop of the live session to the exit (the
// exit itself, or the relay in the path) and nothing else: not the addresses of the exit's card.
// The addresses that session had stay permitted for as long as this exit is used, so that a dropped
// session can be dialed again, and are dropped when another exit is chosen.

// WholeComputerConfig turns the mode on. Nil in Config: the mode is not available.
type WholeComputerConfig struct {
	Firewall tun.Firewall
	Tunnel   tun.Tunnel
	DNS      tun.DNSGuard
	// BypassUDPPort returns the UDP port of the node's own transport, which must stay off the
	// tunnel; nil or 0: none.
	BypassUDPPort func() int
	// Poll is how often the exceptions are compared with the live session; zero means 3 s.
	Poll time.Duration
	// PollInterval is passed to the state machine (tun.Config.PollInterval).
	PollInterval time.Duration
}

// WholeStatus is the state of the mode.
type WholeStatus struct {
	// State is tun.StateOff, StateActive or StateHeld.
	State tun.State
	// Err says why the mode is held, or why it last failed.
	Err string
}

// Whole-computer errors.
var (
	// ErrWholeOff reports a call on an app built without drivers for the mode.
	ErrWholeOff = errors.New("app: the whole-computer mode is not available")
	// ErrWholeStop reports a stop that did not finish (the DNS policy could not be restored): the mode is
	// held, the computer stays blocked, and the stop can be repeated.
	ErrWholeStop = errors.New("app: the protection could not be switched off completely")
	// ErrNeedVPN reports a start of the mode while the VPN is not running: there is no tunnel to feed.
	ErrNeedVPN = errors.New("app: the VPN is not running")
)

// RemoteLink is an ExitLink that knows the address of the node it talks to (the first hop of the
// live session: the exit itself, or the relay in the path). The mode permits exactly these.
type RemoteLink interface {
	// Remote makes sure the session to the exit is up and returns the address of its first hop.
	Remote(ctx context.Context) (netip.AddrPort, error)
}

// The addresses of the tunnel device; the resolver sits inside the pool.
const (
	wholeTunName = "EchoFog"
	// maxSeen bounds the addresses kept per exit; each is permitted for TCP and for UDP.
	maxSeen = tun.MaxEndpoints / 2
)

var (
	wholeResolver = netip.MustParseAddr("198.18.0.2")
	wholeAddrs    = []netip.Prefix{netip.MustParsePrefix("198.18.0.1/15"), netip.MustParsePrefix("fdfe:dcba:9876::1/126")}
	wholePool     = tun.DefaultTunnelPool
)

type wholeState struct {
	op sync.Mutex // one operation at a time (Start, Stop, Sync, Recover); held across driver calls

	mu      sync.Mutex // guards the fields below; never held across a driver call
	sw      *tun.Switch
	node    string           // the exit the seen addresses belong to
	seen    []netip.AddrPort // first hops of the sessions to that exit
	applied []tun.Endpoint   // what the firewall was last given
	err     string
	stop    chan struct{} // ends the timer of Sync
}

// WholeComputerSupported reports whether the mode is available.
func (a *App) WholeComputerSupported() bool { return a.cfg.WholeComputer != nil }

func (a *App) wholeSwitch(plan tun.Plan, tc tun.TunConfig) *tun.Switch {
	w := &a.whole
	cfg := a.cfg.WholeComputer
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sw == nil {
		w.sw = tun.New(tun.Config{Firewall: cfg.Firewall, Tunnel: cfg.Tunnel, DNS: cfg.DNS, Plan: plan, Tun: tc,
			PollInterval: cfg.PollInterval,
			OnState: func(st tun.State, err error) {
				w.mu.Lock()
				w.err = ""
				if err != nil {
					w.err = err.Error()
				}
				w.mu.Unlock()
			}})
	}
	return w.sw
}

func (a *App) wholeCurrent() *tun.Switch {
	a.whole.mu.Lock()
	defer a.whole.mu.Unlock()
	return a.whole.sw
}

// live returns what a plan is made from: the proxy, the exit and its link.
func (a *App) wholeVPN() (listen, node string, link ExitLink, ok bool) {
	v := &a.vpn
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.running {
		return "", "", nil, false
	}
	return v.listen, v.exit, v.link, true
}

// wholePlan makes the plan from the live session. The first hop is asked for (and the session
// opened) before the filters go in: afterwards the exit could not be reached.
func (a *App) wholePlan(ctx context.Context, node string, link ExitLink) (tun.Plan, error) {
	w := &a.whole
	rl, ok := link.(RemoteLink)
	if !ok {
		return tun.Plan{}, errors.New("app: the exit link does not tell its address")
	}
	ap, err := rl.Remote(ctx)
	w.mu.Lock()
	if w.node != node {
		w.node, w.seen = node, nil
	}
	if err == nil && ap.IsValid() {
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if !slices.Contains(w.seen, ap) {
			w.seen = append(w.seen, ap)
			if len(w.seen) > maxSeen {
				w.seen = w.seen[len(w.seen)-maxSeen:]
			}
		}
	}
	seen := slices.Clone(w.seen)
	w.mu.Unlock()
	if len(seen) == 0 {
		if err == nil {
			err = errors.New("app: no address")
		}
		return tun.Plan{}, fmt.Errorf("app: the session to the exit is not up: %w", err)
	}
	p := tun.Plan{TunnelPool: wholePool, AllowDHCP: true}
	for _, s := range seen {
		p.Endpoints = append(p.Endpoints, tun.Endpoint{Addr: s, Proto: tun.ProtoTCP}, tun.Endpoint{Addr: s, Proto: tun.ProtoUDP})
	}
	return p, nil
}

func (a *App) wholeTunConfig(listen string) tun.TunConfig {
	port := 0
	if f := a.cfg.WholeComputer.BypassUDPPort; f != nil {
		port = f()
	}
	return tun.TunConfig{Name: wholeTunName, Addrs: wholeAddrs, Resolver: wholeResolver, Socks: listen, BypassUDPPort: port}
}

// WholeComputerStart brings the mode up: the filters, the tunnel and DNS, in that order. From the
// held state it brings the tunnel back instead.
func (a *App) WholeComputerStart(ctx context.Context) error {
	if a.cfg.WholeComputer == nil {
		return ErrWholeOff
	}
	w := &a.whole
	w.op.Lock()
	defer w.op.Unlock()
	listen, node, link, ok := a.wholeVPN()
	if !ok {
		return ErrNeedVPN
	}
	plan, err := a.wholePlan(ctx, node, link)
	if err != nil {
		return a.wholeFail(err)
	}
	tc := a.wholeTunConfig(listen)
	// A switch that is off carries the plan it was made with; a new run gets a new one.
	if cur := a.wholeCurrent(); cur != nil && cur.State() == tun.StateOff {
		a.wholeReset()
	}
	sw := a.wholeSwitch(plan, tc)
	switch sw.State() {
	case tun.StateActive:
		return nil
	case tun.StateHeld:
		sw.SetTunnel(tc)
		if err := sw.Update(ctx, plan); err != nil {
			return a.wholeFail(err)
		}
		if err := sw.Reconnect(ctx); err != nil {
			return a.wholeFail(err)
		}
	default:
		if err := sw.Start(ctx); err != nil {
			return a.wholeFail(err)
		}
	}
	a.wholeApplied(plan)
	a.wholeTimer()
	return nil
}

func (a *App) wholeReset() {
	w := &a.whole
	w.mu.Lock()
	old := w.sw
	w.sw = nil
	w.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (a *App) wholeFail(err error) error {
	w := &a.whole
	w.mu.Lock()
	w.err = err.Error()
	w.mu.Unlock()
	return err
}

func (a *App) wholeApplied(p tun.Plan) {
	w := &a.whole
	w.mu.Lock()
	w.applied = slices.Clone(p.Endpoints)
	w.err = ""
	w.mu.Unlock()
}

// WholeComputerStop ends the mode on the user's word: the tunnel goes down, DNS is restored and the
// filters leave. It also takes away the filters of an earlier run that was never recovered.
func (a *App) WholeComputerStop(ctx context.Context) error {
	if a.cfg.WholeComputer == nil {
		return ErrWholeOff
	}
	w := &a.whole
	w.op.Lock()
	defer w.op.Unlock()
	a.wholeNoTimer()
	sw := a.wholeCurrent()
	if sw == nil {
		sw = a.wholeSwitch(tun.Plan{}, tun.TunConfig{})
	}
	if sw.State() == tun.StateOff {
		_ = sw.Recover(ctx)
	}
	if err := sw.Stop(ctx); err != nil {
		return a.wholeFail(err)
	}
	w.mu.Lock()
	w.applied, w.err = nil, ""
	w.mu.Unlock()
	return nil
}

// wholeStopIfOn is the stop that goes with the VPN's: the user switched the VPN off, the computer is
// handed back.
func (a *App) wholeStopIfOn() {
	if a.cfg.WholeComputer == nil {
		return
	}
	if sw := a.wholeCurrent(); sw != nil && sw.State() != tun.StateOff {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = a.WholeComputerStop(ctx)
	}
}

// WholeComputerStatus reports the state of the mode.
func (a *App) WholeComputerStatus() WholeStatus {
	w := &a.whole
	w.mu.Lock()
	sw, err := w.sw, w.err
	w.mu.Unlock()
	if sw == nil {
		return WholeStatus{}
	}
	return WholeStatus{State: sw.State(), Err: err}
}

// WholeComputerRecover finds filters an earlier run left and holds the computer closed; call it
// once at start-up.
func (a *App) WholeComputerRecover(ctx context.Context) error {
	if a.cfg.WholeComputer == nil {
		return ErrWholeOff
	}
	w := &a.whole
	w.op.Lock()
	defer w.op.Unlock()
	sw := a.wholeSwitch(tun.Plan{}, tun.TunConfig{})
	return sw.Recover(ctx)
}

// WholeComputerSync compares the exceptions of the firewall with the live session of the exit and
// replaces them when they differ. It runs on a timer; it is public for the tests and for a caller
// that learns of a migration earlier.
func (a *App) WholeComputerSync(ctx context.Context) error {
	if a.cfg.WholeComputer == nil {
		return ErrWholeOff
	}
	w := &a.whole
	w.op.Lock()
	defer w.op.Unlock()
	sw := a.wholeCurrent()
	if sw == nil || sw.State() != tun.StateActive {
		// Off: nothing to keep in step. Held: the helper may be gone, and a call to the firewall
		// driver would start it again with a prompt for rights; only the user brings it back.
		return nil
	}
	_, node, link, ok := a.wholeVPN()
	if !ok {
		return nil // the VPN is between two exits: the exceptions wait for the next
	}
	plan, err := a.wholePlan(ctx, node, link)
	if err != nil {
		return err
	}
	w.mu.Lock()
	same := slices.Equal(w.applied, plan.Endpoints)
	w.mu.Unlock()
	if same {
		return nil
	}
	if err := sw.Update(ctx, plan); err != nil {
		return a.wholeFail(err)
	}
	a.wholeApplied(plan)
	return nil
}

// wholeTimer starts the timer that keeps the exceptions in step with the session.
func (a *App) wholeTimer() {
	w := &a.whole
	w.mu.Lock()
	if w.stop != nil {
		w.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	w.stop = stop
	w.mu.Unlock()
	every := a.cfg.WholeComputer.Poll
	if every <= 0 {
		every = 3 * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = a.WholeComputerSync(ctx)
				cancel()
			}
		}
	}()
}

func (a *App) wholeNoTimer() {
	w := &a.whole
	w.mu.Lock()
	stop := w.stop
	w.stop = nil
	w.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// VPNStopForSwitch stops the VPN to start another exit: the mode, if it is on, stays on. Between the
// two the proxy is down and nothing gets out, which is the point.
func (a *App) VPNStopForSwitch() error { return a.vpnStopKeepingMode("switched") }
