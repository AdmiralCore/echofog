package network

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"

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
	// the node's socket is dual-stack, and peers are mostly IPv4: IPv4 must take the pin (if
	// only IPv6 did, IPv4 would silently loop into the TUN); IPv6 is best effort
	return bindRaw(raw, ifIndex, true, false)
}

// bindRaw sets IP_UNICAST_IF / IPV6_UNICAST_IF on a socket; need4 and need6 say which family
// must succeed for the socket to count as pinned.
func bindRaw(raw syscall.RawConn, ifIndex int, need4, need6 bool) error {
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
	if need4 && err4 != nil {
		return err4
	}
	if need6 && err6 != nil {
		return err6
	}
	if err4 != nil && err6 != nil {
		return errors.Join(err4, err6)
	}
	return nil
}

// bindDialSocket pins a socket the node is about to connect with, for its address family.
func bindDialSocket(network string, raw syscall.RawConn, ifIndex int) error {
	switch network[len(network)-1] {
	case '4':
		return bindRaw(raw, ifIndex, true, false)
	case '6':
		return bindRaw(raw, ifIndex, false, true)
	default:
		return bindRaw(raw, ifIndex, false, false)
	}
}
