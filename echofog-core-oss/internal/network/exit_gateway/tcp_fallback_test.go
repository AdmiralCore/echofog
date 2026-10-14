package exit_gateway

import (
	"context"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTCPSwarm builds a swarm whose peers can only use the TCP/TLS fallback, as on a network
// that blocks UDP.
func newTCPSwarm(t *testing.T) (*swarm.Swarm, *network.FallbackTransport) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	im, store := testutil.SetupTestIdentity(t)
	t.Cleanup(store.CloseAndClear)
	account, err := im.GetActiveAccount()
	require.NoError(t, err)
	_, pub, err := account.GetIdentityKey()
	require.NoError(t, err)
	var pubKey types.PeerPublicKey
	copy(pubKey[:], pub)

	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	q, err := network.NewQUICTransport("127.0.0.1:0", tlsCfg, network.GetQuicConfig(), im, config.CurrentProtocolVersion)
	require.NoError(t, err)
	mim := network.MimicryConfigFrom(network.MimicryOptions{StealthPassword: ""})
	mim.FallbackAddr = ""
	// the TCP camouflage needs an ECDSA cert (Chrome offers no ed25519 signature algorithm)
	camoCert, err := mimicry.GenerateCamouflageCert("www.microsoft.com")
	require.NoError(t, err)
	tcp := network.NewTCPTransport(camoCert, mim,
		network.NewDefaultPeerAuthenticator(im, config.CurrentProtocolVersion, 5*time.Second))
	f := network.NewFallbackTransport(q, tcp, network.ModeTCP, "127.0.0.1:0")
	require.NoError(t, f.ListenEarly(ctx))
	t.Cleanup(func() { f.Close() })

	d := dispatcher.NewDispatcher(ctx)
	go d.Start()
	sw := swarm.NewSwarm(ctx, f, types.PeerPubKeyToID(pubKey), d, im, f.ConnChan(), store.St, config.DefaultConfig(""))
	t.Cleanup(sw.Close)
	return sw, f
}

func TestExitAndMediaOverTCPFallback(t *testing.T) {
	exitSwarm, exitNet := newTCPSwarm(t)
	clientSwarm, clientNet := newTCPSwarm(t)

	svc := NewExitService()
	svc.AllowPrivateTargets(true)
	svc.SetEnabled(true)
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientNet.DialEarly(context.Background(), exitNet.TCPAddr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)
	exitPeer := clientSwarm.GetAllPeers()[0]
	assert.Equal(t, router.ProtoTCPTLS, exitPeer.ActiveProtocol())

	// 1. TCP through the exit via SOCKS5
	srv := NewSOCKS5Server("127.0.0.1:0", NewP2PDialer(SwarmOpener(clientSwarm, exitPeer.ID())))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { srv.Stop() })
	c := socksConnect(t, srv.Addr().String(), echoServer(t))
	defer c.Close()
	msg := []byte("tcp proxy over tcp/tls")
	_, err := c.Write(msg)
	require.NoError(t, err)
	got := make([]byte, len(msg))
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, msg, got)

	// 2. UDP through the exit
	echo := udpEchoServer(t)
	u, err := NewP2PDialer(SwarmOpener(clientSwarm, exitPeer.ID())).DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()
	require.NoError(t, u.WriteTo([]byte("udp over tcp"), echo.LocalAddr().String()))
	b, _ := readWithin(t, u, 3*time.Second)
	assert.Equal(t, "udp over tcp", string(b))

	// 3. call media frames (QUIC datagrams normally) arrive over the TCP datagram channel
	got3 := make(chan []byte, 8)
	exitSwarm.RegisterFastFrameHandler(net_types.TypeCallMediaAudio, func(p []byte, _ types.PeerID) error {
		got3 <- append([]byte(nil), p...)
		return nil
	})
	for i := 0; i < 5; i++ {
		require.NoError(t, exitPeer.SendRawUnreliable(net_types.TypeCallMediaAudio, []byte("opus frame "+strconv.Itoa(i))))
	}
	for i := 0; i < 5; i++ {
		select {
		case f := <-got3:
			assert.Equal(t, "opus frame "+strconv.Itoa(i), string(f))
		case <-time.After(3 * time.Second):
			t.Fatalf("media frame %d did not arrive", i)
		}
	}

}
