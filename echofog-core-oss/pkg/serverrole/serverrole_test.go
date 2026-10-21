package serverrole

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/api/types"
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

func serverRole(c *config.AppConfig) {
	c.Role = config.RoleServer
	c.Relay.Server.Enabled = true
}

func join(n *coreNode.Node) string { return network.JoinPeerAddr(n.Transport.Addr().String(), n.ID) }

func connected(a, b *coreNode.Node) bool {
	return a.Swarm.GetPeer(b.ID) != nil && b.Swarm.GetPeer(a.ID) != nil
}

// chatKeys are the keys a node writes for chats, sessions, one-time prekeys and message requests:
// none may appear on a server because a stranger wrote to it.
var chatKeys = []string{"opk:", "opkused:", "sess:", "m:", "outbox/", "seen/", "msgreq:", "contacts:", "fogack1:", "fogreset1:"}

// snapshot is every key of the node's database under chatKeys, with a copy of its value.
func snapshot(t *testing.T, n *coreNode.Node) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range chatKeys {
		entries, err := n.Storage.FindValues([]byte(p), 0)
		require.NoError(t, err)
		for _, e := range entries {
			out[string(e.Key)] = append([]byte(nil), e.Value...)
		}
	}
	return out
}

func sameSnapshot(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

// A server-role node runs no chat, call or file service, hands out no pre-key bundle and writes
// nothing to disk for the frames a stranger sends it of those kinds. A client node is unchanged.
func TestServerRoleRunsNoChatCallsOrFiles(t *testing.T) {
	sN, s := startNode(t, "server", t.TempDir()+"/db", serverRole)
	defer sN.Stop()
	cN, _ := startNode(t, "client", t.TempDir()+"/db", nil)
	defer cN.Stop()

	assert.Equal(t, config.RoleServer, sN.Role())
	assert.Nil(t, sN.Messenger, "no messenger on a server")
	assert.Nil(t, sN.Contact, "no contacts on a server")
	assert.Nil(t, sN.CallManager, "no call manager on a server")
	assert.Nil(t, sN.Files, "no file transfer on a server")
	for _, mt := range []net_types.MessageType{net_types.TypeChatMessage, net_types.TypeCallSignaling, net_types.TypeCallMediaAudio,
		net_types.TypeCallMediaVideo, net_types.TypeFileXfer, net_types.TypeLocalPublicKeyBundleRequest} {
		assert.True(t, sN.Swarm.Drops(mt), "a server must drop %v on arrival", mt)
		assert.False(t, cN.Swarm.Drops(mt), "a client must not drop %v", mt)
	}
	assert.False(t, sN.Swarm.Drops(net_types.TypeRelayMessageRequest), "mail for others must still reach a server")

	// the client role keeps everything
	assert.Equal(t, config.RoleClient, cN.Role())
	assert.NotNil(t, cN.Messenger)
	assert.NotNil(t, cN.Contact)
	assert.NotNil(t, cN.CallManager)
	assert.NotNil(t, cN.Files)

	// the API refuses chat on a server instead of crashing
	assert.Error(t, s.SendMessage(s.GetMyPubKey(), "hi"))
	_, err := s.GetContacts()
	assert.Error(t, err)

	require.NoError(t, s.ConnectToPeer(join(cN)))
	require.Eventually(t, func() bool { return connected(sN, cN) }, 5*time.Second, 50*time.Millisecond)

	// the client can fetch nobody's bundle from the server, and the server mints no prekey for it
	before := snapshot(t, sN)
	dropped := sN.Swarm.DroppedFrames()
	_, err = cN.Swarm.GetBundle(sN.ID)
	assert.Error(t, err, "a server must not hand out a pre-key bundle")

	// a signed chat envelope, call signals and media, and file frames: all dropped on arrival
	chat := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  cN.ID.ToBytes(),
		TargetId:  sN.ID.ToBytes(),
		Timestamp: uint64(time.Now().UnixNano()),
		Payload:   &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: 1}},
	}
	require.NoError(t, cN.Swarm.SendDataForPeer(sN.ID, net_types.TypeChatMessage, chat))
	for _, mt := range []net_types.MessageType{net_types.TypeCallSignaling, net_types.TypeCallMediaAudio, net_types.TypeCallMediaVideo, net_types.TypeFileXfer} {
		require.NoError(t, cN.Swarm.SendRawFrame(sN.ID, mt, []byte("junk that no handler reads")))
	}
	require.Eventually(t, func() bool { return sN.Swarm.DroppedFrames() >= dropped+6 }, 5*time.Second, 20*time.Millisecond,
		"the bundle request, the chat message, the call and the file frames must be counted as dropped")
	time.Sleep(200 * time.Millisecond)
	assert.True(t, sameSnapshot(before, snapshot(t, sN)), "a server wrote chat, session or prekey data for a stranger")

	// a client answers a bundle request as before, with a fresh one-time prekey (which is also what
	// shows the snapshot above would have seen one)
	clientBefore := snapshot(t, cN)
	_, err = sN.Swarm.GetBundle(cN.ID)
	assert.NoError(t, err, "a client must still hand out its bundle")
	assert.False(t, sameSnapshot(clientBefore, snapshot(t, cN)), "a client's bundle answer should store a new one-time prekey")
}

