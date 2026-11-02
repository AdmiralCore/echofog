//go:build linux || darwin || freebsd

package dpi

import "golang.org/x/sys/unix"

// eachCmsg calls f with every control message in oob (whole message, header and body) and the
// offset of the message; it stops at the first malformed one.
func eachCmsg(oob []byte, f func(off int, msg []byte, hdr unix.Cmsghdr, body []byte)) {
	off := 0
	for rest := oob; len(rest) >= unix.SizeofCmsghdr; {
		hdr, body, remainder, err := unix.ParseOneSocketControlMessage(rest)
		if err != nil {
			return
		}
		l := len(rest) - len(remainder)
		f(off, rest[:l], hdr, body)
		off += l
		rest = remainder
	}
}

// StripECN appends to dst the control messages of oob without the ones that mark ECN (IP_TOS,
// IPV6_TCLASS): the packet then leaves Not-ECT, as from a sender without ECN.
func StripECN(dst, oob []byte) []byte {
	eachCmsg(oob, func(_ int, msg []byte, hdr unix.Cmsghdr, _ []byte) {
		switch {
		case hdr.Level == unix.IPPROTO_IP && (hdr.Type == unix.IP_TOS || hdr.Type == unix.IP_RECVTOS):
		case hdr.Level == unix.IPPROTO_IPV6 && hdr.Type == unix.IPV6_TCLASS:
		default:
			dst = append(dst, msg...)
		}
	})
	return dst
}

// HasECNMark reports whether oob asks for an ECN marking.
func HasECNMark(oob []byte) bool {
	found := false
	eachCmsg(oob, func(_ int, _ []byte, hdr unix.Cmsghdr, _ []byte) {
		if (hdr.Level == unix.IPPROTO_IP && (hdr.Type == unix.IP_TOS || hdr.Type == unix.IP_RECVTOS)) ||
			(hdr.Level == unix.IPPROTO_IPV6 && hdr.Type == unix.IPV6_TCLASS) {
			found = true
		}
	})
	return found
}
