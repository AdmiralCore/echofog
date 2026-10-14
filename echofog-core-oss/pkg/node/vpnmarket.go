package node

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// swarmTunneler opens a tunnel through an exit and closes it again without sending payload
type swarmTunneler struct{ n *Node }

func (t swarmTunneler) TestDial(ctx context.Context, exit types.PeerID, target string) (time.Duration, error) {
	dctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(t.n.Swarm, exit)).DialExitStream(dctx, target)
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return time.Since(start), nil
}

// swarmConnector dials peers that are known by ID but not connected yet
type swarmConnector struct{ n *Node }

func (c swarmConnector) Ensure(ctx context.Context, peer types.PeerID) error {
	if c.n.Swarm.GetPeer(peer) != nil {
		return nil
	}
	if c.n.DhtEngine == nil {
		return errors.New("peer is not connected")
	}
	addr, err := c.n.DhtEngine.FindPeer(ctx, peer)
	if err != nil {
		return err
	}
	return c.n.Swarm.AddPotentialPeerWithCtx(ctx, peer, addr)
}

func (n *Node) connectedPeerIDs() []types.PeerID {
	var out []types.PeerID
	for _, p := range n.Swarm.GetAllPeers() {
		out = append(out, p.ID())
	}
	return out
}

// initVPNMarket wires the exit node marketplace: publisher, directory, prober and local ratings
func (n *Node) initVPNMarket() {
	st := n.Swarm.GetStorage()
	ratings := vpnmarket.NewRatingStore(st)
	dir := vpnmarket.NewDirectory(n.DhtEngine, ratings, n.ID, n.connectedPeerIDs)
	pub := vpnmarket.NewPublisher(n.DhtEngine, n.Swarm, n.ID, n.Swarm.GetMyPubKey(), dir.TopNeighbors)
	n.VPNMarket = vpnmarket.NewService(pub, dir, ratings, st, swarmTunneler{n}, n.BWProbe, swarmConnector{n})
	n.VPNMarket.Run(n.Ctx)
}
