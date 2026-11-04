//go:build !linux

package udpsock

import (
	"errors"
	"net"
)

// platformCapabilities: GSO exists only on Linux; elsewhere every datagram is one call.
func platformCapabilities() (gso, batchRead bool) { return false, false }

var errNoGSO = errors.New("udpsock: GSO is not available on this platform")

// writeGSO is never called: gso is off from construction.
func (c *Conn) writeGSO([][]byte, *net.UDPAddr, int) error { return errNoGSO }

// gsoSegment: no UDP_SEGMENT message exists on this platform.
func gsoSegment([]byte) int { return 0 }

func dropGSO(dst, oob []byte) []byte { return append(dst, oob...) }

// appendGSOSegment: no UDP_SEGMENT message exists on this platform.
func appendGSOSegment(oob []byte, _ int) []byte { return oob }
