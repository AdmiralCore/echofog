//go:build linux

package udpsock

import "golang.org/x/net/ipv4"

// readKernel is the batch read: one recvmmsg for as many datagrams as the caller's messages
// allow.
func (c *Conn) readKernel(ms []ipv4.Message, flags int) (int, error) {
	return c.pc.ReadBatch(ms, flags)
}
