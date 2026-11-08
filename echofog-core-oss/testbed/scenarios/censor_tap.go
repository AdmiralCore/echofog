package scenarios

import (
	"net/netip"
)

// fakeClientIP is the address every tapped client has in the pcap.
var fakeClientIP = netip.MustParseAddr("10.0.0.2")

// TCPTap is a TCP relay between a client and a server on loopback that writes the pcap of what it
// relays, with a fake client and a fake server address.
type TCPTap struct{}

// NewTCPTap listens on a loopback port and relays every connection to target.
func NewTCPTap(target string, fakeServer netip.AddrPort, pw *PcapWriter) (*TCPTap, error) {
	return nil, errNotWritten
}

// Addr is the loopback address clients dial.
func (t *TCPTap) Addr() string { return "" }

// Close stops the relay and ends the connections it holds.
func (t *TCPTap) Close() error { return nil }

// UDPTap is the UDP counterpart: one upstream socket per client address.
type UDPTap struct{}

// NewUDPTap listens on a loopback UDP port and relays datagrams to target.
func NewUDPTap(target string, fakeServer netip.AddrPort, pw *PcapWriter) (*UDPTap, error) {
	return nil, errNotWritten
}

// Addr is the loopback address clients dial.
func (t *UDPTap) Addr() string { return "" }

// FirstClientDatagram is the first datagram a client sent through the tap (a legitimate first
// flight, which the prober replays).
func (t *UDPTap) FirstClientDatagram() []byte { return nil }

// Close stops the relay.
func (t *UDPTap) Close() error { return nil }
