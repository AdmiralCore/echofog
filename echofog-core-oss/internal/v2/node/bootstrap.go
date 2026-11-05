package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// ErrBootstrapTarget reports a bootstrap target that cannot be dialed.
var ErrBootstrapTarget = errors.New("node: bad bootstrap target")

// BootstrapTarget is one address of a bootstrap list that has no card (wire.md §1).
type BootstrapTarget struct {
	// Addr is the address to dial: suite, host, port, mask index 1 and the suite parameters.
	Addr *pb.Addr
	// Mask is the responder's masking public key from the list (the list is the only source).
	Mask keys.AKey
	// Expected is the device ID the list names for the address. Nil: bare address.
	Expected *DeviceID
}

// BootstrapResult is what an XX bootstrap session found.
type BootstrapResult struct {
	// Card is the responder's public card from message 2.
	Card *objects.Card
	// Verified is true only when the list named the device ID and the handshake checked it.
	Verified bool
}

// DialBootstrap runs the XX handshake to an address without a card (handshake.md §4.2).
func (r *Runtime) DialBootstrap(ctx context.Context, t BootstrapTarget, opt DialOpt) (*BootstrapResult, error) {
	return nil, errors.New("node: DialBootstrap is not implemented")
}
