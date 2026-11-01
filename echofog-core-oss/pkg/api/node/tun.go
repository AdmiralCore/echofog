package node

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/fakedns"
	"github.com/echofog/echofog-core/internal/network/tunmode"
)

// ifaceCheckEvery is how often whole-computer mode checks that the network it is pinned to is
// still there (Wi-Fi switched, cable pulled, resumed on another network).
const ifaceCheckEvery = 3 * time.Second

// tunState is whole-computer mode: every application's traffic, UDP and DNS included, goes
// through the running proxy via an elevated TUN helper.
type tunState struct {
	mu      sync.Mutex // serialises enable/disable; held across the administrator prompt
	ctl     tunmode.Controller
	lastErr string
	// fake is kept for the life of the node: browsers cache the fake IPs they were given, and a
	// fresh mapper on every enable would hand the same IPs to other names
	fake *fakedns.Mapper

	watchMu   sync.Mutex // unwindTun also runs from the helper's exit callback, outside mu
	stopWatch context.CancelFunc

	// killSwitch: nothing gets out around the exit while whole-computer mode is on — not when
	// the exit drops, the app is switching exits, the network changes or the app crashes (the
	// helper then holds the traffic until the app comes back; see tunmode/hold.go). On by default.
	killSwitch atomic.Bool
	holdFile   string // where a holding helper says how to reach it; "" the default (tests set it)
}

func (n *NodeApi) holdPath() string {
	if n.tun.holdFile != "" {
		return n.tun.holdFile
	}
	return tunmode.DefaultHoldFile()
}

// KillSwitchHeld reports whether a helper from an earlier run holds the computer's traffic: the
// app went away without turning whole-computer mode off, and with the kill switch on nothing gets
// out until it reconnects or the user lets the traffic go (ReleaseKillSwitch). Works before login.
func KillSwitchHeld() bool { return tunmode.Held(tunmode.DefaultHoldFile()) }

// ReleaseKillSwitch lets the computer back on its own network after a held kill switch.
func ReleaseKillSwitch() error {
	if err := tunmode.ReleaseHold(tunmode.DefaultHoldFile()); err != nil {
		return err
	}
	network.SetDirectInterface(0)
	return nil
}

// KillSwitchOn reports whether the kill switch is on.
func (n *NodeApi) KillSwitchOn() bool { return n.tun.killSwitch.Load() }

// SetKillSwitch saves the kill switch and applies it at once, to a running TUN too.
func (n *NodeApi) SetKillSwitch(on bool) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node is not running")
	}
	settingsMu.Lock()
	err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.KillSwitch = &on })
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	n.tun.killSwitch.Store(on)
	if n.tun.ctl.Running() {
		_ = n.tun.ctl.SetHold(on)
	}
	return nil
}

// pinHeldNode keeps the node's own traffic off a TUN that a helper from an earlier run still
// holds: it would otherwise go into the blocked tunnel and never reach an exit to adopt it with.
// It runs before the node starts; adopting pins the transport itself.
func (n *NodeApi) pinHeldNode() int {
	if !tunmode.Held(n.holdPath()) {
		return 0
	}
	idx, err := tunmode.PhysicalInterfaceIndex()
	if err != nil || idx == 0 {
		return 0
	}
	network.SetDirectInterface(idx)
	return idx
}

// adoptHeldTun turns whole-computer mode back on through the helper that held the traffic, once
// the exit serves us again: the kill switch then lets the traffic flow without a new prompt.
func (n *NodeApi) adoptHeldTun() {
	if n.tun.ctl.Running() || !tunmode.Held(n.holdPath()) {
		return
	}
	if err := n.SetWholeComputer(true); err != nil {
		slog.Warn("kill switch: could not take the held tunnel over", "err", err)
	}
}

// WholeComputerSupported reports whether this OS can route all traffic through EchoFog.
func (n *NodeApi) WholeComputerSupported() bool { return tunmode.Supported() }

