package network

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestSplitPeerAddrTakesTheShareableLink(t *testing.T) {
	var id types.PeerID
	id[0], id[31] = 0x5f, 0x4d
	for _, in := range []string{
		"34.158.189.116:443/" + id.String(),
		"echofog://relay/34.158.189.116:443/" + id.String(),
		"  echofog://relay/34.158.189.116:443/" + id.String() + " ",
	} {
		addr, got, ok := SplitPeerAddr(in)
		if !ok || addr != "34.158.189.116:443" || got != id {
			t.Fatalf("%q -> %q %v %v", in, addr, got, ok)
		}
	}
}
