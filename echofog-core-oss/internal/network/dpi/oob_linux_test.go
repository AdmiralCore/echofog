//go:build linux

package dpi

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// ecnMarked reports whether received control messages carry ECT(0), ECT(1) or CE.
func ecnMarked(oob []byte) bool {
	marked := false
	eachCmsg(oob, func(_ int, _ []byte, hdr unix.Cmsghdr, body []byte) {
		switch {
		case hdr.Level == unix.IPPROTO_IP && hdr.Type == unix.IP_TOS && len(body) >= 1:
			marked = marked || body[0]&3 != 0
		case hdr.Level == unix.IPPROTO_IPV6 && hdr.Type == unix.IPV6_TCLASS && len(body) >= 4:
			marked = marked || binary.NativeEndian.Uint32(body)&3 != 0
		}
	})
	return marked
}
