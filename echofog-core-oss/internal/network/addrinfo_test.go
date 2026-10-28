package network

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Everything that travels with a node's address — the camouflage SNI, the server role — comes back
// out of every form the address takes: a join link with the peer ID, the "host:port?query" of peer
// exchange and the DHT, a ws:// URL.
func TestParseAddrInfoForms(t *testing.T) {
	var id types.PeerID
	id[0], id[31] = 0x12, 0x34

	cases := []struct {
		in   string
		want AddrInfo
	}{
		{"203.0.113.15:4242/" + id.String() + "?sni=cover.lab&srv=1",
			AddrInfo{Addr: "203.0.113.15:4242", ID: id, Named: true, SNI: "cover.lab", Server: true}},
		{"echofog://relay/203.0.113.15:4242/" + id.String() + "?sni=cover.lab",
			AddrInfo{Addr: "203.0.113.15:4242", ID: id, Named: true, SNI: "cover.lab"}},
		{"203.0.113.15:4242/" + id.String() + "?srv=1",
			AddrInfo{Addr: "203.0.113.15:4242", ID: id, Named: true, Server: true}},
		{"203.0.113.15:4242/" + id.String(),
			AddrInfo{Addr: "203.0.113.15:4242", ID: id, Named: true}},
		// peer exchange and the DHT: no peer ID, a query; the address comes back without it
		{"203.0.113.15:4242?sni=cover.lab&srv=1",
			AddrInfo{Addr: "203.0.113.15:4242", SNI: "cover.lab", Server: true}},
		{"203.0.113.15:4242?sni=cover.lab", AddrInfo{Addr: "203.0.113.15:4242", SNI: "cover.lab"}},
		{"?sni=cover.lab&srv=1", AddrInfo{Addr: "", SNI: "cover.lab", Server: true}},
		{"203.0.113.15:4242", AddrInfo{Addr: "203.0.113.15:4242"}},
		{"wss://vpn.example.org/ws/" + id.String() + "?sni=cover.lab",
			AddrInfo{Addr: "wss://vpn.example.org/ws", ID: id, Named: true, SNI: "cover.lab"}},
		{"wss://vpn.example.org/ws", AddrInfo{Addr: "wss://vpn.example.org/ws"}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ParseAddrInfo(c.in), c.in)
	}
}

// The old function keeps its shape but no longer hands back "host:port?sni=..." as an address.
func TestSplitPeerAddrSNIStripsTheQueryWithoutAnID(t *testing.T) {
	addr, _, sni, ok := SplitPeerAddrSNI("203.0.113.15:4242?sni=cover.lab")
	assert.False(t, ok)
	assert.Equal(t, "203.0.113.15:4242", addr)
	assert.Equal(t, "cover.lab", sni)
}

func TestAnnotateAddr(t *testing.T) {
	assert.Equal(t, "1.2.3.4:4242", AnnotateAddr("1.2.3.4:4242", "", false))
	assert.Equal(t, "1.2.3.4:4242?sni=cover.lab", AnnotateAddr("1.2.3.4:4242", "cover.lab", false))
	assert.Equal(t, "1.2.3.4:4242?srv=1", AnnotateAddr("1.2.3.4:4242", "", true))
	full := AnnotateAddr("1.2.3.4:4242", "cover.lab", true)
	assert.Equal(t, "1.2.3.4:4242?sni=cover.lab&srv=1", full)
	assert.Equal(t, full, AnnotateAddr(full, "cover.lab", true), "an address is annotated once")
	assert.Equal(t, "1.2.3.4:4242?sni=a%26b%3D1", AnnotateAddr("1.2.3.4:4242", "a&b=1", false), "the SNI is escaped")
	assert.Equal(t, "a&b=1", ParseAddrInfo(AnnotateAddr("1.2.3.4:4242", "a&b=1", false)).SNI)
}

func TestJoinPeerAddrInfoRoundTrip(t *testing.T) {
	var id types.PeerID
	id[5] = 9
	for _, c := range []struct {
		sni    string
		server bool
	}{{"", false}, {"cover.lab", false}, {"", true}, {"cover.lab", true}} {
		link := JoinPeerAddrInfo("203.0.113.15:4242", id, c.sni, c.server)
		got := ParseAddrInfo(link)
		require.True(t, got.Named, link)
		assert.Equal(t, AddrInfo{Addr: "203.0.113.15:4242", ID: id, Named: true, SNI: c.sni, Server: c.server}, got, link)
	}
	// the old helper still gives the same SNI form
	assert.Equal(t, JoinPeerAddrInfo("1.2.3.4:1", id, "x.lab", false), JoinPeerAddrWithSNI("1.2.3.4:1", id, "x.lab"))
}
