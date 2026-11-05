package noise3

import (
	"context"
	"errors"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// SendRaw sends one datagram to to from the listener's socket. It opens this end's NAT mapping
// towards to (hole punching, services.md §6.4 step 4) and is the junk sender.
func (l *Listener) SendRaw(to netip.AddrPort, p []byte) error {
	return errors.New("noise3: not implemented")
}

// DialFrom dials like Dialer.Dial but sends from the listener's own socket, so the NAT mapping the
// peer has to hit is the listener's. At most budget datagrams leave for the target in all (Init
// retransmissions included); later ones are dropped silently. Replies from the target are routed to
// this dial instead of the listener's responder side while it lives.
func (l *Listener) DialFrom(ctx context.Context, cfg DialerConfig, req carrier.DialRequest, budget int) (mux.Carrier, error) {
	return nil, errors.New("noise3: not implemented")
}
