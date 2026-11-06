package noise3

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func TestTapSeesEveryDatagram(t *testing.T) {
	l, _, _ := startNoise3(t)
	got := make(chan netip.AddrPort, 4)
	l.SetTap(func(src netip.AddrPort, d []byte) {
		if len(d) == 5 {
			got <- src
		}
	})
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteToUDPAddrPort([]byte("hello"), l.Addr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatal(err)
	}
	select {
	case src := <-got:
		if src != c.LocalAddr().(*net.UDPAddr).AddrPort() {
			t.Fatalf("tap saw %v", src)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tap saw nothing")
	}
}

// A sibling on another socket dials a real listener and accepts a dial itself.
func TestSiblingDialsAndAccepts(t *testing.T) {
	a, _, _ := startNoise3(t)
	b, card, addr := startNoise3(t)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	sa, err := a.Sibling(conn)
	if err != nil {
		t.Fatalf("Sibling: %v", err)
	}
	defer sa.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acc := make(chan error, 1)
	go func() { _, e := b.Accept(ctx); acc <- e }()
	if _, err := sa.DialFrom(ctx, DialerConfig{}, carrier.DialRequest{Card: card, Addr: addr}, 24); err != nil {
		t.Fatalf("sibling DialFrom: %v", err)
	}
	if err := <-acc; err != nil {
		t.Fatalf("Accept: %v", err)
	}
	// and the other way: b's sibling accepts what a dials
	conn2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Sibling(conn2)
	if err != nil {
		t.Fatalf("Sibling: %v", err)
	}
	defer sb.Close()
	acc2 := make(chan error, 1)
	go func() { _, e := sb.Accept(ctx); acc2 <- e }()
	addr2 := proto.Clone(addr).(*pb.Addr)
	addr2.Port = uint32(conn2.LocalAddr().(*net.UDPAddr).Port)
	if _, err := a.DialFrom(ctx, DialerConfig{}, carrier.DialRequest{Card: card, Addr: addr2}, 24); err != nil {
		t.Fatalf("DialFrom to a sibling: %v", err)
	}
	if err := <-acc2; err != nil {
		t.Fatalf("sibling Accept: %v", err)
	}
}
