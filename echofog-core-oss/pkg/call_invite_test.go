package pkg

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/node"
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
	n := coreNode.NewNode(context.Background(), cfg, name)
	_, err := n.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	return n, node.NewNodeApi(n)
}

// The callee's node learns about a call from the invitation message, and both end up in one call.
func TestCallInviteReachesTheCalleeAndBothJoin(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	bobInbox := bob.MessageHandler()

	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	callID, coordinator, err := alice.InviteToCall(bob.GetMyPubKey())
	require.NoError(t, err)
	assert.Equal(t, aliceN.ID.String(), coordinator)

	var gotCall, gotCoord string
	require.Eventually(t, func() bool {
		select {
		case msg := <-bobInbox:
			if msg.IsMine {
				return false
			}
			id, coord, ok := node.ParseCallInvite(msg.Text)
			if ok {
				gotCall, gotCoord = id, coord
			}
			return ok
		default:
			return false
		}
	}, 5*time.Second, 20*time.Millisecond, "Bob never saw the invitation")
	assert.Equal(t, callID, gotCall)
	assert.Equal(t, coordinator, gotCoord)

	_, err = alice.JoinCall(callID, coordinator)
	require.NoError(t, err)
	_, err = bob.JoinCall(gotCall, gotCoord)
	require.NoError(t, err)
	t.Cleanup(func() { _ = alice.LeaveCall(); _ = bob.LeaveCall() })

	require.Eventually(t, func() bool {
		return len(alice.GetCallRoster().Members) == 2 && len(bob.GetCallRoster().Members) == 2
	}, 10*time.Second, 100*time.Millisecond, "both nodes should see two members in the call")
}

func TestParseCallInvite(t *testing.T) {
	id, coord, ok := node.ParseCallInvite("fogcall://abc123/def456")
	assert.True(t, ok)
	assert.Equal(t, "abc123", id)
	assert.Equal(t, "def456", coord)
	for _, bad := range []string{"", "hello", "fogcall://", "fogcall://abc", "fogcall:///def", "fogcall://abc/"} {
		_, _, ok := node.ParseCallInvite(bad)
		assert.False(t, ok, bad)
	}
}
