//go:build linux

package dpi

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// gsoSegment finds the UDP_SEGMENT control message quic-go adds for GSO: the segment size and the
// offset of its value in oob, or 0, -1.
func gsoSegment(oob []byte) (size, at int) {
	size, at = 0, -1
	eachCmsg(oob, func(off int, _ []byte, hdr unix.Cmsghdr, body []byte) {
		if at < 0 && hdr.Level == unix.IPPROTO_UDP && hdr.Type == unix.UDP_SEGMENT && len(body) >= 2 {
			size, at = int(binary.NativeEndian.Uint16(body)), off+unix.CmsgLen(0)
		}
	})
	return size, at
}

// dropGSO appends to dst the control messages of oob without UDP_SEGMENT.
func dropGSO(dst, oob []byte) []byte {
	eachCmsg(oob, func(_ int, msg []byte, hdr unix.Cmsghdr, _ []byte) {
		if hdr.Level != unix.IPPROTO_UDP || hdr.Type != unix.UDP_SEGMENT {
			dst = append(dst, msg...)
		}
	})
	return dst
}

// setGSOSegment rewrites the segment size found by gsoSegment at offset at.
func setGSOSegment(oob []byte, at, size int) {
	binary.NativeEndian.PutUint16(oob[at:], uint16(size))
}