// Two clients that only share a server-role relay: mail for one that is offline is kept there and
// delivered when it comes back, and the server still runs the exit and its account service.
func TestServerRoleRelaysMailAndRunsTheExit(t *testing.T) {
	rN, r := startNode(t, "relay", t.TempDir()+"/db", serverRole)
	defer rN.Stop()
	aN, a := startNode(t, "alice", t.TempDir()+"/db", nil)
	defer aN.Stop()
	bDB := t.TempDir() + "/db"
	bN, b := startNode(t, "bob", bDB, nil)

	require.NoError(t, a.ConnectToPeer(join(rN)))
	require.NoError(t, b.ConnectToPeer(join(rN)))
	require.Eventually(t, func() bool { return connected(aN, rN) && connected(bN, rN) }, 5*time.Second, 50*time.Millisecond)
	require.True(t, aN.Swarm.GetPeer(rN.ID).Relays(), "a server-role relay announces itself as a relay")

	// they meet once directly, so that each knows the other's bundle, and say hello
	require.NoError(t, a.ConnectToPeer(join(bN)))
	require.Eventually(t, func() bool { return connected(aN, bN) }, 5*time.Second, 50*time.Millisecond)
	bInbox := b.MessageHandler()
	require.NoError(t, a.SendMessage(b.GetMyPubKey(), "hello"))
	require.True(t, waitFor(bInbox, "hello", 10*time.Second))
	bobPub := b.GetMyPubKey()

	// Bob goes offline; Alice's next message can only go to the relay's mailbox
	bN.Stop()
	require.Eventually(t, func() bool { return aN.Swarm.GetPeer(bN.ID) == nil && rN.Swarm.GetPeer(bN.ID) == nil },
		10*time.Second, 50*time.Millisecond)
	require.NoError(t, a.SendMessage(bobPub, "kept for you"))
	require.Eventually(t, func() bool { return rN.RelayServer.Stats().Stored >= 1 }, 10*time.Second, 50*time.Millisecond,
		"the server-role relay must keep mail for an offline peer")

	// Bob comes back and connects only to the relay: the mail is waiting for him
	bN, b = startNode(t, "bob", bDB, nil)
	defer bN.Stop()
	inbox := b.MessageHandler()
	require.NoError(t, b.ConnectToPeer(join(rN)))
	assert.True(t, waitFor(inbox, "kept for you", 20*time.Second), "the kept message never arrived")
	assert.Positive(t, rN.RelayServer.Stats().Delivered, "the relay must count the delivery")

	// the exit runs on a server-role node
	require.NoError(t, r.StartSharing(node.SharingConfig{UploadKbps: 10000}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acc, err := exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(aN.Swarm, rN.ID)).QueryAccount(ctx)
	require.NoError(t, err)
	assert.True(t, acc.Enabled, "the server's exit must answer")

	// and it publishes no bundle of its own: nobody can start a chat with it from the DHT
	_, err = aN.DhtEngine.Get(swarm.BundleKey(rN.ID))
	assert.Error(t, err, "a server must not publish a pre-key bundle")
}

func waitFor(inbox <-chan *dto.MessageDTO, text string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case m := <-inbox:
			if !m.IsMine && strings.Contains(m.Text, text) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
