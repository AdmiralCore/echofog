package network

import (
	"encoding/binary"
	"errors"
	"net"

	"golang.org/x/sys/windows"
)

const (
	ipUnicastIf   = 31 // IP_UNICAST_IF
	ipv6UnicastIf = 31 // IPV6_UNICAST_IF
)

// bindUDPToInterface pins outgoing datagrams to one interface regardless of the routing table,
// so the node's own traffic does not loop back into the TUN that captures everything else.
// ifIndex 0 removes the pin.
func bindUDPToInterface(c *net.UDPConn, ifIndex int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var v4be [4]byte // IPv4 wants the index in network byte order, IPv6 in host order
	binary.BigEndian.PutUint32(v4be[:], uint32(ifIndex))
	v4 := int(binary.LittleEndian.Uint32(v4be[:]))
	var err4, err6 error
	ctlErr := raw.Control(func(fd uintptr) {
		h := windows.Handle(fd)
		err4 = windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIf, v4)
		err6 = windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIf, ifIndex)
	})
	if ctlErr != nil {
		return ctlErr
	}
	if err4 != nil && err6 != nil {
		return errors.Join(err4, err6)
	}
	return nil
}
