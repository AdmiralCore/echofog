package scenarios

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// freeExit is the exit offer of a free exit.
type freeExit struct{}

func (freeExit) ExitOffer() *pb.ExitOffer { return &pb.ExitOffer{} }

// registerLabExit serves exit/2 with the product's exit server on a node that does not carry the
// exit role, with private targets allowed. The node module refuses them (that is what
// v2-exit/private-refused measures on a node that does carry the role); the lab's web server
// stands on loopback.
func registerLabExit(rt *node.Runtime) error {
	spec, ok := service.Builtin(service.Exit)
	if !ok {
		return errNoExitService
	}
	srv, err := exit.NewServer(exit.ServerConfig{AllowPrivateTargets: true})
	if err != nil {
		return err
	}
	rt.Register(spec, func(ctx context.Context, p node.Peer, st *mux.Stream) {
		srv.Handle(ctx, exit.Peer{Node: [32]byte(p.Node), Device: [32]byte(p.Device), SID: p.SID, Remote: p.Remote}, st)
	})
	return nil
}
