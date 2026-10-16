package node

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	directoryAsk     = 8 // peers asked for their exits on each refresh
	directoryTimeout = 5 * time.Second
	maxDirectoryJSON = vpnmarket.MaxShared * 4096
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
	// a dial, else a hole punch through a relay for an exit behind NAT
	return c.n.EnsureConnected(ctx, peer)
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
	dir.SetHints(n.askPeersForExits)
	n.Swarm.RegisterStreamHandler(net_types.TypeExitDirectory, func(st *network.Stream, _ []byte, _ types.PeerID) {
		defer st.Close()
		if raw, err := json.Marshal(dir.Shared()); err == nil {
			_ = st.WriteFrame(net_types.TypeExitDirectory, raw)
		}
	})
	pub := vpnmarket.NewPublisher(n.DhtEngine, n.Swarm, n.ID, n.Swarm.GetMyPubKey(), dir.TopNeighbors)
	n.VPNMarket = vpnmarket.NewService(pub, dir, ratings, st, swarmTunneler{n}, n.BWProbe, swarmConnector{n})
	n.VPNMarket.Run(n.Ctx)
}

// askPeersForExits asks a few connected peers, at once, which exits they know. Relays and
// signalers are connected to many nodes, exits behind NAT among them, so they know the most.
func (n *Node) askPeersForExits(ctx context.Context) [][]byte {
	peers := n.connectedPeerIDs()
	rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })
	if len(peers) > directoryAsk {
		peers = peers[:directoryAsk]
	}
	var (
		mu  sync.Mutex
		out [][]byte
		wg  sync.WaitGroup
	)
	for _, p := range peers {
		wg.Add(1)
		go func(p types.PeerID) {
			defer wg.Done()
			got := n.askExitDirectory(ctx, p)
			mu.Lock()
			out = append(out, got...)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

// askExitDirectory fetches one peer's directory; a peer of an older version has no handler and
// closes the stream, which gives nothing.
func (n *Node) askExitDirectory(ctx context.Context, peer types.PeerID) [][]byte {
	ctx, cancel := context.WithTimeout(ctx, directoryTimeout)
	defer cancel()
	streams, err := n.Swarm.OpenNewStreams(ctx, peer, 1)
	if err != nil {
		return nil
	}
	st := streams[0]
	defer st.Close()
	go func() { <-ctx.Done(); st.Close() }()
	if err := st.WriteFrame(net_types.TypeExitDirectory, nil); err != nil {
		return nil
	}
	t, raw, err := st.ReadFrame()
	if err != nil || t != net_types.TypeExitDirectory || len(raw) > maxDirectoryJSON {
		return nil
	}
	var adverts [][]byte
	if json.Unmarshal(raw, &adverts) != nil || len(adverts) > vpnmarket.MaxShared {
		return nil
	}
	return adverts
}
