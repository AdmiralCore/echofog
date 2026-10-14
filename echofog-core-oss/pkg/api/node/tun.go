package node

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/echofog/echofog-core/internal/network/fakedns"
	"github.com/echofog/echofog-core/internal/network/tunmode"
)

// tunState is whole-computer mode: every application's traffic, UDP and DNS included, goes
// through the running proxy via an elevated TUN helper.
type tunState struct {
	mu      sync.Mutex // serialises enable/disable; held across the administrator prompt
	ctl     tunmode.Controller
	lastErr string
}

// WholeComputerSupported reports whether this OS can route all traffic through EchoFog.
func (n *NodeApi) WholeComputerSupported() bool { return tunmode.Supported() }

// SetWholeComputer turns whole-computer mode on or off. Turning it on asks for administrator
// rights (UAC on Windows, polkit on Linux) and requires the proxy to be running.
func (n *NodeApi) SetWholeComputer(on bool) error {
	if n == nil || n.node == nil || n.node.Transport == nil {
		return errors.New("node is not running")
	}
	n.tun.mu.Lock()
	defer n.tun.mu.Unlock()
	if !on {
		n.disableTunLocked()
		return nil
	}
	if n.tun.ctl.Running() {
		return nil
	}

	n.vpn.mu.Lock()
	srv := n.vpn.server
	n.vpn.mu.Unlock()
	if srv == nil || srv.Addr() == nil {
		return errors.New("connect to an exit first")
	}
	socks := srv.Addr().String()
	if host, _, _ := net.SplitHostPort(socks); host != "127.0.0.1" {
		return errors.New("the proxy must listen on 127.0.0.1")
	}

	// Read the physical interface before the TUN takes over the routes.
	ifIndex, err := tunmode.DefaultInterfaceIndex()
	if err != nil {
		return err
	}
	if err := n.node.Transport.BindToInterface(ifIndex); err != nil {
		// without the pin the node's own packets would loop through the TUN
		return errors.New("cannot keep EchoFog's own traffic off the tunnel: " + err.Error())
	}
	srv.SetFakeDNS(fakedns.New())

	n.tun.ctl.SetOnExit(func(err error) {
		n.tun.mu.Lock()
		n.tun.lastErr = err.Error()
		n.tun.mu.Unlock()
		n.unwindTun()
		n.emit("vpn:whole_computer", map[string]any{"on": false, "error": err.Error()})
	})
	err = n.tun.ctl.Start(context.Background(), socks, n.node.Transport.UDPPort())
	if err != nil {
		n.unwindTun()
		n.tun.lastErr = err.Error()
		return err
	}
	n.tun.lastErr = ""
	n.emit("vpn:whole_computer", map[string]any{"on": true})
	return nil
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
