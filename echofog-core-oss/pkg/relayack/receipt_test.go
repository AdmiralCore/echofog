package relayack

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// startAt starts a node on the database db: a new account, or the one there (a restart).
func startAt(t *testing.T, name, db string, fresh bool) *coreNode.Node {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = db
	cfg.Network.BootstrapNodes = nil
	cfg.Network.NoAutoDial = true
	n := coreNode.NewNode(context.Background(), cfg, name)
	if fresh {
		_, err := n.Register([]byte("pass123"), []byte("pass123"))
		require.NoError(t, err)
	} else {
		require.NoError(t, n.Login([]byte("pass123")))
	}
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	return n
}

// The recipient confirms to the relay each kept message it got, and the relay drops the message
// on that receipt. (Here the first hand-over, on the connection the restarted recipient opens,
// is lost, as happens on a fresh connection; the second pass brings it.)
func TestMailboxDropsMailOnTheRecipientsReceipt(t *testing.T) {
	aN, a := start(t, "alice")
	rN, _ := start(t, "relay")
	zDB := t.TempDir() + "/db"
	zN := startAt(t, "zed", zDB, true)
	zKey := node.NewNodeApi(zN).GetMyPubKey()
	connect(t, aN, rN)
	connect(t, zN, rN)
	meetAndLeave(t, aN, a, zN)
	require.Eventually(t, func() bool { return rN.Swarm.GetPeer(zN.ID) == nil }, 10*time.Second, 100*time.Millisecond)

	require.NoError(t, a.SendMessage(zKey, "kept for you"))
	require.Eventually(t, func() bool { return rN.MailboxManager.Count(zN.ID) == 1 }, 10*time.Second, 50*time.Millisecond,
		"the relay did not keep the message")

	aN.Stop() // the relay is the only way the message can come now
	zN = startAt(t, "zed", zDB, false)
	inbox := node.NewNodeApi(zN).MessageHandler()
	connect(t, zN, rN)
	require.Eventually(t, func() bool {
		select {
		case m := <-inbox:
			return m.Text == "kept for you"
		default:
			return false
		}
	}, 10*time.Second, 20*time.Millisecond, "the kept message did not arrive")
	require.Eventually(t, func() bool { return rN.RelayServer.Stats().Receipts == 1 }, 4*time.Second, 50*time.Millisecond,
		"the recipient did not confirm the message to the relay")
	require.Eventually(t, func() bool { return rN.MailboxManager.Count(zN.ID) == 0 }, 4*time.Second, 50*time.Millisecond,
		"the mail stayed after the recipient got it")
}
