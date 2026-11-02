package bwprobe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingTransport counts the bytes the prober sends and drops them.
type countingTransport struct {
	bytes atomic.Int64
	h     func([]byte, types.PeerID) error
}

func (c *countingTransport) SendRawFrame(_ types.PeerID, _ net_types.MessageType, data []byte) error {
	c.bytes.Add(int64(len(data)))
	return nil
}

func (c *countingTransport) RegisterFastFrameHandler(_ net_types.MessageType, h func([]byte, types.PeerID) error) {
	c.h = h
}

func peerN(i int) types.PeerID {
	var id types.PeerID
	id[0], id[1], id[2] = byte(i), byte(i>>8), byte(i>>16)
	return id
}

func TestFreshPeerIDsShareOneNodeWideBudget(t *testing.T) {
	p := New(&countingTransport{})
	now := time.Now()
	p.refilled = now

	admitted := 0
	for i := 0; i < 100; i++ {
		p.mu.Lock()
		if p.admitLocked(peerN(i), "", now) {
			admitted++
			p.serving-- // as if the burst finished, having sent everything
		}
		p.mu.Unlock()
	}
	assert.Equal(t, 4, admitted, "16 MB a minute is four 4 MB bursts, however many peer IDs ask")

	p.mu.Lock()
	defer p.mu.Unlock()
	assert.False(t, p.admitLocked(peerN(1000), "", now.Add(10*time.Second)), "10 s refills less than a burst")
	assert.True(t, p.admitLocked(peerN(1001), "", now.Add(16*time.Second)), "15 s refills one burst")
}

func TestPerPeerIntervalAndConcurrencyHold(t *testing.T) {
	p := New(&countingTransport{})
	p.SetLimits(Limits{Serve: true, PerPeerEvery: 5 * time.Second, MaxConcurrent: 2, BytesPerMinute: 100 * serveBurstBytes})
	p.tokens = 100 * serveBurstBytes
	now := time.Now()
	p.refilled = now

	p.mu.Lock()
	defer p.mu.Unlock()
	require.True(t, p.admitLocked(peerN(1), "", now))
	assert.False(t, p.admitLocked(peerN(1), "", now.Add(time.Second)), "same peer inside its interval")
	require.True(t, p.admitLocked(peerN(2), "", now))
	assert.False(t, p.admitLocked(peerN(3), "", now), "two bursts in flight already")
	p.serving = 0
	assert.True(t, p.admitLocked(peerN(1), "", now.Add(6*time.Second)))
}

func TestPeerTableStaysBounded(t *testing.T) {
	p := New(&countingTransport{})
	p.SetLimits(Limits{Serve: true, PerPeerEvery: 5 * time.Second, MaxConcurrent: 1 << 30, BytesPerMinute: 1 << 62})
	p.tokens = 1 << 62
	now := time.Now()
	p.refilled = now

	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < maxTrackedPeers+500; i++ {
		p.admitLocked(peerN(i), "", now)
	}
	assert.LessOrEqual(t, len(p.lastReq), maxTrackedPeers)
	// once the old entries are past their interval they make room again
	assert.True(t, p.admitLocked(peerN(1<<20), "", now.Add(6*time.Second)))
	assert.LessOrEqual(t, len(p.lastReq), maxTrackedPeers)
}

func TestServeOffSendsNothing(t *testing.T) {
	tr := &countingTransport{}
	p := New(tr)
	l := p.Limits()
	l.Serve = false
	p.SetLimits(l)

	req := make([]byte, headerSize)
	req[0] = kindReq
	require.NoError(t, tr.h(req, peerN(7)))
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, tr.bytes.Load())

	// measuring others still works with serving off
	_, err := p.Measure(ctxShort(t), peerN(8))
	assert.Error(t, err) // nobody acks on this transport, but the burst went out
	assert.Greater(t, tr.bytes.Load(), int64(maxBurstBytes/2))
}

func TestEndMarkerFloodIsBounded(t *testing.T) {
	tr := &countingTransport{}
	p := New(tr)
	for i := 0; i < 1000; i++ {
		end := make([]byte, headerSize)
		end[0] = kindEnd
		end[1], end[2] = byte(i), byte(i>>8)
		require.NoError(t, tr.h(end, peerN(i)))
	}
	p.mu.Lock()
	n := p.finishing
	p.mu.Unlock()
	assert.LessOrEqual(t, n, maxFinishing)
}

func ctxShort(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func reqFrame(i int) []byte {
	req := make([]byte, headerSize)
	req[0] = kindReq
	req[1] = byte(i)
	return req
}

// A 9-byte request buys a burst only for a peer the gate lets in (an exit's recent client), at most
// once per interval for its address too, and never more than serveBurstBytes.
func TestServeOnlyThroughGateCappedAndPerAddress(t *testing.T) {
	tr := &countingTransport{}
	p := New(tr)

	require.NoError(t, tr.h(reqFrame(1), peerN(1)))
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, tr.bytes.Load(), "no gate set: nobody gets a burst")

	clients := map[types.PeerID]string{peerN(1): "198.51.100.1", peerN(2): "198.51.100.1", peerN(3): "198.51.100.3"}
	p.SetGate(func(id types.PeerID) (string, bool) {
		addr, ok := clients[id]
		return addr, ok
	})
	require.NoError(t, tr.h(reqFrame(2), peerN(9)))
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, tr.bytes.Load(), "a peer the gate does not know gets nothing")

	require.NoError(t, tr.h(reqFrame(3), peerN(1)))
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.serving == 0 && tr.bytes.Load() > 0
	}, 5*time.Second, 10*time.Millisecond)
	one := tr.bytes.Load()
	assert.LessOrEqual(t, one, int64(serveBurstBytes+frameSize+headerSize), "the answer is capped")

	require.NoError(t, tr.h(reqFrame(4), peerN(2)))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, one, tr.bytes.Load(), "another ID from the same address waits its interval")

	require.NoError(t, tr.h(reqFrame(5), peerN(3)))
	require.Eventually(t, func() bool { return tr.bytes.Load() > one }, 5*time.Second, 10*time.Millisecond,
		"a client at another address is served")
}
