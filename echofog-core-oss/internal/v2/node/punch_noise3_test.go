package node_test

import (
	"context"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/punch"
)

func TestNoise3PunchOffersConfirmedIPWithTheListenerPort(t *testing.T) {
	var sk [32]byte
	_, _ = rand.Read(sk[:])
	src := node.Noise3Listener{Addr: "127.0.0.1:0", Masks: []noise3.MaskKey{{Private: sk, NotAfter: time.Now().Add(time.Hour)}}}
	p := &node.Noise3Punch{PublicIP: func() (netip.Addr, bool) { return netip.MustParseAddr("198.51.100.9"), true },
		Card: func(node.DeviceID) *objects.Card { return nil }}
	src.Punch = p
	l, err := src.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint16(l.Addr().(interface{ AddrPort() netip.AddrPort }).AddrPort().Port())
	got := p.Net(node.DeviceID{1}, punch.Initiator).OwnAddrs()
	if len(got) != 1 || got[0] != netip.AddrPortFrom(netip.MustParseAddr("198.51.100.9"), port) {
		t.Fatalf("OwnAddrs %v, want 198.51.100.9:%d", got, port)
	}
	// No confirmed IP: nothing to offer, never a guess.
	p.PublicIP = func() (netip.Addr, bool) { return netip.Addr{}, false }
	if got := p.Net(node.DeviceID{1}, punch.Initiator).OwnAddrs(); len(got) != 0 {
		t.Fatalf("offered %v without a confirmed public IP", got)
	}
	// No card of the peer: the dial fails instead of panicking.
	if _, err := p.Net(node.DeviceID{1}, punch.Initiator).Dial(context.Background(), []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:9")}, 24); err == nil {
		t.Fatal("dial without a peer card")
	}
}

func TestNoise3PunchWithoutListenerIsInert(t *testing.T) {
	p := &node.Noise3Punch{}
	n := p.Net(node.DeviceID{1}, punch.Responder)
	if len(n.OwnAddrs()) != 0 || n.SendJunk(netip.MustParseAddrPort("127.0.0.1:9"), []byte{1}) == nil {
		t.Fatal("a node without a noise3 listener must offer nothing and send nothing")
	}
}
