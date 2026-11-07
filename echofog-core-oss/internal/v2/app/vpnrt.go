package app

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// ExitClientSource is the part of *node.Runtime the VPN dials exits with.
type ExitClientSource interface {
	ExitClient(ctx context.Context, card *objects.Card, opt node.DialOpt) (*exit.Client, error)
}

// RuntimeExit makes the ExitDialer of Config.Exit over the node's runtime: every exit gets its own
// client identity and session (node.Runtime.ExitClient).
func RuntimeExit(rt ExitClientSource) ExitDialer { return runtimeExit{rt} }

type runtimeExit struct{ rt ExitClientSource }

func (r runtimeExit) Dial(ctx context.Context, card *objects.Card) (ExitLink, error) {
	c, err := r.rt.ExitClient(ctx, card, node.DialOpt{})
	if err != nil {
		return nil, err
	}
	return c, nil
}
