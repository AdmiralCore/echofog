package noise3

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// sink is a bare UDP socket that records who sent to it.
func sink(t *testing.T) (*net.UDPConn, chan netip.AddrPort) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ch := make(chan netip.AddrPort, 4096)
	go func() {
		buf := make([]byte, 4096)
		for {
			_, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			ch <- from
		}
	}()
	return c, ch
}

func TestSendRawComesFromTheListenerSocket(t *testing.T) {
	l, _, _ := startNoise3(t)
	c, got := sink(t)
	to := c.LocalAddr().(*net.UDPAddr).AddrPort()
	if err := l.SendRaw(to, []byte("junk junk junk junk junk junk junk junk")); err != nil {
		t.Fatal(err)
	}
	select {
	case from := <-got:
		if from != l.Addr().(*net.UDPAddr).AddrPort() {
			t.Fatalf("junk came from %v, want the listener %v", from, l.Addr())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram")
	}
}

// DialFrom: the Inits leave from the listener's address, never more than the budget, and a real
// listener answers: the carrier comes back and the far side accepts it.
func TestDialFromUsesTheListenerSocketAndBudget(t *testing.T) {
	l, _, _ := startNoise3(t)
	c, got := sink(t)
	_, card, addr := startNoise3(t)
	addr.Port = uint32(c.LocalAddr().(*net.UDPAddr).Port) // nobody answers behind this address
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_, err := l.DialFrom(ctx, DialerConfig{}, carrier.DialRequest{Card: card, Addr: addr}, 24)
	if err == nil {
		t.Fatal("a dial to a silent address succeeded")
	}
	n := 0
	for len(got) > 0 {
		from := <-got
		if from != l.Addr().(*net.UDPAddr).AddrPort() {
			t.Fatalf("Init came from %v, want the listener %v", from, l.Addr())
		}
		n++
	}
	if n == 0 || n > 24 {
		t.Fatalf("%d datagrams to the target, want 1..24", n)
	}
}

func TestDialFromReachesARealListener(t *testing.T) {
	a, _, _ := startNoise3(t)
	b, card, addr := startNoise3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acc := make(chan error, 1)
	go func() { _, err := b.Accept(ctx); acc <- err }()
	c, err := a.DialFrom(ctx, DialerConfig{}, carrier.DialRequest{Card: card, Addr: addr}, 24)
	if err != nil {
		t.Fatalf("DialFrom: %v", err)
	}
	defer c.Close()
	if err := <-acc; err != nil {
		t.Fatalf("far side Accept: %v", err)
	}
}
