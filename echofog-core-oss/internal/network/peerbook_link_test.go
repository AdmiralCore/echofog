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

func TestSplitPeerAddrWithSNI(t *testing.T) {
	var id types.PeerID
	id[0], id[31] = 0x12, 0x34
	in := "203.0.113.15:4242/" + id.String() + "?sni=cover.lab"
	addr, gotID, sni, ok := SplitPeerAddrSNI(in)
	if !ok || addr != "203.0.113.15:4242" || gotID != id || sni != "cover.lab" {
		t.Fatalf("SplitPeerAddrSNI failed: addr=%q id=%v sni=%q ok=%v", addr, gotID, sni, ok)
	}

	joined := JoinPeerAddrWithSNI("203.0.113.15:4242", id, "cover.lab")
	addr2, gotID2, sni2, ok2 := SplitPeerAddrSNI(joined)
	if !ok2 || addr2 != "203.0.113.15:4242" || gotID2 != id || sni2 != "cover.lab" {
		t.Fatalf("JoinPeerAddrWithSNI roundtrip failed: %q -> %q %v %q", joined, addr2, gotID2, sni2)
	}
}

func TestPeerBookSNIRetrieval(t *testing.T) {
	var b peerBook
	var id types.PeerID
	id[0] = 0xab
	b.setSNI("1.2.3.4:443", id, "example.org")

	sni, ok := b.getSNI("1.2.3.4:443")
	if !ok || sni != "example.org" {
		t.Fatalf("getSNI by addr failed: %q %v", sni, ok)
	}
	sniID, okID := b.getSNIByID(id)
	if !okID || sniID != "example.org" {
		t.Fatalf("getSNIByID failed: %q %v", sniID, okID)
	}
}
