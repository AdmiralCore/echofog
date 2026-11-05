package noise3

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// PunchNet is the noise3 side of hole punching (punch.Net, services.md §6.4): the listener's socket
// is the one that sends junk and Init datagrams, so a single NAT mapping serves both directions.
type PunchNet struct {
	Listener *Listener
	// Own returns the external addresses of the listener's socket to offer.
	Own func() []netip.AddrPort
	// Card is the verified card of the peer: its noise3 address supplies the masking key.
	Card *objects.Card
	// Dialer configures the inner QUIC client; the zero value is usable.
	Dialer DialerConfig
}

// OwnAddrs implements punch.Net.
func (p *PunchNet) OwnAddrs() []netip.AddrPort {
	if p.Own == nil {
		return nil
	}
	return p.Own()
}

// SendJunk implements punch.Net.
func (p *PunchNet) SendJunk(to netip.AddrPort, b []byte) error { return p.Listener.SendRaw(to, b) }

type dialResult struct {
	c   mux.Carrier
	err error
}

// Dial implements punch.Net: one DialFrom per target, in parallel, sharing the Init budget; the first
// carrier wins and the other dials are cancelled (a late winner is closed).
func (p *PunchNet) Dial(ctx context.Context, targets []netip.AddrPort, budget int) (mux.Carrier, error) {
	if len(targets) == 0 || p.Card == nil || p.Card.Body == nil {
		return nil, errors.New("noise3: punch dial without targets or card")
	}
	var base *pb.Addr
	for _, a := range p.Card.Addrs() {
		if a.GetSuite() == objects.SuiteNoise3 {
			base = a
			break
		}
	}
	if base == nil {
		return nil, errors.New("noise3: the peer card has no noise3 address")
	}
	per := max(budget/len(targets), 1)
	dctx, cancel := context.WithCancel(ctx)
	ch := make(chan dialResult, len(targets))
	for _, t := range targets {
		addr := proto.Clone(base).(*pb.Addr)
		addr.Host, addr.Port = t.Addr().String(), uint32(t.Port())
		go func() {
			c, err := p.Listener.DialFrom(dctx, p.Dialer, carrier.DialRequest{Card: p.Card, Addr: addr}, per)
			ch <- dialResult{c, err}
		}()
	}
	var last error
	for got := 1; got <= len(targets); got++ {
		r := <-ch
		if r.err != nil {
			last = r.err
			continue
		}
		cancel()
		rest := len(targets) - got
		go func() { // dials that finished anyway are not wanted
			for i := 0; i < rest; i++ {
				if l := <-ch; l.c != nil {
					_ = l.c.Close()
				}
			}
		}()
		return r.c, nil
	}
	cancel()
	return nil, fmt.Errorf("noise3: punch dial: %w", last)
}
