package hidden

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pass = "pass123"

func startNode(t *testing.T, name, dbPath string, tune func(*config.AppConfig)) (*coreNode.Node, *node.NodeApi) {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = dbPath
	cfg.Network.BootstrapNodes = nil
	cfg.Network.NoAutoDial = true
	if tune != nil {
		tune(cfg)
	}
	n := coreNode.NewNode(context.Background(), cfg, name)
	if n.IsRegistered() {
		require.NoError(t, n.Login([]byte(pass)))
	} else {
		_, err := n.Register([]byte(pass), []byte(pass))
		require.NoError(t, err)
	}
	require.NoError(t, n.Start())
	return n, node.NewNodeApi(n)
}

func hiddenMode(mode string) func(*config.AppConfig) {
	return func(c *config.AppConfig) { c.Access.Mode = mode }
}

// link is the node's invitation link with its loopback address (a test node knows no public one).
func link(t *testing.T, n *coreNode.Node) string {
	t.Helper()
	n.Cfg.Access.PublicAddrs = []string{n.Transport.Addr().String()}
	l, err := n.InviteLink()
	require.NoError(t, err)
	return l
}

func connected(n *coreNode.Node, other *coreNode.Node) bool { return n.Swarm.GetPeer(other.ID) != nil }

func exitAccount(t *testing.T, client *coreNode.Node, exit *coreNode.Node) *exit_gateway.AccountInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acc, err := exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(client.Swarm, exit.ID)).QueryAccount(ctx)
	require.NoError(t, err)
	return acc
}

// A hidden exit: a stranger who has its address and peer ID gets no connection; whoever has the
// link gets in, and is remembered across a new secret and a restart.
func TestHiddenNodeOnlyForTheInvited(t *testing.T) {
	hDB := t.TempDir() + "/db"
	hN, h := startNode(t, "hidden", hDB, hiddenMode(config.AccessHidden))
	sN, s := startNode(t, "stranger", t.TempDir()+"/db", nil)
	defer sN.Stop()
	fN, f := startNode(t, "friend", t.TempDir()+"/db", nil)
	defer fN.Stop()

	require.NoError(t, h.StartSharing(node.SharingConfig{}))

	// the stranger knows exactly where the node is, but not the secret
	err := s.ConnectToPeer(network.JoinPeerAddr(hN.Transport.Addr().String(), hN.ID))
	assert.Error(t, err)
	assert.False(t, connected(hN, sN))

	// the friend has the link
	invite := link(t, hN)
	info, err := f.ParseInvite(invite)
	require.NoError(t, err)
	assert.True(t, info.Private)
	id, err := f.JoinInvite(invite)
	require.NoError(t, err)
	assert.Equal(t, hN.ID.String(), id)
	require.Eventually(t, func() bool { return connected(hN, fN) }, 5*time.Second, 50*time.Millisecond)
	assert.Contains(t, hN.Access.Joined(), fN.ID)
	assert.True(t, exitAccount(t, fN, hN).Enabled, "the friend may use the exit")

	// over a connection the hidden node opened itself, a stranger still cannot use its exit
	require.NoError(t, h.ConnectToPeer(network.JoinPeerAddr(sN.Transport.Addr().String(), sN.ID)))
	require.Eventually(t, func() bool { return connected(sN, hN) }, 5*time.Second, 50*time.Millisecond)
	assert.False(t, exitAccount(t, sN, hN).Enabled, "the exit is off for a stranger")

	// a new secret: the old link lets nobody new in, the friend stays
	newLink, err := h.RotateAccessSecret()
	require.NoError(t, err)
	assert.NotEqual(t, invite, newLink)
	st, err := h.GetAccessStatus()
	require.NoError(t, err)
	assert.Equal(t, config.AccessHidden, st.Mode)
	require.Len(t, st.Joined, 1)
	assert.Equal(t, fN.ID.String(), st.Joined[0].PeerID)

	// restart: the secret and who joined are kept
	secret := hN.Access.Secret()
	sN.Swarm.RemovePeer(hN.ID)
	fN.Swarm.RemovePeer(hN.ID)
	hN.Stop()
	hN, _ = startNode(t, "hidden", hDB, hiddenMode(config.AccessHidden))
	defer hN.Stop()
	assert.Equal(t, secret, hN.Access.Secret())
	assert.Contains(t, hN.Access.Joined(), fN.ID)

	// the friend reconnects without a link; the stranger still cannot
	require.NoError(t, f.ConnectToPeer(network.JoinPeerAddr(hN.Transport.Addr().String(), hN.ID)))
	assert.Error(t, s.ConnectToPeer(network.JoinPeerAddr(hN.Transport.Addr().String(), hN.ID)))
}

