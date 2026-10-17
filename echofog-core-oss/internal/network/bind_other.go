//go:build !windows

package network

import (
	"net"
	"syscall"
)

// bindUDPToInterface is a no-op off Windows: on Linux the TUN helper adds a policy-routing rule
// for the node's UDP port instead (binding to a device would need CAP_NET_RAW).
func bindUDPToInterface(c *net.UDPConn, ifIndex int) error { return nil }

func bindDialSocket(string, syscall.RawConn, int) error { return nil }
