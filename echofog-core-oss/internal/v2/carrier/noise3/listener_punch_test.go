package noise3

import (
	"context"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
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

// transfer moves n bytes from the far side to the dialed carrier over a stream, so the dial's route
// has to keep carrying data long after the Init handshake used its budget.
func transfer(t *testing.T, ctx context.Context, dialed, far mux.Carrier, n int) int {
	t.Helper()
	go func() {
		s, err := far.OpenStream(ctx)
		if err != nil {
			return
		}
		buf := make([]byte, 1000)
		for sent := 0; sent < n; {
			k := min(len(buf), n-sent)
			if _, err := s.Write(buf[:k]); err != nil {
				return
			}
			sent += k
		}
		_ = s.CloseWrite()
	}()
	s, err := dialed.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	_ = s.SetReadDeadline(time.Now().Add(8 * time.Second))
	got, _ := io.Copy(io.Discard, s)
	return int(got)
}

// G1: the Init budget covers the handshake only; the data of the carrier afterwards is not limited.
func TestDialFromBudgetIsForTheHandshakeOnly(t *testing.T) {
	a, _, _ := startNoise3(t)
	b, card, addr := startNoise3(t)
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
	c, err := a.DialFrom(ctx, DialerConfig{}, carrier.DialRequest{Card: card, Addr: addr}, 24)
	if err != nil {
		t.Fatalf("DialFrom: %v", err)
	}
	defer c.Close()
	far := <-acc
	if far == nil {
		t.Fatal("far side did not accept")
	}
	defer far.Close()
	const n = 300_000
	if got := transfer(t, ctx, c, far, n); got != n {
		t.Fatalf("received %d of %d bytes: the budget cut the carrier off", got, n)
	}
}

// G2: a dual-stack listener ([::]) sees IPv4 peers as 4in6 addresses; the route must still match.
func TestDialFromOnADualStackListener(t *testing.T) {
	mk, _ := newMaskKey(t, time.Now())
	a, err := Listen("[::]:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t), Rand: mrand.New(mrand.NewPCG(3, 4))})
	if err != nil {
		t.Skipf("no dual-stack socket: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, card, addr := startNoise3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acc := make(chan error, 1)
	go func() { _, err := b.Accept(ctx); acc <- err }()
	c, err := a.DialFrom(ctx, DialerConfig{}, carrier.DialRequest{Card: card, Addr: addr}, 24)
	if err != nil {
		t.Fatalf("DialFrom on [::]: %v", err)
	}
	defer c.Close()
	if err := <-acc; err != nil {
		t.Fatalf("far Accept: %v", err)
	}
}
