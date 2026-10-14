package pkg

import (
	"bytes"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/storage"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The DHT gives out and accepts DHT records only: a peer can neither read nor overwrite anything
// else of another node's database (the identity, contacts, messages).
func TestDHTCannotReachTheDatabase(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	_, err := aliceN.DhtEngine.Get([]byte("system:identity:root"))
	assert.Error(t, err, "a peer read the identity root of another node")

	// records under keys that are not the publisher's own are not even held
	planted := []byte("contacts:planted")
	require.NoError(t, aliceN.DhtEngine.Set(planted, []byte("not yours")))
	// a record alice may publish arrives, so the planted one had its chance too
	own := []byte("bundle:" + aliceN.ID.String())
	require.NoError(t, aliceN.DhtEngine.Set(own, []byte("alice's")))
	require.Eventually(t, func() bool { return bobN.Storage.Exists(dht.RecordKey(own)) }, 5*time.Second, 50*time.Millisecond)
	assert.False(t, bobN.Storage.Exists(planted), "a peer wrote into another node's contacts")
	assert.False(t, bobN.Storage.Exists(dht.RecordKey(planted)), "a record nobody owns was stored")

	// nor can alice publish under bob's key
	bobs := []byte("bundle:" + bobN.ID.String())
	require.NoError(t, aliceN.DhtEngine.Set(bobs, []byte("fake bundle")))
	time.Sleep(300 * time.Millisecond)
	if v, err := bobN.DhtEngine.Get(bobs); err == nil {
		assert.NotEqual(t, "fake bundle", string(v), "a record in another node's name was stored")
	}
}

// What the user keeps is stored encrypted: a message and a contact name do not appear in the
// database files.
func TestMessagesAndContactsAreEncryptedOnDisk(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	inbox := bob.MessageHandler()
	secret := "the treasure is under the third oak"
	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), secret))
	require.Eventually(t, func() bool {
		select {
		case m := <-inbox:
			return m.Text == secret
		default:
			return false
		}
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, aliceN.Contact.SetPeerAlias(bobN.ID.String(), "Uncle Bob"))
	alias, err := aliceN.Contact.GetPeerAlias(bobN.ID.String())
	require.NoError(t, err)
	assert.Equal(t, "Uncle Bob", alias)

	msgs, err := alice.GetMessages(bobN.ID.String(), 10, 0)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "the history is readable through the API")

	// underneath, neither the text, nor the name, nor whom it was with
	for _, n := range []*coreNode.Node{aliceN, bobN} {
		sealed, ok := n.Storage.(*storage.Sealed)
		require.True(t, ok, "the node does not use the encrypted storage")
		all, err := sealed.Inner().FindValues(nil, 0)
		require.NoError(t, err)
		other := bobN.ID
		if n == bobN {
			other = aliceN.ID
		}
		for _, e := range all {
			for _, s := range [][]byte{[]byte(secret), []byte("Uncle Bob"), []byte(other.String()), other[:]} {
				assert.False(t, bytes.Contains(e.Key, s) || bytes.Contains(e.Value, s), "%q shows %q", e.Key, s)
			}
		}
	}
}
