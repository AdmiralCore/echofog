package scenarios

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

var (
	relayAddr = netip.MustParseAddrPort("198.51.100.1:443")
	r1p       = netip.MustParseAddrPort("198.51.100.77:5000")
	r1q       = netip.MustParseAddrPort("198.51.100.77:5001")
	r2p       = netip.MustParseAddrPort("198.51.100.88:5000")
)

// The NAT model is physics, not product: what a mapping and a filter do to a datagram.
func TestAConeKeepsOneExternalPortForEveryDestinationASymmetricNatDoesNot(t *testing.T) {
	ext := netip.MustParseAddr("203.0.113.9")
	for _, typ := range []NATType{FullCone, AddressRestricted, PortRestricted} {
		n := newNATBox(typ, ext, 4433)
		assert.Equal(t, n.Send(r1p), n.Send(r2p), "%s: one mapping for the socket", typ)
		assert.Equal(t, ext, n.Send(r1p).Addr())
	}
	s := newNATBox(Symmetric, ext, 4433)
	assert.NotEqual(t, s.Send(r1p), s.Send(r2p), "a symmetric NAT draws a new port for every destination")
	assert.Equal(t, s.Send(r1p), s.Send(r1p), "and the same one for the same destination")
}

func TestTheFilterOfEachNATType(t *testing.T) {
	ext := netip.MustParseAddr("203.0.113.9")
	cases := []struct {
		typ                         NATType
		sameAddrPort, sameAddrOther bool
		otherAddr                   bool
	}{
		{FullCone, true, true, true},
		{AddressRestricted, true, true, false},
		{PortRestricted, true, false, false},
		{Symmetric, true, false, false},
	}
	for _, c := range cases {
		n := newNATBox(c.typ, ext, 4433)
		src := n.Send(r1p) // the node wrote to r1p
		assert.Equal(t, c.sameAddrPort, n.Accepts(r1p, src.Port()), "%s: from the address and port it wrote to", c.typ)
		assert.Equal(t, c.sameAddrOther, n.Accepts(r1q, src.Port()), "%s: from the same address, another port", c.typ)
		assert.Equal(t, c.otherAddr, n.Accepts(r2p, src.Port()), "%s: from an address it never wrote to", c.typ)
		assert.False(t, n.Accepts(r1p, src.Port()+1), "%s: a port nobody mapped", c.typ)
	}
}

func TestASymmetricNatAcceptsOnlyOnThePortMappedForThatDestination(t *testing.T) {
	n := newNATBox(Symmetric, netip.MustParseAddr("203.0.113.9"), 4433)
	toRelay := n.Send(relayAddr)
	toPeer := n.Send(r1p)
	assert.True(t, n.Accepts(relayAddr, toRelay.Port()))
	assert.False(t, n.Accepts(r1p, toRelay.Port()), "the peer cannot reach the port the relay was given")
	assert.True(t, n.Accepts(r1p, toPeer.Port()))
}
