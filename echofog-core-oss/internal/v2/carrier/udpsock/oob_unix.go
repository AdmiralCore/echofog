//go:build linux || darwin || freebsd

package udpsock

import (
	"net"

	"golang.org/x/sys/unix"
)

// enableECN turns on the control messages that carry the ECN bits. It reports whether the socket
// can deliver them; a kernel or a policy that says no leaves the socket without ECN, which is
// wire-neutral (platforms.md §1.2).
func enableECN(c *net.UDPConn) bool {
	rc, err := c.SyscallConn()
	if err != nil {
		return false
	}
	var ok4, ok6 bool
	if err := rc.Control(func(fd uintptr) {
		ok4 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_RECVTOS, 1) == nil
		ok6 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVTCLASS, 1) == nil
	}); err != nil {
		return false
	}
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP.To4() != nil {
		return ok4
	}
	return ok4 || ok6
}

// eachCmsg calls f with every control message in oob; it stops at the first malformed one, as
// the kernel does.
func eachCmsg(oob []byte, f func(msg []byte, hdr unix.Cmsghdr, body []byte)) {
	for rest := oob; len(rest) >= unix.SizeofCmsghdr; {
		hdr, body, remainder, err := unix.ParseOneSocketControlMessage(rest)
		if err != nil {
			return
		}
		f(rest[:len(rest)-len(remainder)], hdr, body)
		rest = remainder
	}
}

// isECN reports whether a control message carries an ECN mark: the TOS byte on send and receive,
// under either the IPv4 or the IPv6 level.
func isECN(hdr unix.Cmsghdr) bool {
	if hdr.Level == unix.IPPROTO_IP {
		return hdr.Type == unix.IP_TOS || hdr.Type == unix.IP_RECVTOS
	}
	return hdr.Level == unix.IPPROTO_IPV6 && hdr.Type == unix.IPV6_TCLASS
}

// stripECN appends to dst the control messages of oob without the ones that mark ECN: the
// datagram then leaves Not-ECT, as from a sender without ECN (udp=restricted, platforms.md §2).
func stripECN(dst, oob []byte) []byte {
	eachCmsg(oob, func(msg []byte, hdr unix.Cmsghdr, _ []byte) {
		if !isECN(hdr) {
			dst = append(dst, msg...)
		}
	})
	return dst
}

// hasECN reports whether oob asks for an ECN marking.
func hasECN(oob []byte) bool {
	found := false
	eachCmsg(oob, func(_ []byte, hdr unix.Cmsghdr, _ []byte) {
		if isECN(hdr) {
			found = true
		}
	})
	return found
}
