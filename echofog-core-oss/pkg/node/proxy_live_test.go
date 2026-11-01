package node

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/stretchr/testify/require"
)

// A live run through a real Tor: EF_LIVE_TOR=127.0.0.1:9050 (tor, or 9150 for Tor Browser) and,
// optionally, EF_LIVE_RELAY=<annotated relay address>; the first built-in bootstrap node otherwise.
// The node joins through the relay over Tor, and no TCP connection of its own goes to the relay.
func TestLiveJoinThroughTor(t *testing.T) {
	proxy := os.Getenv("EF_LIVE_TOR")
	if proxy == "" {
		t.Skip("set EF_LIVE_TOR to a running Tor SOCKS port")
	}
	relay := os.Getenv("EF_LIVE_RELAY")
	if relay == "" {
		relay = config.DefaultBootstrapNodes[0]
	}
	info := network.ParseAddrInfo(relay)
	require.True(t, info.Named, "the relay address needs its peer ID")

	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = []string{relay}
	cfg.Network.Proxy = proxy
	n := NewNode(context.Background(), cfg, "live-tor")
	_, err := n.Register([]byte("pass123"), []byte(""))
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)

	require.Eventually(t, func() bool { return n.Swarm.GetPeer(info.ID) != nil }, 3*time.Minute, time.Second,
		"no link to the relay through Tor")
	p := n.Swarm.GetPeer(info.ID)
	t.Logf("joined through Tor in %s; relay link %s, peers %d, guards %v", time.Since(start).Round(time.Millisecond),
		p.ActiveProtocol(), len(n.Swarm.GetAllPeers()), n.Swarm.Guards())

	if runtime.GOOS == "windows" {
		out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
		require.NoError(t, err)
		host, _, _ := strings.Cut(info.Addr, ":")
		for _, line := range strings.Split(string(out), "\n") {
			require.NotContains(t, line, host, "a TCP connection straight to the relay, around Tor")
		}
	}
}
