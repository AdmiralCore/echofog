// Package carrier is the L0 carrier layer of EchoFog v2 (docs/protocol/v2/wire.md §1, §10):
// the interfaces every masking suite implements, the D21 carrier-selection plan, happy-eyeballs
// dialing, the per-network memory of the working suite, and the conformance suite in
// carrier/carriertest.
//
// A carrier is one mux.Carrier connection (see internal/v2/mux/carrier.go). Suite packages
// (carrier/noise3, carrier/quicx, carrier/reality2, carrier/ws2) register their dialers and
// listeners under their L0 suite number from their own package; this package never imports them,
// so the dependency graph stays one-way.
//
// The package also ships carrier/memcarrier, an in-memory pair used by session and service tests.
package carrier

import (
	"context"
	"net"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Dialer opens one carrier for one address of a peer's card. Implementations are registered by
// their suite package (see Registry) and must respect ctx and the dial timeout in DialTimeout.
type Dialer interface {
	Dial(ctx context.Context, req DialRequest) (mux.Carrier, error)
}

// Listener accepts carrier connections of one masking suite. Implementations are registered by
// their suite package (see Registry); Accept returns the server side of one connection.
type Listener interface {
	Accept(ctx context.Context) (mux.Carrier, error)
	// Addr is the address the listener bound, or nil when the suite has no single address.
	Addr() net.Addr
	Close() error
}

// DialRequest is everything a suite needs to dial one address from a signed card: the verified
// card (masking keys, suite parameters), the chosen address, and the proxy hook.
type DialRequest struct {
	// Card is the verified responder card the address came from (wire.md §1: nothing else may
	// choose a suite or its parameters).
	Card *objects.Card
	// Addr is the card address to dial; its suite selects the registered Dialer.
	Addr *pb.Addr
	// DialContext, when not nil, opens the underlying TCP or UDP socket through a proxy
	// (T11.6: SOCKS5 for reality2-tcp and ws2). network is "tcp" or "udp", address is host:port.
	// A nil hook means a direct dial.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}
