package media

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

func newQUICSwarm(t *testing.T) (*swarm.Swarm, *network.QuicTransport) {
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

// fallbackCounter counts frames the outbox had to move to the reliable path.
type fallbackCounter struct {
	*swarm.Swarm
	reliable atomic.Int64
}

func (f *fallbackCounter) SendRawFrame(peer types.PeerID, t net_types.MessageType, b []byte) error {
	f.reliable.Add(1)
	return f.Swarm.SendRawFrame(peer, t, b)
}

// Exit UDP keeps a margin under the datagram limit the connection reports after path MTU
// discovery (udpDgramSlack in exit_gateway), for frames quic-go might accept and then drop.
// Media never asks for the limit: it cuts frames to DatagramBudget, what the 1280-byte starting
// packet takes, so PMTUD only ever adds room. This checks budget-sized frames arrive over a real
// QUIC connection before and after PMTUD, and logs how frames at the reported limit fare.
func TestMediaFramesAtTheBudgetSurvivePMTUD(t *testing.T) {
	sendSw, sendTr := newQUICSwarm(t)
	recvSw, recvTr := newQUICSwarm(t)
	require.NoError(t, sendTr.DialEarly(context.Background(), recvTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(sendSw.GetAllPeers()) > 0 && len(recvSw.GetAllPeers()) > 0
	}, 5*time.Second, 20*time.Millisecond)
	peer := sendSw.GetAllPeers()[0].ID()

	var mu sync.Mutex
	got := map[uint32]bool{}
	recvSw.RegisterFastFrameHandler(net_types.TypeCallMediaVideo, func(b []byte, _ types.PeerID) error {
		if len(b) >= 4 {
			mu.Lock()
			got[binary.BigEndian.Uint32(b)] = true
			mu.Unlock()
		}
		return nil
	})
	arrived := func(from, to uint32) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for i := from; i < to; i++ {
			if got[i] {
				n++
			}
		}
		return n
	}
	reportedMax := func() int {
		var tooLarge *network.DatagramTooLargeError
		err := sendSw.SendUnreliableFrame(peer, net_types.TypeCallMediaVideo, make([]byte, 4096))
		require.True(t, errors.As(err, &tooLarge), "oversized frame accepted: %v", err)
		return tooLarge.Max
	}

	sender := &fallbackCounter{Swarm: sendSw}
	out := NewOutbox(sender)
	defer out.Close()
	var seq uint32
	burst := func(n, size int) (from, to uint32) {
		from = seq
		for i := 0; i < n; i++ {
			b := make([]byte, size)
			binary.BigEndian.PutUint32(b, seq)
			seq++
			require.True(t, out.Send(peer, ClassVideo, net_types.TypeCallMediaVideo, b))
			time.Sleep(500 * time.Microsecond) // pace under the loopback queues: only size may lose frames
		}
		return from, seq
	}
	const n = 200
	waitAll := func(from, to uint32) int {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && arrived(from, to) < int(to-from) {
			time.Sleep(20 * time.Millisecond)
		}
		return arrived(from, to)
	}

	beforeMax := reportedMax()
	from, to := burst(n, DatagramBudget)
	require.Equal(t, n, waitAll(from, to), "before PMTUD (reported max %d)", beforeMax)

	// PMTUD on loopback grows packets to quic-go's 1452-byte ceiling within a few round trips
	require.Eventually(t, func() bool { return reportedMax() > DatagramBudget+48 }, 10*time.Second, 50*time.Millisecond)
	afterMax := reportedMax()

	// frames exactly at the reported limit: logged, not asserted, as this is quic-go's business
	raw := seq
	for i := 0; i < n; i++ {
		b := make([]byte, afterMax)
		binary.BigEndian.PutUint32(b, seq)
		seq++
		require.NoError(t, sendSw.SendUnreliableFrame(peer, net_types.TypeCallMediaVideo, b))
		time.Sleep(500 * time.Microsecond)
	}
	t.Logf("reported max %d -> %d; raw frames at the reported max: %d/%d arrived", beforeMax, afterMax, waitAll(raw, seq), n)

	from, to = burst(n, DatagramBudget)
	require.Equal(t, n, waitAll(from, to), "after PMTUD (reported max %d)", afterMax)
	require.Zero(t, sender.reliable.Load(), "budget-sized frames should never need the reliable path")
}
