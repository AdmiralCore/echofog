package scenarios

import (
	"net/netip"
	"sync"
)

// NATType is the behaviour of a NAT towards UDP (RFC 4787): how it maps a socket's port and whom it
// lets in.
type NATType int

const (
	// FullCone: one external port per socket, anybody may write to it once it is mapped.
	FullCone NATType = iota
	// AddressRestricted: one external port per socket; only addresses the socket has written to.
	AddressRestricted
	// PortRestricted: one external port per socket; only address and port pairs it has written to.
	PortRestricted
	// Symmetric: a new external port for every destination, open to that destination alone.
	Symmetric
)

func (t NATType) String() string {
	switch t {
	case FullCone:
		return "full-cone"
	case AddressRestricted:
		return "address-restricted"
	case PortRestricted:
		return "port-restricted"
	case Symmetric:
		return "symmetric"
	}
	return "?"
}

// natBox is the NAT of one node's noise3 socket. It is physics, not product code.
type natBox struct {
	typ NATType
	ext netip.Addr

	mu      sync.Mutex
	next    uint16
	fixed   uint16                             // the one external port of a cone
	bydest  map[netip.AddrPort]uint16          // symmetric: a port per destination
	allowed map[uint16]map[netip.AddrPort]bool // external port: whom the socket has written to from it
}

func newNATBox(typ NATType, ext netip.Addr, internal uint16) *natBox {
	return &natBox{typ: typ, ext: ext, next: 30000 + internal%1000,
		bydest: map[netip.AddrPort]uint16{}, allowed: map[uint16]map[netip.AddrPort]bool{}}
}

func (n *natBox) port(to netip.AddrPort) uint16 {
	if n.typ != Symmetric {
		if n.fixed == 0 {
			n.fixed = n.next
			n.next++
		}
		return n.fixed
	}
	if p, ok := n.bydest[to]; ok {
		return p
	}
	p := n.next
	n.next += 7 // not a counter a peer could guess
	n.bydest[to] = p
	return p
}

// Send is a datagram from the node's socket to a destination: it returns the source address the
// destination sees, and remembers whom the socket wrote to.
func (n *natBox) Send(to netip.AddrPort) netip.AddrPort {
	n.mu.Lock()
	defer n.mu.Unlock()
	p := n.port(to)
	if n.allowed[p] == nil {
		n.allowed[p] = map[netip.AddrPort]bool{}
	}
	n.allowed[p][to] = true
	return netip.AddrPortFrom(n.ext, p)
}

// Accepts reports whether a datagram from the given source to the external port gets through.
func (n *natBox) Accepts(from netip.AddrPort, extPort uint16) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	remotes, ok := n.allowed[extPort]
	if !ok {
		return false
	}
	switch n.typ {
	case FullCone:
		return true
	case AddressRestricted:
		for r := range remotes {
			if r.Addr() == from.Addr() {
				return true
			}
		}
		return false
	}
	return remotes[from]
}
