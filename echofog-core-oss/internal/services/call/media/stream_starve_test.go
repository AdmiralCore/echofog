package media

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// A stream that fills every packet with media must not starve the connection's streams: pings,
// call signaling and chat ride on them. With datagrams always waiting, QUIC put one in each packet
// and gave streams only what was left, nothing once a frame filled the packet; a peer streaming
// for long then stopped answering, and everything else between the two broke.
func TestMediaFloodLeavesRoomForStreams(t *testing.T) {
	// a link of a few Mbit/s, as a home uplink is to a 4K stream: the datagram queue never empties
	t.Setenv("ECHOFOG_CONGESTION", "brutal:8")
	sendSw, sendTr := newQUICSwarm(t)
	recvSw, recvTr := newQUICSwarm(t)
	require.NoError(t, sendTr.DialEarly(context.Background(), recvTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(sendSw.GetAllPeers()) > 0 && len(recvSw.GetAllPeers()) > 0
	}, 5*time.Second, 20*time.Millisecond)
	peer := sendSw.GetAllPeers()[0].ID()
	recvSw.RegisterFastFrameHandler(net_types.TypeCallMediaVideo, func([]byte, types.PeerID) error { return nil })

	// the largest frame the connection takes now: each one fills a packet
	size := 1200
	var tooLarge *network.DatagramTooLargeError
	if err := sendSw.SendUnreliableFrame(peer, net_types.TypeCallMediaVideo, make([]byte, 4096)); errors.As(err, &tooLarge) {
		size = tooLarge.Max
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		frame := make([]byte, size)
		for !stop.Load() {
			_ = sendSw.SendUnreliableFrame(peer, net_types.TypeCallMediaVideo, frame)
		}
	}()
	defer func() { stop.Store(true); <-done }()

	time.Sleep(300 * time.Millisecond) // the queues fill
	answered := 0
	for i := 0; i < 5; i++ {
		start := time.Now()
		if sendSw.Answers(peer) {
			answered++
		}
		t.Logf("ping %d under the flood: %v", i, time.Since(start))
	}
	require.GreaterOrEqual(t, answered, 4, "the peer streaming media must still answer on a stream")
}
