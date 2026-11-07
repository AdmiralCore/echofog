package node

import (
	"net"
	"net/netip"
)

// SetNetworkID tells the runtime which network the client is on now. A change forgets what was
// learned about the old one: a freeze belongs to a network, not to the device.
func (r *Runtime) SetNetworkID(id string) {}

// NetworkID returns the identifier of the current network.
func (r *Runtime) NetworkID() string { return "" }

func (r *Runtime) noteStall(ip netip.Addr, bytes int) bool { return false }

func (r *Runtime) frozen() bool { return false }

// freezeConn watches a TCP flow to a foreign address for the freeze.
func (r *Runtime) freezeConn(c net.Conn, ip netip.Addr) net.Conn { return c }
