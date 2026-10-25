package serverrole

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two people who never met, each behind a NAT, connected only to a server-role relay, and a DHT
// that does not hand out their key bundles: the first message still arrives. The keys travel with
// a knock through the relay, and the message follows once the sender has them; the sender sees it
// go from "waiting for keys" to "sent" to "delivered".
func TestFirstMessageWithoutTheDHTGoesThroughTheRelay(t *testing.T) {
	rN, _ := startNode(t, "relay", t.TempDir()+"/db", serverRole)
	defer rN.Stop()
	aN, a := startNode(t, "alice", t.TempDir()+"/db", nil)
	defer aN.Stop()
	bN, b := startNode(t, "bob", t.TempDir()+"/db", nil)
	defer bN.Stop()

	noDHT := func([]byte) ([]byte, error) { return nil, errors.New("the DHT does not answer") }
	aN.Swarm.SetBundleFetcher(noDHT)
	bN.Swarm.SetBundleFetcher(noDHT)
	// on one machine a dial between them would succeed; behind two NATs it does not
	aN.Messenger.SetConnector(func(types.PeerID) {})
	bN.Messenger.SetConnector(func(types.PeerID) {})

	require.NoError(t, a.ConnectToPeer(join(rN)))
	require.NoError(t, b.ConnectToPeer(join(rN)))
	require.Eventually(t, func() bool { return connected(aN, rN) && connected(bN, rN) }, 5*time.Second, 50*time.Millisecond)
	require.False(t, connected(aN, bN), "Alice and Bob must not be connected to each other")

	var mu sync.Mutex
	var states []string
	a.OnMessageState(func(ev node.MessageStateDTO) {
		mu.Lock()
		states = append(states, ev.State)
		mu.Unlock()
	})
	seen := func(state string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range states {
			if s == state {
				return true
			}
		}
		return false
	}

	inbox := b.MessageHandler()
	inv, err := a.InviteToCall(b.GetMyPubKey())
	require.NoError(t, err)
	assert.Equal(t, messenger.StateKeys, inv.State, "no keys yet: the invitation waits for them")
	bob := bN.ID
	assert.Equal(t, map[string]string{inv.MessageID: messenger.StateKeys}, aN.Messenger.PendingStates(bob))

	assert.True(t, waitFor(inbox, "fogcall://", 30*time.Second), "the invitation never reached Bob")
	require.Eventually(t, func() bool { return len(aN.Messenger.PendingStates(bob)) == 0 }, 20*time.Second, 100*time.Millisecond,
		"Bob's receipt never came back")
	assert.True(t, seen(messenger.StateSent), "the UI is told when a relay took it")
	assert.True(t, seen(messenger.StateDelivered), "and when Bob confirmed it")
	assert.False(t, connected(aN, bN), "it all went through the relay")
	assert.Positive(t, rN.RelayServer.Stats().Forwarded, "the relay passed the messages on")

	// Bob can answer at once: he got Alice's keys with the knock
	aInbox := a.MessageHandler()
	require.NoError(t, b.SendMessage(a.GetMyPubKey(), "coming"))
	assert.True(t, waitFor(aInbox, "coming", 20*time.Second), "Bob's answer never reached Alice")
}
