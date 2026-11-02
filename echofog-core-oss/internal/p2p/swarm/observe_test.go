package swarm

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A peer reports the address it sees our connection at; on loopback that is our own socket.
func TestPeersReportTheAddressTheySeeUsAt(t *testing.T) {
	ctxA, swA, trA, storeA := testSwarmInit(t, "127.0.0.1:0")
	defer swA.Close()
	defer trA.Close()
	defer storeA.CloseAndClear()
	_, swB, trB, storeB := testSwarmInit(t, "127.0.0.1:0")
	defer swB.Close()
	defer trB.Close()
	defer storeB.CloseAndClear()

	require.NoError(t, trB.DialEarly(ctxA, trA.Addr().String()))
	require.Eventually(t, func() bool { return len(swA.GetAllPeers()) > 0 && len(swB.GetAllPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// B asks A which address it sees B at: B's own QUIC socket
	ap, err := swB.ObservedAddr(ctx, swB.GetAllPeers()[0].ID())
	require.NoError(t, err)
	assert.Equal(t, trB.Addr().String(), ap.String())

	// and A asks B (the connection was opened by B, A sees B's listening address either way)
	ap, err = swA.ObservedAddr(ctx, swA.GetAllPeers()[0].ID())
	require.NoError(t, err)
	assert.Equal(t, trA.Addr().String(), ap.String())

	obs := swB.ObserveFromPeers(ctx, 4)
	require.Len(t, obs, 1)
	assert.Equal(t, trB.Addr().String(), obs[0].Addr.String())
	r := natcheck.Classify(obs, nil)
	assert.Equal(t, natcheck.MappingUnknown, r.Mapping, "a single observer cannot tell the mapping kind")
}

func TestObservedAddrFailsForAnUnknownPeer(t *testing.T) {
	_, swA, trA, storeA := testSwarmInit(t, "127.0.0.1:0")
	defer swA.Close()
	defer trA.Close()
	defer storeA.CloseAndClear()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := swA.ObservedAddr(ctx, [32]byte{9})
	assert.Error(t, err)
	assert.Empty(t, swA.ObserveFromPeers(ctx, 4))
}

// Only peers connected through the given socket are asked: another socket has a NAT mapping of
// its own.
func TestObserveFromPeersOnKeepsToOneSocket(t *testing.T) {
	ctxA, swA, trA, storeA := testSwarmInit(t, "127.0.0.1:0")
	defer swA.Close()
	defer trA.Close()
	defer storeA.CloseAndClear()
	_, swB, trB, storeB := testSwarmInit(t, "127.0.0.1:0")
	defer swB.Close()
	defer trB.Close()
	defer storeB.CloseAndClear()

	require.NoError(t, trB.DialEarly(ctxA, trA.Addr().String()))
	require.Eventually(t, func() bool { return len(swB.GetAllPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	main := swB.ObserveFromPeersOn(ctx, 4, trB.UDPPort())
	require.Len(t, main, 1, "the main socket's peer is asked")
	assert.False(t, main[0].IPOnly)
}

// A peer connected through another socket (a REALITY dial's own) tells our IP, not our port: in
// guard mode the guards may be the only peers there are, and without them the node's country was
// never known, so guard_mode "auto" never stood down even in a free country.
func TestPeersOnAnotherSocketTellTheIPOnly(t *testing.T) {
	ctxA, swA, trA, storeA := testSwarmInit(t, "127.0.0.1:0")
	defer swA.Close()
	defer trA.Close()
	defer storeA.CloseAndClear()
	_, swB, trB, storeB := testSwarmInit(t, "127.0.0.1:0")
	defer swB.Close()
	defer trB.Close()
	defer storeB.CloseAndClear()

	require.NoError(t, trB.DialEarly(ctxA, trA.Addr().String()))
	require.Eventually(t, func() bool { return len(swB.GetAllPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// seen as if the node listened on another port, A is a peer on another socket, like a guard
	obs := swB.ObserveFromPeersOn(ctx, 4, trB.UDPPort()+1)
	require.Len(t, obs, 1, "a peer on another socket still tells our IP")
	r := natcheck.Classify(obs, nil)
	assert.Equal(t, "127.0.0.1", r.Public.Addr().String(), "the IP is taken")
	assert.Zero(t, r.Public.Port(), "the port is not: that socket has a NAT mapping of its own")
	assert.Equal(t, natcheck.MappingUnknown, r.Mapping)
}

// The servers the node was told about and its guards are asked first, then peers it dialed, then
// peers that dialed in; and one peer per network before a second from the same one. What a peer
// announces about itself does not count: it used to be enough to announce relaying to be asked
// first, so two cheap hosts answered for the whole Internet.
func TestObserversAreTheTrustedAndTheDiverseFirst(t *testing.T) {
	id := func(b byte) types.PeerID { return types.PeerID{b} }
	host := netip.MustParseAddr
	cands := []observerCand{
		{id: id(1), host: host("1.0.0.1"), tier: tierInbound},
		{id: id(2), host: host("9.9.1.1"), tier: tierDialed},
		{id: id(3), host: host("9.9.2.2"), tier: tierDialed}, // same /16 as 2
		{id: id(4), host: host("8.8.8.8"), tier: tierTold},
		{id: id(5), host: host("7.7.7.7"), tier: tierInbound},
		{id: id(6), host: host("7.7.7.7"), tier: tierInbound}, // same host as 5
	}
	ids := func(cs []observerCand) (out []types.PeerID) {
		for _, c := range cs {
			out = append(out, c.id)
		}
		return out
	}
	assert.Equal(t, []types.PeerID{id(4), id(2), id(1)}, ids(pickObservers(append([]observerCand(nil), cands...), 3)))
	assert.Equal(t, []types.PeerID{id(4), id(2), id(1), id(5), id(3)}, ids(pickObservers(append([]observerCand(nil), cands...), 8)),
		"the second one from a network only when nothing else is left, a host only once")

	// peers on other sockets fill up after the ones on ours, trusted or not (a second one from
	// a network waits even for them)
	guard := observerCand{id: id(7), host: host("5.5.5.5"), tier: tierTold, ipOnly: true}
	assert.Equal(t, []types.PeerID{id(4), id(2), id(1), id(5)}, ids(pickObservers(append([]observerCand{guard}, cands...), 4)))
	assert.Equal(t, []types.PeerID{id(4), id(2), id(1), id(5), id(7)}, ids(pickObservers(append([]observerCand{guard}, cands...), 5)))
}
