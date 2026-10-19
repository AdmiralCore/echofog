package privacy

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startNode(t *testing.T, name string) (*coreNode.Node, *node.NodeApi) {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	cfg.Network.NoAutoDial = true
	n := coreNode.NewNode(context.Background(), cfg, name)
	_, err := n.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	return n, node.NewNodeApi(n)
}

func connect(t *testing.T, from *node.NodeApi, fromN, to *coreNode.Node) {
	t.Helper()
	require.NoError(t, from.ConnectToPeer(network.JoinPeerAddr(to.Transport.Addr().String(), to.ID)))
	require.Eventually(t, func() bool { return to.Swarm.GetPeer(fromN.ID) != nil }, 5*time.Second, 50*time.Millisecond)
}

func messages(t *testing.T, a *node.NodeApi, peer string) []dto.MessageDTO {
	t.Helper()
	msgs, err := a.GetMessages(peer, 50, 0)
	require.NoError(t, err)
	return msgs
}

// A stranger's message lands in requests, not in the contacts; a contact's lands in its chat; a
// blocked peer's is confirmed and thrown away.
func TestStrangersBecomeRequestsAndBlockedPeersAreDropped(t *testing.T) {
	bN, bob := startNode(t, "bob")
	aN, alice := startNode(t, "alice") // a stranger to bob
	cN, carol := startNode(t, "carol") // bob's contact
	connect(t, alice, aN, bN)
	connect(t, carol, cN, bN)
	carolID := cN.ID.String()
	require.NoError(t, bob.NewContact(dto.ContactDTO{PeerID: carolID, Alias: "Carol"}))

	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "hi, I am a stranger"))
	require.NoError(t, carol.SendMessage(bob.GetMyPubKey(), "hi, it is Carol"))

	aliceID := aN.ID.String()
	require.Eventually(t, func() bool { return len(messages(t, bob, aliceID)) == 1 && len(messages(t, bob, carolID)) == 1 }, 15*time.Second, 50*time.Millisecond)

	reqs, err := bob.GetMessageRequests()
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	assert.Equal(t, aliceID, reqs[0].PeerID)
	contacts, _ := bob.GetContacts()
	for _, c := range contacts {
		assert.NotEqual(t, aliceID, c.PeerID, "a stranger must not become a contact by writing")
	}
	assert.False(t, bN.IsContact(aN.ID), "a request is not one of ours (hidden node gate, groups)")
	assert.False(t, bob.CanRing(aliceID), "strangers do not ring by default")
	assert.True(t, bob.CanRing(carolID))

	// blocked: confirmed, never stored
	require.NoError(t, bob.BlockPeer(aliceID))
	reqs, _ = bob.GetMessageRequests()
	assert.Empty(t, reqs, "blocking drops the request")
	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "you cannot ignore me"))
	require.NoError(t, carol.SendMessage(bob.GetMyPubKey(), "still here"))
	require.Eventually(t, func() bool { return len(messages(t, bob, carolID)) == 2 }, 15*time.Second, 50*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	assert.Len(t, messages(t, bob, aliceID), 1, "nothing new from a blocked peer")
	assert.False(t, bob.CanRing(aliceID))
	blocked, _ := bob.GetBlockedPeers()
	require.Len(t, blocked, 1)

	// a blocked contact is no longer one of ours either
	require.NoError(t, bob.BlockPeer(carolID))
	assert.False(t, bN.IsContact(cN.ID))
	assert.False(t, bob.CanRing(carolID))
	require.NoError(t, bob.UnblockPeer(carolID))
	assert.True(t, bob.CanRing(carolID), "unblocking restores the contact")
}

func TestAcceptingARequestMakesAContact(t *testing.T) {
	bN, bob := startNode(t, "bob")
	aN, alice := startNode(t, "alice")
	connect(t, alice, aN, bN)
	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "hello"))
	aliceID := aN.ID.String()
	require.Eventually(t, func() bool { r, _ := bob.GetMessageRequests(); return len(r) == 1 }, 15*time.Second, 50*time.Millisecond)

	require.NoError(t, bob.AcceptMessageRequest(aliceID, "Alice"))
	r, _ := bob.GetMessageRequests()
	assert.Empty(t, r)
	assert.True(t, bN.IsContact(aN.ID))
	assert.True(t, bob.CanRing(aliceID))
}

func TestPrivacySettings(t *testing.T) {
	bN, bob := startNode(t, "bob")
	aN, alice := startNode(t, "alice")
	connect(t, alice, aN, bN)
	aliceID := aN.ID.String()

	assert.Equal(t, coreNode.DefaultPrivacy, bob.GetPrivacy())
	assert.Error(t, bob.SetPrivacy(node.PrivacyDTO{Messages: "nobody", Calls: "contacts"}))

	// only contacts may write: the stranger's message is dropped, not even a request
	require.NoError(t, bob.SetPrivacy(node.PrivacyDTO{Messages: coreNode.WhoContacts, Calls: coreNode.WhoEveryone}))
	assert.True(t, bob.CanRing(aliceID), "calls from everybody")
	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "anyone home?"))
	time.Sleep(2 * time.Second)
	assert.Empty(t, messages(t, bob, aliceID))
	r, _ := bob.GetMessageRequests()
	assert.Empty(t, r)

	require.NoError(t, bob.SetPrivacy(node.PrivacyDTO{Messages: coreNode.WhoEveryone, Calls: coreNode.WhoNobody}))
	require.NoError(t, bob.NewContact(dto.ContactDTO{PeerID: aliceID, Alias: "Alice"}))
	assert.False(t, bob.CanRing(aliceID), "nobody rings, not even a contact")

	// the choice is kept for the next start
	again := node.NewNodeApi(bN)
	assert.Equal(t, coreNode.WhoNobody, again.GetPrivacy().Calls)
}
