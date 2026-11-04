//go:build !linux && !darwin && !freebsd

package udpsock

import "net"

// enableECN: this platform has no control messages, so ECN is not set (platforms.md §1.2).
func enableECN(*net.UDPConn) bool { return false }

// stripECN: no control messages to strip.
func stripECN(dst, oob []byte) []byte { return append(dst, oob...) }

// hasECN: no control messages, so no mark.
func hasECN([]byte) bool { return false }
