package noise3

import (
	"context"
	"errors"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
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

func (p *PunchNet) OwnAddrs() []netip.AddrPort { return nil }

func (p *PunchNet) SendJunk(to netip.AddrPort, b []byte) error { return errors.New("not implemented") }

func (p *PunchNet) Dial(ctx context.Context, targets []netip.AddrPort, budget int) (mux.Carrier, error) {
	return nil, errors.New("not implemented")
}
