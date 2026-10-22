package pkg

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/stretchr/testify/require"
)

// A node reachable only over WebSocket (as it would be behind a CDN) is joined by another node
// through a wss-style bootstrap address, and a message flows between them.
func TestNodesConnectOverWebSocket(t *testing.T) {
	aN, a := startNodeWith(t, "ws-listener", func(c *config.AppConfig) {
		c.Network.WSListenAddr = "127.0.0.1:0"
		c.Network.WSPath = "/echofog"
	})
	require.Eventually(t, func() bool { return aN.WS != nil && aN.WS.Addr() != nil }, 5*time.Second, 50*time.Millisecond)
	wsEntry := network.JoinPeerAddr("ws://"+aN.WS.Addr().String()+"/echofog", aN.ID)

	bN, b := startNodeWith(t, "ws-dialer", func(c *config.AppConfig) {
		c.Network.BootstrapNodes = []string{wsEntry}
	})
	_ = bN

	require.Eventually(t, func() bool { return len(a.GetPeers()) > 0 && len(b.GetPeers()) > 0 }, 10*time.Second, 100*time.Millisecond)

	// the peers exchange keys and a message over the WebSocket path
	inbox := a.MessageHandler()
	require.Eventually(t, func() bool {
		select {
		case m := <-inbox:
			return m.Text == "hello through a CDN"
		default:
			_ = b.SendMessage(a.GetMyPubKey(), "hello through a CDN")
			return false
		}
	}, 20*time.Second, 200*time.Millisecond)
}
