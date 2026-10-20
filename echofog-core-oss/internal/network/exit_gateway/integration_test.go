package exit_gateway

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/bwprobe"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSwarm(t *testing.T) (*swarm.Swarm, *network.QuicTransport) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	im, store := testutil.SetupTestIdentity(t)
	t.Cleanup(store.CloseAndClear)

	account, err := im.GetActiveAccount()
	require.NoError(t, err)
	priv, pub, err := account.GetIdentityKey()
	require.NoError(t, err)
	locked, err := priv.Open()
	require.NoError(t, err)
	defer locked.Destroy()

	var pubKey types.PeerPublicKey
	copy(pubKey[:], pub)
	_, tls := testutil.GetRandomKeyAndTls(t)
	tr, err := network.NewQUICTransport("127.0.0.1:0", tls, network.GetQuicConfig(), im, config.CurrentProtocolVersion)
	require.NoError(t, err)
	require.NoError(t, tr.ListenEarly(ctx))
	t.Cleanup(func() { tr.Close() })

	d := dispatcher.NewDispatcher(ctx)
	go d.Start()
	sw := swarm.NewSwarm(ctx, tr, types.PeerPubKeyToID(pubKey), d, im, tr.ConnChan(), store.St, config.DefaultConfig(""))
	t.Cleanup(sw.Close)
	return sw, tr
}

func socksConnect(t *testing.T, proxy, target string) net.Conn {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	require.NoError(t, err)
	c.SetDeadline(time.Now().Add(6 * time.Second))
	_, err = c.Write([]byte{5, 1, 0})
	require.NoError(t, err)
	rep := make([]byte, 2)
	_, err = io.ReadFull(c, rep)
	require.NoError(t, err)

	host, portStr, _ := net.SplitHostPort(target)
	ip := net.ParseIP(host).To4()
	var port int
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	req := append([]byte{5, 1, 0, 1}, ip...)
	req = append(req, byte(port>>8), byte(port))
	_, err = c.Write(req)
	require.NoError(t, err)
	resp := make([]byte, 10)
	_, err = io.ReadFull(c, resp)
	require.NoError(t, err)
	require.Equal(t, byte(0), resp[1], "socks reply code")
	return c
}

func TestEndToEndSocksThroughRealSwarm(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)

	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)

	exitID := clientSwarm.GetAllPeers()[0].ID()
	srv := NewSOCKS5Server("127.0.0.1:0", NewP2PDialer(SwarmOpener(clientSwarm, exitID)))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { srv.Stop() })

	c := socksConnect(t, srv.Addr().String(), echoServer(t))
	defer c.Close()

	msg := []byte("through the swarm and back")
	_, err := c.Write(msg)
	require.NoError(t, err)
	got := make([]byte, len(msg))
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, msg, got)

	acc, _, bytes := svc.Stats()
	assert.Equal(t, uint64(1), acc)
	assert.Greater(t, bytes, uint64(0))
}

func TestSwarmExitRefusedWhenOptedOut(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)

	svc := NewExitService()
	svc.allowPrivate.Store(true)
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool { return len(clientSwarm.GetAllPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	exitID := clientSwarm.GetAllPeers()[0].ID()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := NewP2PDialer(SwarmOpener(clientSwarm, exitID)).DialExitStreamConfirmed(ctx, echoServer(t))
	assert.ErrorIs(t, err, ErrExitDenied)
}

func TestRealSwarmDownloadProbeAndTestDial(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)

	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	AttachToSwarm(exitSwarm, svc)
	bwprobe.NewForSwarm(exitSwarm)
	clientProbe := bwprobe.NewForSwarm(clientSwarm)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)
	exitID := clientSwarm.GetAllPeers()[0].ID()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	kbps, err := clientProbe.MeasureDownload(ctx, exitID)
	require.NoError(t, err)
	assert.Greater(t, kbps, 1000, "loopback should deliver far more than 1 Mbit/s")

	conn, err := NewP2PDialer(SwarmOpener(clientSwarm, exitID)).DialExitStream(ctx, echoServer(t))
	require.NoError(t, err)
	conn.Close()
}
