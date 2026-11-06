package noise3

import (
	"errors"
	"net"
	"net/netip"
)

// SetTap makes the listener call f with the source and bytes of every datagram its socket reads,
// before the datagram is processed (nil removes it). The bytes belong to f only for the call. The
// birthday punch watches the listener's socket for the symmetric peer's first datagram with it
// (services.md §6.4.1).
func (l *Listener) SetTap(f func(src netip.AddrPort, d []byte)) {}

// Sibling starts another listener with this one's configuration and current masking keys on conn,
// which it then owns. Its replay filter is in memory. The birthday punch runs the noise3 responder
// and dialer on the one socket that a mapping was found for.
func (l *Listener) Sibling(conn net.PacketConn) (*Listener, error) {
	return nil, errors.New("noise3: not implemented")
}