// The owner's contacts reach a hidden node without a link: calls and messages keep working.
func TestHiddenNodeLetsContactsIn(t *testing.T) {
	hN, h := startNode(t, "hidden", t.TempDir()+"/db", hiddenMode(config.AccessHidden))
	defer hN.Stop()
	cN, c := startNode(t, "contact", t.TempDir()+"/db", nil)
	defer cN.Stop()

	require.NoError(t, h.NewContact(dto.ContactDTO{PeerID: cN.ID.String(), Alias: "friend"}))
	require.NoError(t, c.ConnectToPeer(network.JoinPeerAddr(hN.Transport.Addr().String(), hN.ID)))
	assert.Empty(t, hN.Access.Joined(), "a contact is let in, it does not join")
}

// An isolated node dials nobody, not even its bootstrap nodes, and still takes the invited.
func TestIsolatedNodeDialsNobody(t *testing.T) {
	bN, _ := startNode(t, "public", t.TempDir()+"/db", nil)
	defer bN.Stop()
	boot := network.JoinPeerAddr(bN.Transport.Addr().String(), bN.ID)
	iN, i := startNode(t, "isolated", t.TempDir()+"/db", func(c *config.AppConfig) {
		c.Access.Mode = config.AccessIsolated
		c.Network.BootstrapNodes = []string{boot}
		c.Network.NoAutoDial = false
	})
	defer iN.Stop()
	fN, f := startNode(t, "friend", t.TempDir()+"/db", nil)
	defer fN.Stop()

	assert.Error(t, i.ConnectToPeer(boot), "an isolated node must not dial")
	time.Sleep(2 * time.Second)
	assert.Empty(t, iN.Swarm.GetAllPeers(), "an isolated node joins no network")

	invite := link(t, iN)
	inv, err := f.ParseInvite(invite)
	require.NoError(t, err)
	assert.Zero(t, inv.Relays, "an isolated node names no relay")
	_, err = f.JoinInvite(invite)
	require.NoError(t, err)
	assert.True(t, connected(fN, iN))
}

// A link of a newer format or of another network says so instead of failing silently.
func TestInviteOfAnotherNetwork(t *testing.T) {
	nN, n := startNode(t, "public", t.TempDir()+"/db", nil)
	defer nN.Stop()
	invite := link(t, nN)
	old := config.NetworkName
	config.NetworkName = "myfork"
	defer func() { config.NetworkName = old }()
	_, err := n.ParseInvite(invite)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"echofog"`)
}

// A public node with an address names only that address in its link, not the relays it happens to
// be connected to; a hidden node names them, since they are how it is reached from behind a NAT.
func TestOnlyAHiddenNodeNamesRelaysInItsLink(t *testing.T) {
	rN, _ := startNode(t, "relay", t.TempDir()+"/db", nil)
	defer rN.Stop()
	join := network.JoinPeerAddr(rN.Transport.Addr().String(), rN.ID)
	for _, mode := range []string{"", config.AccessHidden} {
		n, api := startNode(t, "node-"+mode, t.TempDir()+"/db", hiddenMode(mode))
		require.NoError(t, api.ConnectToPeer(join))
		require.Eventually(t, func() bool { return connected(n, rN) }, 5*time.Second, 50*time.Millisecond)
		inv, err := api.ParseInvite(link(t, n))
		require.NoError(t, err)
		if mode == "" {
			assert.Empty(t, inv.Relays, "a public node names no relay")
		} else {
			assert.NotEmpty(t, inv.Relays, "a hidden node names its relays")
		}
		n.Stop()
	}
}
