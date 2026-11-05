package noise3

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func punchCard(card *objects.Card, addr *pb.Addr) *objects.Card {
	card.Body.Addrs = []*pb.Addr{addr}
	return card
}

func TestPunchNetOwnAndJunk(t *testing.T) {
	l, _, _ := startNoise3(t)
	want := []netip.AddrPort{netip.MustParseAddrPort("198.51.100.9:50000")}
	p := &PunchNet{Listener: l, Own: func() []netip.AddrPort { return want }}
	if got := p.OwnAddrs(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("OwnAddrs %v", got)
	}
	c, got := sink(t)
	if err := p.SendJunk(c.LocalAddr().(*net.UDPAddr).AddrPort(), make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	select {
	case from := <-got:
		if from != l.Addr().(*net.UDPAddr).AddrPort() {
			t.Fatalf("junk from %v", from)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no junk")
	}
}

// Two targets, only the second one is a real listener: the first silent one does not stop the dial,
// and the carrier is the one to the real listener, sent from our listener's socket.
func TestPunchNetDialPicksTheTargetThatAnswers(t *testing.T) {
	a, _, _ := startNoise3(t)
	b, card, addr := startNoise3(t)
	silent, _ := sink(t)
	p := &PunchNet{Listener: a, Card: punchCard(card, addr)}
	targets := []netip.AddrPort{
		silent.LocalAddr().(*net.UDPAddr).AddrPort(),
		b.Addr().(*net.UDPAddr).AddrPort(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acc := make(chan error, 1)
	go func() { _, err := b.Accept(ctx); acc <- err }()
	c, err := p.Dial(ctx, targets, 24)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := <-acc; err != nil {
		t.Fatalf("far Accept: %v", err)
	}
}

func TestPunchNetDialWithoutNoise3AddressFails(t *testing.T) {
	a, _, _ := startNoise3(t)
	p := &PunchNet{Listener: a, Card: &objects.Card{Body: &pb.NodeCard{}}}
	if _, err := p.Dial(context.Background(), []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:9")}, 24); err == nil {
		t.Fatal("dial without a noise3 address in the card")
	}
	if _, err := p.Dial(context.Background(), nil, 24); err == nil {
		t.Fatal("dial without targets")
	}
}

// G1 through PunchNet: the per-target share of the budget limits the handshake, not the carrier's data.
func TestPunchNetCarrierKeepsCarryingData(t *testing.T) {
	a, _, _ := startNoise3(t)
	b, card, addr := startNoise3(t)
	silent1, _ := sink(t)
	silent2, _ := sink(t)
	p := &PunchNet{Listener: a, Card: punchCard(card, addr)}
	targets := []netip.AddrPort{
		silent1.LocalAddr().(*net.UDPAddr).AddrPort(),
		silent2.LocalAddr().(*net.UDPAddr).AddrPort(),
		b.Addr().(*net.UDPAddr).AddrPort(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	acc := make(chan mux.Carrier, 1)
	go func() {
		c, err := b.Accept(ctx)
		if err != nil {
			acc <- nil
			return
		}
		acc <- c
	}()
	c, err := p.Dial(ctx, targets, 24)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	far := <-acc
	if far == nil {
		t.Fatal("far side did not accept")
	}
	defer far.Close()
	const n = 300_000
	if got := transfer(t, ctx, c, far, n); got != n {
		t.Fatalf("received %d of %d bytes", got, n)
	}
}
