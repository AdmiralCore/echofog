package noise3

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/punch"
)

// BirthdaySocks is the noise3 socket side of birthday punching (punch.SockNet, services.md §6.4.1)
// for one peer: extra UDP sockets for the symmetric side's many mappings, the listener's own socket
// for the spray, and noise3 Init and accept on whichever socket a mapping was found.
type BirthdaySocks struct {
	// Main is the node's noise3 listener: its socket sprays, its keys serve accepted flows.
	Main *Listener
	// Card is the verified card of the peer: its noise3 address supplies the masking key for Init.
	Card *objects.Card
	// Base is the noise3 address to take the masking key from; nil takes the card's own.
	Base *pb.Addr
	// Dialer configures the inner QUIC client; the zero value is usable.
	Dialer DialerConfig
	// Deliver receives a carrier accepted on a birthday socket (the runtime serves it like any
	// accepted carrier).
	Deliver func(mux.Carrier)

	tapOnce sync.Once
	tapCh   chan punch.Datagram
}

var _ punch.SockNet = (*BirthdaySocks)(nil)

// udpSock is one extra UDP socket. A reader goroutine feeds Recv until detach hands the socket to a
// listener.
type udpSock struct {
	conn *net.UDPConn
	recv chan punch.Datagram
	done chan struct{}
	once sync.Once
}

func newUDPSock() (*udpSock, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	s := &udpSock{conn: c, recv: make(chan punch.Datagram, 64), done: make(chan struct{})}
	go s.pump()
	return s, nil
}

func (s *udpSock) pump() {
	defer close(s.done)
	buf := make([]byte, 4096)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return // closed, or the read deadline of detach
		}
		d := punch.Datagram{From: netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), Payload: append([]byte(nil), buf[:n]...)}
		select {
		case s.recv <- d:
		default:
		}
	}
}

// detach stops the reader and returns the socket for a listener to read.
func (s *udpSock) detach() *net.UDPConn {
	s.once.Do(func() {
		_ = s.conn.SetReadDeadline(time.Now())
		<-s.done
		_ = s.conn.SetReadDeadline(time.Time{})
	})
	return s.conn
}

// LocalPort is the port of the socket.
func (s *udpSock) LocalPort() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

func (s *udpSock) Send(to netip.AddrPort, p []byte) error {
	_, err := s.conn.WriteToUDPAddrPort(p, to)
	return err
}
func (s *udpSock) Recv() <-chan punch.Datagram { return s.recv }
func (s *udpSock) Close() error                { return s.conn.Close() }

// OpenSock implements punch.SockNet.
func (b *BirthdaySocks) OpenSock() (punch.Sock, error) { return newUDPSock() }

// listenerSock is the listener's own socket: Send leaves from its port, Recv is the tap.
type listenerSock struct{ b *BirthdaySocks }

func (s listenerSock) Send(to netip.AddrPort, p []byte) error { return s.b.Main.SendRaw(to, p) }
func (s listenerSock) Recv() <-chan punch.Datagram            { return s.b.tapCh }
func (s listenerSock) Close() error                           { return nil }

// Listener implements punch.SockNet.
func (b *BirthdaySocks) Listener() punch.Sock {
	b.tapOnce.Do(func() {
		b.tapCh = make(chan punch.Datagram, 256)
		b.Main.SetTap(func(src netip.AddrPort, d []byte) {
			select {
			case b.tapCh <- punch.Datagram{From: src, Payload: append([]byte(nil), d...)}:
			default:
			}
		})
	})
	for len(b.tapCh) > 0 { // datagrams of an earlier attempt are stale
		<-b.tapCh
	}
	return listenerSock{b}
}

// DialFrom implements punch.SockNet: a sibling listener takes over the socket and dials from it.
func (b *BirthdaySocks) DialFrom(ctx context.Context, s punch.Sock, target netip.AddrPort, budget int) (mux.Carrier, error) {
	us, ok := s.(*udpSock)
	if !ok {
		return nil, errors.New("noise3: DialFrom needs a birthday socket")
	}
	if b.Card == nil || b.Card.Body == nil {
		return nil, errors.New("noise3: birthday dial without the peer card")
	}
	base := b.Base
	for _, a := range b.Card.Addrs() {
		if base == nil && a.GetSuite() == objects.SuiteNoise3 {
			base = a
		}
	}
	if base == nil {
		return nil, errors.New("noise3: the peer card has no noise3 address")
	}
	sib, err := b.Main.Sibling(us.detach())
	if err != nil {
		return nil, err
	}
	addr := proto.Clone(base).(*pb.Addr)
	addr.Host, addr.Port = target.Addr().String(), uint32(target.Port())
	c, err := sib.DialFrom(ctx, b.Dialer, carrier.DialRequest{Card: b.Card, Addr: addr}, budget)
	if err != nil {
		_ = sib.Close()
		return nil, fmt.Errorf("noise3: birthday dial: %w", err)
	}
	return &sibCarrier{Carrier: c, sib: sib}, nil
}

// AcceptOn implements punch.SockNet: a sibling listener takes over the socket until a flow arrives.
func (b *BirthdaySocks) AcceptOn(ctx context.Context, s punch.Sock) error {
	us, ok := s.(*udpSock)
	if !ok {
		return errors.New("noise3: AcceptOn needs a birthday socket")
	}
	sib, err := b.Main.Sibling(us.detach())
	if err != nil {
		return err
	}
	c, err := sib.Accept(ctx)
	if err != nil {
		_ = sib.Close()
		return err
	}
	wrapped := &sibCarrier{Carrier: c, sib: sib}
	if b.Deliver == nil {
		_ = wrapped.Close()
		return errors.New("noise3: no receiver for the accepted carrier")
	}
	b.Deliver(wrapped)
	return nil
}

// sibCarrier closes the sibling listener (and with it the socket) when the carrier closes.
type sibCarrier struct {
	mux.Carrier
	sib  *Listener
	once sync.Once
}

func (c *sibCarrier) Close() error {
	err := c.Carrier.Close()
	c.once.Do(func() { _ = c.sib.Close() })
	return err
}
