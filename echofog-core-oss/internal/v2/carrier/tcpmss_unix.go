//go:build linux || darwin || freebsd

package carrier

import "golang.org/x/sys/unix"

// mssSupported: this OS lets a socket limit its MSS before connect (TCP_MAXSEG).
const mssSupported = true

func setTCPMSS(fd uintptr, mss int) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG, mss)
}
