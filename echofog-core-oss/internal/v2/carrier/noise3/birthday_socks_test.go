package noise3

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

func loopback(sock interface{ LocalPort() int }) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(sock.LocalPort()))
}

// An extra socket receives what is sent to it and reports who sent it.
func TestBirthdaySockRecv(t *testing.T) {
	a, cardA, _ := startNoise3(t)
	bs := &BirthdaySocks{Main: a, Card: cardA}
	s, err := bs.OpenSock()
	if err != nil {
		t.Fatalf("OpenSock: %v", err)
	}
	defer s.Close()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteToUDPAddrPort([]byte("ping"), loopback(s.(interface{ LocalPort() int }))); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-s.Recv():
		if d.From != c.LocalAddr().(*net.UDPAddr).AddrPort() || string(d.Payload) != "ping" {
			t.Fatalf("got %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing received")
	}
	if err := s.Send(c.LocalAddr().(*net.UDPAddr).AddrPort(), []byte("pong")); err != nil {
		t.Fatal(err)
	}
}

// The listener's socket as a Sock: Send goes out of the listener's port, Recv is the tap.
func TestBirthdayListenerSock(t *testing.T) {
	a, cardA, _ := startNoise3(t)
	bs := &BirthdaySocks{Main: a, Card: cardA}
	ls := bs.Listener()
	c, got := sink(t)
	if err := ls.Send(c.LocalAddr().(*net.UDPAddr).AddrPort(), make([]byte, 40)); err != nil {
		t.Fatal(err)
	}
	if from := <-got; from != a.Addr().(*net.UDPAddr).AddrPort() {
		t.Fatalf("sent from %v", from)
	}
	if _, err := c.WriteToUDPAddrPort([]byte("12345"), a.Addr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-ls.Recv():
		if d.From != c.LocalAddr().(*net.UDPAddr).AddrPort() {
			t.Fatalf("tap saw %v", d.From)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the listener sock saw nothing")
	}
}

// Dial from one extra socket to an extra socket that accepts: the birthday pair on loopback.
func TestBirthdayDialFromAcceptOn(t *testing.T) {
	a, _, _ := startNoise3(t)
	b, cardB, _ := startNoise3(t)
	delivered := make(chan mux.Carrier, 1)
	sa := &BirthdaySocks{Main: a, Card: cardB}
	sb := &BirthdaySocks{Main: b, Card: nil, Deliver: func(c mux.Carrier) { delivered <- c }}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accSock, err := sb.OpenSock()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sb.AcceptOn(ctx, accSock) }()
	dialSock, err := sa.OpenSock()
	if err != nil {
		t.Fatal(err)
	}
	c, err := sa.DialFrom(ctx, dialSock, loopback(accSock.(interface{ LocalPort() int })), 24)
	if err != nil {
		t.Fatalf("DialFrom: %v", err)
	}
	defer c.Close()
	select {
	case got := <-delivered:
		_ = got.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the accepted carrier was not delivered")
	}
	if err := <-done; err != nil {
		t.Fatalf("AcceptOn: %v", err)
	}
}
