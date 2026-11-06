package noise3

import (
	"context"
	"errors"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
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
	// Dialer configures the inner QUIC client; the zero value is usable.
	Dialer DialerConfig
	// Deliver receives a carrier accepted on a birthday socket (the runtime serves it like any
	// accepted carrier).
	Deliver func(mux.Carrier)
}

var _ punch.SockNet = (*BirthdaySocks)(nil)

// OpenSock implements punch.SockNet.
func (b *BirthdaySocks) OpenSock() (punch.Sock, error) {
	return nil, errors.New("noise3: not implemented")
}

// Listener implements punch.SockNet.
func (b *BirthdaySocks) Listener() punch.Sock { return stubSock{} }

type stubSock struct{}

func (stubSock) Send(netip.AddrPort, []byte) error { return errors.New("noise3: not implemented") }
func (stubSock) Recv() <-chan punch.Datagram       { return nil }
func (stubSock) Close() error                      { return nil }

// DialFrom implements punch.SockNet.
func (b *BirthdaySocks) DialFrom(ctx context.Context, s punch.Sock, target netip.AddrPort, budget int) (mux.Carrier, error) {
	return nil, errors.New("noise3: not implemented")
}

// AcceptOn implements punch.SockNet.
func (b *BirthdaySocks) AcceptOn(ctx context.Context, s punch.Sock) error {
	return errors.New("noise3: not implemented")
}
