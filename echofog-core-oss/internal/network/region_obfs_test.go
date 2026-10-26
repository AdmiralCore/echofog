package network

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedAddr mirrors how ExpectPeer{,SNI} key the obfuscator: by the resolved UDP address.
func resolvedAddr(t *testing.T, addr string) net.Addr {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp", addr)
	require.NoError(t, err)
	return ua
}

// A peer with a camouflage SNI is dialed over REALITY (raw) by default, and over noise once the
// region policy places it in this node's own region.
func TestRegionNoise_DomesticPeerUsesNoiseCrossBorderKeepsReality(t *testing.T) {
	const sni = "www.example.com"

	t.Run("cross-border keeps REALITY", func(t *testing.T) {
		client := newObfsNode(t, false, "")
		peer := newObfsNode(t, false, "")
		client.tr.ExpectPeerSNI(peer.addr(), peer.id, sni)

		got, ok := client.tr.book.getSNI(peer.addr())
		assert.True(t, ok && got == sni, "a cross-border peer keeps its camouflage SNI")
		assert.True(t, client.tr.obfs.IsRaw(resolvedAddr(t, peer.addr())), "REALITY sends the peer's packets raw")
	})

	t.Run("same region uses noise", func(t *testing.T) {
		client := newObfsNode(t, false, "")
		peer := newObfsNode(t, false, "")
		client.tr.SetRegionNoise(func(addr string) bool { return true }) // every peer is domestic

		client.tr.ExpectPeerSNI(peer.addr(), peer.id, sni)

		_, ok := client.tr.book.getSNI(peer.addr())
		assert.False(t, ok, "a same-region peer drops the SNI and is keyed as a plain noise peer")
		assert.False(t, client.tr.obfs.IsRaw(resolvedAddr(t, peer.addr())), "a domestic peer is obfuscated as noise, not sent raw")
	})

	t.Run("policy decides per address", func(t *testing.T) {
		client := newObfsNode(t, false, "")
		domestic := newObfsNode(t, false, "")
		foreign := newObfsNode(t, false, "")
		client.tr.SetRegionNoise(func(addr string) bool { return addr == domestic.addr() })

		client.tr.ExpectPeerSNI(domestic.addr(), domestic.id, sni)
		client.tr.ExpectPeerSNI(foreign.addr(), foreign.id, sni)

		assert.False(t, client.tr.obfs.IsRaw(resolvedAddr(t, domestic.addr())), "the domestic peer uses noise")
		assert.True(t, client.tr.obfs.IsRaw(resolvedAddr(t, foreign.addr())), "the foreign peer still uses REALITY")
	})
}
