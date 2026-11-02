package network

import (
	"net"
	"testing"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/stretchr/testify/require"
)

// Whatever stack is under it (STUN multiplexer or not, UDP noise or not), quic-go gets an
// OOB-capable connection with batch reads, so it keeps GSO, ECN and recvmmsg on Linux. Before,
// every wrapper hid these and quic-go fell back to one packet per syscall.
func TestQUICTransportConnIsOOBCapable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		obfs, direct bool
	}{{"plain", false, false}, {"direct", false, true}, {"noise", true, false}, {"noise-direct", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			n := perfNode(t, tc.obfs, tc.direct)
			c := n.tr.tr.Conn
			_, ok := c.(quic.OOBCapablePacketConn)
			require.True(t, ok, "%T is not OOB-capable", c)
			_, ok = c.(dpi.BatchReader)
			require.True(t, ok, "%T has no ReadBatch: quic-go would read the socket past the wrappers", c)
		})
	}
}

// A REALITY connection over something without control messages stays a plain PacketConn.
func TestQuicRealityConnWithoutOOBBase(t *testing.T) {
	type plain struct{ net.PacketConn }
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer u.Close()
	c := NewQuicRealityConn(plain{u}, nil)
	_, ok := c.PacketConn().(quic.OOBCapablePacketConn)
	require.False(t, ok)
}