// SetWholeComputer turns whole-computer mode on or off. Turning it on asks for administrator
// rights (UAC on Windows, polkit on Linux) and requires the proxy to be running.
func (n *NodeApi) SetWholeComputer(on bool) error {
	if n == nil {
		return errors.New("node is not running")
	}
	if !on {
		// off must always reach the helper (a node half stopped included): a kill switch that
		// missed the STOP would hold the computer's traffic after a deliberate stop
		n.tun.mu.Lock()
		n.disableTunLocked()
		n.tun.mu.Unlock()
		return nil
	}
	if n.node == nil || n.node.Transport == nil {
		return errors.New("node is not running")
	}
	n.tun.mu.Lock()
	defer n.tun.mu.Unlock()
	if n.tun.ctl.Running() {
		return nil
	}
	if n.node.ProxyOn() {
		// the proxy's own connections (tor) would go into the TUN, and the tunnel goes out through
		// the proxy: a loop that takes the computer offline
		return errors.New("whole-computer mode does not work while the app goes through a proxy (Tor): turn the proxy off in Settings, or set it per app")
	}

	n.vpn.mu.Lock()
	srv, relay, state, notice := n.vpn.server, n.vpn.relayPeer, n.vpn.exitState, n.vpn.exitNotice
	n.vpn.mu.Unlock()
	if srv == nil || srv.Addr() == nil {
		return errors.New("connect to an exit first")
	}
	// with the TUN up every app depends on the exit: an exit that is not connected, or does not
	// serve us yet, would take the whole computer offline
	if !n.relayConnected(relay) {
		return errors.New("the exit is not connected yet")
	}
	if state != ExitOK {
		if notice == "" {
			notice = "the exit has not confirmed yet that it serves you"
		}
		return errors.New(notice)
	}
	socks := srv.Addr().String()
	if host, _, _ := net.SplitHostPort(socks); host != "127.0.0.1" {
		return errors.New("the proxy must listen on 127.0.0.1")
	}

	// Read the physical interface before the TUN takes over the routes; a helper that holds them
	// from an earlier run (the kill switch) has them already, so look past its adapter.
	held := tunmode.Held(n.holdPath())
	var ifIndex int
	var err error
	if held {
		ifIndex, err = tunmode.PhysicalInterfaceIndex()
	} else {
		ifIndex, err = tunmode.DefaultInterfaceIndex()
	}
	if err != nil {
		return err
	}
	if err := n.node.Transport.BindToInterface(ifIndex); err != nil {
		// without the pin the node's own packets would loop through the TUN
		return errors.New("cannot keep EchoFog's own traffic off the tunnel: " + err.Error())
	}
	network.SetDirectInterface(ifIndex) // fallback, WebSocket, DNS and wallet calls too
	if n.tun.fake == nil {
		n.tun.fake = fakedns.New()
	}
	srv.SetFakeDNS(n.tun.fake)

	n.tun.ctl.SetOnExit(func(err error) {
		n.tun.mu.Lock()
		n.tun.lastErr = err.Error()
		n.tun.mu.Unlock()
		n.unwindTun()
		n.emit("vpn:whole_computer", map[string]any{"on": false, "error": err.Error()})
	})
	if held {
		err = n.tun.ctl.Adopt(n.holdPath(), n.node.Transport.UDPPort())
	} else {
		err = n.tun.ctl.Start(context.Background(), socks, n.node.Transport.UDPPort(), n.holdPath())
	}
	if err != nil {
		n.unwindTun()
		n.tun.lastErr = err.Error()
		return err
	}
	_ = n.tun.ctl.SetHold(n.tun.killSwitch.Load())
	n.tun.lastErr = ""
	n.watchInterface(ifIndex)
	n.emit("vpn:whole_computer", map[string]any{"on": true})
	return nil
}

// watchInterface turns whole-computer mode off when the network the node is pinned to goes
// away: the node's own traffic would be stuck on a dead interface while the TUN keeps capturing
// everyone else's, leaving the computer offline until someone noticed the toggle.
func (n *NodeApi) watchInterface(ifIndex int) {
	ctx, cancel := context.WithCancel(context.Background())
	n.tun.watchMu.Lock()
	n.tun.stopWatch = cancel
	n.tun.watchMu.Unlock()
	go func() {
		t := time.NewTicker(ifaceCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if interfaceUsable(ifIndex) {
				continue
			}
			n.tun.mu.Lock()
			if ctx.Err() != nil {
				n.tun.mu.Unlock()
				return
			}
			if !n.tun.killSwitch.Load() {
				n.tun.lastErr = "the network changed; turn whole-computer mode on again"
				n.disableTunLocked()
				n.tun.mu.Unlock()
				return
			}
			// kill switch: the TUN stays (nothing gets out around it); the node follows the
			// network to the interface the computer uses now, or waits blocked for one
			if idx, err := tunmode.PhysicalInterfaceIndex(); err == nil && idx != ifIndex && interfaceUsable(idx) {
				if n.node.Transport.BindToInterface(idx) == nil {
					network.SetDirectInterface(idx)
					slog.Info("kill switch: the network changed, following it", "from", ifIndex, "to", idx)
					ifIndex = idx
					n.tun.lastErr = ""
				}
			} else if n.tun.lastErr == "" {
				n.tun.lastErr = "no network: the internet stays blocked until one is back (kill switch)"
				n.emit("vpn:whole_computer", map[string]any{"on": true, "error": n.tun.lastErr})
			}
			n.tun.mu.Unlock()
		}
	}()
}

// interfaceUsable: the interface is up and still has an address to send from.
func interfaceUsable(ifIndex int) bool {
	ifc, err := net.InterfaceByIndex(ifIndex)
	if err != nil || ifc.Flags&net.FlagUp == 0 {
		return false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func (n *NodeApi) disableTunLocked() {
	if !n.tun.ctl.Running() {
		return
	}
	n.tun.ctl.SetOnExit(nil)
	n.tun.ctl.Stop()
	n.unwindTun()
	n.emit("vpn:whole_computer", map[string]any{"on": false})
}

// unwindTun undoes the node-side half: socket pin and fake DNS.
func (n *NodeApi) unwindTun() {
	n.tun.watchMu.Lock()
	if n.tun.stopWatch != nil {
		n.tun.stopWatch()
		n.tun.stopWatch = nil
	}
	n.tun.watchMu.Unlock()
	network.SetDirectInterface(0)
	if n.node != nil && n.node.Transport != nil {
		_ = n.node.Transport.BindToInterface(0)
	}
	n.vpn.mu.Lock()
	if n.vpn.server != nil {
		n.vpn.server.SetFakeDNS(nil)
	}
	n.vpn.mu.Unlock()
}

func (n *NodeApi) wholeComputerStatus() (on bool, lastErr string) {
	// TryLock: while the administrator prompt is open the status must not block the UI.
	if !n.tun.mu.TryLock() {
		return false, ""
	}
	defer n.tun.mu.Unlock()
	return n.tun.ctl.Running(), n.tun.lastErr
}

func (n *NodeApi) emit(topic string, payload any) {
	if n != nil && n.node != nil && n.node.EventBus != nil {
		n.node.EventBus.Emit(topic, payload)
	}
}

// TunHelperFlag as os.Args[1] means the process was started as the elevated TUN helper; the
// executable must then call RunTunHelper before anything else and exit with its result.
const TunHelperFlag = tunmode.HelperFlag

// RunTunHelper runs the TUN helper with the arguments that follow TunHelperFlag.
func RunTunHelper(args []string) int { return tunmode.HelperMain(args) }
