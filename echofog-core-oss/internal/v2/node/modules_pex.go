package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/pex"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// registerPEX registers pex/2 (T5.4). The service itself is created in New, together with its
// persistent candidate table, so Host.Peers() works before Start; this file only wires the
// handler into the service registry.
func (r *Runtime) registerPEX() {
	spec, ok := service.Builtin(service.PEX)
	if !ok || r.pex == nil {
		return
	}
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		r.pex.Handle(ctx, pex.Peer{
			Node:   [32]byte(p.Node),
			Device: [32]byte(p.Device),
			Card:   p.Card,
			SID:    p.SID,
		}, st)
	})
}
