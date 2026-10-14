package holepunch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type node struct {
	id   types.PeerID
	tr   *network.QuicTransport
	svc  *Service
	mu   sync.Mutex
	conn map[types.PeerID]bool
}

func (n *node) connected(p types.PeerID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conn[p]
}

func newNode(t *testing.T, ctx context.Context) *node {
	im, store := testutil.SetupTestIdentity(t)
	t.Cleanup(store.CloseAndClear)
	account, err := im.GetActiveAccount()
	require.NoError(t, err)
	_, pub, err := account.GetIdentityKey()
	require.NoError(t, err)
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	tr, err := network.NewQUICTransport("127.0.0.1:0", tlsCfg, network.GetQuicConfig(), im, config.CurrentProtocolVersion)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })
	require.NoError(t, tr.ListenEarly(ctx))

	n := &node{id: types.PeerPubKeyToID(pk), tr: tr, conn: map[types.PeerID]bool{}}
	go func() { // what the swarm does with new connections
		for ev := range tr.ConnChan() {
			n.mu.Lock()
			n.conn[ev.PeerID] = true
			n.mu.Unlock()
		}
	}()
	return n
}

// Two real QUIC nodes coordinate through an in-memory relay and end up directly connected. On
// loopback there is no NAT in the way, so this proves the choreography (CONNECT, SYNC, punch,
// dial, arrival) works end to end, not that a given NAT lets it through.
func TestRealQUICNodesConnectDirectlyAfterCoordinating(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := newNode(t, ctx), newNode(t, ctx)

	var mu sync.Mutex
	peers := map[types.PeerID]*Service{}
	mk := func(self *node) *Service {
		return New(Config{
			Transport:    self.tr,
			Signal:       &link{from: self.id, mu: &mu, peers: peers},
			OwnAddrs:     func() []string { return []string{self.tr.Addr().String()} },
			Connected:    self.connected,
			AllowPrivate: true,
		})
	}
	a.svc, b.svc = mk(a), mk(b)
	mu.Lock()
	peers[a.id], peers[b.id] = a.svc, b.svc
	mu.Unlock()
	defer a.svc.Close()
	defer b.svc.Close()

	cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	defer ccancel()
	require.NoError(t, a.svc.Connect(cctx, b.id))

	require.Eventually(t, func() bool { return a.connected(b.id) && b.connected(a.id) }, 5*time.Second, 50*time.Millisecond,
		"both sides must hold an authenticated connection to the other")
	assert.NotEqual(t, a.id, b.id)
}
