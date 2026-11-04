//go:build linux

package udpsock

import (
	"encoding/binary"
	"net"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// platformCapabilities: the Linux kernel and the Android kernels under the same tag offer GSO
// (UDP_SEGMENT) and recvmmsg. Whether a particular kernel or SELinux policy allows them is
// learned from the first refusal (doc.go), not guessed here.
func platformCapabilities() (gso, batchRead bool) { return true, true }

// trainPool holds the concatenated buffers of GSO trains. A train lives for one sendmsg.
var trainPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64*1500)
		return &b
	},
}

// writeGSO sends bufs as one sendmsg cut by the kernel at seg-byte segments.
func (c *Conn) writeGSO(bufs [][]byte, addr *net.UDPAddr, seg int) error {
	total := 0
	for _, p := range bufs {
		total += len(p)
	}
	buf := trainPool.Get().(*[]byte)
	if cap(*buf) < total {
		*buf = make([]byte, total)
	}
	train := (*buf)[:0]
	for _, p := range bufs {
		train = append(train, p...)
	}
	var oob [64]byte // room for one UDP_SEGMENT message (24 bytes)
	_, _, err := c.writeMsg(train, appendGSOSegment(oob[:0], seg), addr)
	trainPool.Put(buf)
	return err
}

// gsoSegment returns the UDP_SEGMENT size of oob, or 0 when there is no such message.
func gsoSegment(oob []byte) int {
	seg := 0
	eachCmsg(oob, func(_ []byte, hdr unix.Cmsghdr, body []byte) {
		if seg == 0 && hdr.Level == unix.IPPROTO_UDP && hdr.Type == unix.UDP_SEGMENT && len(body) >= 2 {
			seg = int(binary.NativeEndian.Uint16(body))
		}
	})
	return seg
}

// dropGSO appends to dst the control messages of oob without UDP_SEGMENT.
func dropGSO(dst, oob []byte) []byte {
	eachCmsg(oob, func(msg []byte, hdr unix.Cmsghdr, _ []byte) {
		if hdr.Level != unix.IPPROTO_UDP || hdr.Type != unix.UDP_SEGMENT {
			dst = append(dst, msg...)
		}
	})
	return dst
}

// appendGSOSegment appends a UDP_SEGMENT control message of size seg to oob.
func appendGSOSegment(oob []byte, seg int) []byte {
	const dataLen = 2 // a uint16 segment size
	start := len(oob)
	oob = append(oob, make([]byte, unix.CmsgSpace(dataLen))...)
	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[start]))
	h.Level = unix.IPPROTO_UDP
	h.Type = unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(dataLen))
	offset := start + unix.CmsgSpace(0)
	binary.NativeEndian.PutUint16(oob[offset:], uint16(seg))
	return oob
}
