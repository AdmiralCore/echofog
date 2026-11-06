package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

var errExitClientNotImplemented = errors.New("node: exit client not implemented")

// ExitClient returns the exit/2 client of one exit, dialing it under the per-exit client identity
// (services.md §7).
func (r *Runtime) ExitClient(ctx context.Context, card *objects.Card, opt DialOpt) (*exit.Client, error) {
	return nil, errExitClientNotImplemented
}

// ExitIdentityIDs returns the node and device IDs the given exit sees for this client.
func (r *Runtime) ExitIdentityIDs(ctx context.Context, exitDevice DeviceID) (NodeID, DeviceID, error) {
	return NodeID{}, DeviceID{}, errExitClientNotImplemented
}

// exitShape is the shape profile the exit pads its tunnels with: the profile of its address.
func exitShape(cfg Config) *shape.Profile { return cfg.ShapeProfile }
