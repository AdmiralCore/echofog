package exit_gateway

import (
	"errors"
	"syscall"
)

// udpReadTransient: Windows reports an ICMP port-unreachable for an earlier datagram as an error
// on the next read (WSAECONNRESET). The socket is fine; ending the association for it would cut
// every flow on it because one destination is closed.
func udpReadTransient(err error) bool {
	return errors.Is(err, syscall.Errno(10054)) // WSAECONNRESET
}
