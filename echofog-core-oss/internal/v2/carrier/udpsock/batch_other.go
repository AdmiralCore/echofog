//go:build !linux

package udpsock

import "golang.org/x/net/ipv4"

// readKernel reads one datagram: this platform has no recvmmsg. batchRead is off from
// construction, so this is reached only through the fallback seam.
func (c *Conn) readKernel(ms []ipv4.Message, _ int) (int, error) {
	return c.readOne(ms)
}
